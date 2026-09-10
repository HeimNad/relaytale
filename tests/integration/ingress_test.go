package integration

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
	"github.com/google/uuid"
	"mailgateway/internal/auth"
	"mailgateway/internal/database"
	"mailgateway/internal/devtls"
	"mailgateway/internal/message"
	"mailgateway/internal/smtpserver"
	"mailgateway/internal/storage"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db
}
func account(t *testing.T, db *sql.DB) (string, string, string) {
	t.Helper()
	id := uuid.NewString()
	name := "test-" + id
	password := "integration-only-test-password"
	hash, err := auth.Hash(password)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(context.Background(), `INSERT INTO smtp_accounts(id,username,password_hash,allowed_from) VALUES($1,$2,$3,ARRAY['sender@example.test'])`, id, name, hash)
	if err != nil {
		t.Fatal(err)
	}
	return id, name, password
}

type fixture struct {
	addr   string
	tls    *tls.Config
	server *smtpserver.Server
}

func serve(t *testing.T, db *sql.DB, receiver smtpserver.Receiver, max int64) fixture {
	t.Helper()
	dir := t.TempDir()
	if err := devtls.Generate(dir); err != nil {
		t.Fatal(err)
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "smtp.crt"), filepath.Join(dir, "smtp.key"))
	if err != nil {
		t.Fatal(err)
	}
	pem, err := os.ReadFile(filepath.Join(dir, "smtp.crt"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		t.Fatal("invalid cert")
	}
	accounts, err := auth.NewAccounts(db)
	if err != nil {
		t.Fatal(err)
	}
	s := smtpserver.New(&smtpserver.Backend{Accounts: accounts, Receiver: receiver, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Timeout: 5 * time.Second}, "localhost", cert, max)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve(listener) }()
	t.Cleanup(func() { _ = s.Close(); <-done })
	return fixture{listener.Addr().String(), &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12}, s}
}
func (f fixture) client(t *testing.T, secure bool) *smtp.Client {
	t.Helper()
	conn, err := net.DialTimeout("tcp", f.addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	var c *smtp.Client
	if secure {
		c, err = smtp.NewClientStartTLS(conn, f.tls)
	} else {
		c = smtp.NewClient(conn)
	}
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}
func code(t *testing.T, err error, want int) {
	t.Helper()
	var se *smtp.SMTPError
	if !errors.As(err, &se) || se.Code != want {
		t.Fatalf("want SMTP %d got %v", want, err)
	}
}
func send(c *smtp.Client, raw string) error {
	if err := c.Mail("sender@example.test", nil); err != nil {
		return err
	}
	for _, r := range []string{"one@example.test", "two@example.test"} {
		if err := c.Rcpt(r, nil); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, raw); err != nil {
		return err
	}
	return w.Close()
}

const raw = "From: Sender <sender@example.test>\r\nTo: one@example.test\r\nMessage-ID: <preserved@example.test>\r\nSubject: Integration\r\nDKIM-Signature: v=1; b=unchanged-test-value\r\nContent-Type: text/plain\r\n\r\n.dot-stuffed line\r\nbody\r\n"

func TestSMTPIngress(t *testing.T) {
	db := testDB(t)
	id, username, password := account(t, db)
	root := t.TempDir()
	f := serve(t, db, message.Receiver{Store: storage.LocalStore{Root: root, MaxBytes: 1024}, Repo: message.Postgres{DB: db}}, 1024)
	t.Run("anonymous and plaintext denied", func(t *testing.T) {
		c := f.client(t, false)
		code(t, c.Mail("sender@example.test", nil), 530)
		if err := c.Auth(sasl.NewPlainClient("", username, password)); err == nil {
			t.Fatal("plaintext auth accepted")
		}
	})
	t.Run("wrong password", func(t *testing.T) {
		c := f.client(t, true)
		code(t, c.Auth(sasl.NewPlainClient("", username, "wrong")), 535)
	})
	t.Run("restricted envelope sender", func(t *testing.T) {
		c := f.client(t, true)
		if err := c.Auth(sasl.NewPlainClient("", username, password)); err != nil {
			t.Fatal(err)
		}
		code(t, c.Mail("spoof@example.test", nil), 550)
	})
	t.Run("restricted header sender", func(t *testing.T) {
		c := f.client(t, true)
		if err := c.Auth(sasl.NewPlainClient("", username, password)); err != nil {
			t.Fatal(err)
		}
		code(t, send(c, strings.Replace(raw, "Sender <sender@example.test>", "spoof@example.test", 1)), 550)
	})
	t.Run("durable plain and login", func(t *testing.T) {
		for _, a := range []sasl.Client{sasl.NewPlainClient("", username, password), sasl.NewLoginClient(username, password)} {
			c := f.client(t, true)
			if err := c.Auth(a); err != nil {
				t.Fatal(err)
			}
			if err := send(c, raw); err != nil {
				t.Fatal(err)
			}
			var mid, path, status, hash string
			var size int64
			err := db.QueryRowContext(context.Background(), `SELECT id,eml_path,status,eml_sha256,eml_size FROM messages WHERE smtp_account_id=$1 ORDER BY created_at DESC LIMIT 1`, id).Scan(&mid, &path, &status, &hash, &size)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != raw || status != "QUEUED" || hash != fmt.Sprintf("%x", sha256.Sum256(data)) || size != int64(len(raw)) {
				t.Fatal("acknowledged data is not durably intact")
			}
			for table, want := range map[string]int{"recipients": 2, "events": 3} {
				var n int
				if err := db.QueryRowContext(context.Background(), "SELECT count(*) FROM "+table+" WHERE message_id=$1", mid).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n != want {
					t.Fatalf("%s: want %d got %d", table, want, n)
				}
			}
			if _, err := db.ExecContext(context.Background(), `UPDATE events SET source='changed' WHERE message_id=$1`, mid); err == nil {
				t.Fatal("ledger mutable")
			}
			if err := c.Quit(); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("oversize rejected", func(t *testing.T) {
		c := f.client(t, true)
		if err := c.Auth(sasl.NewPlainClient("", username, password)); err != nil {
			t.Fatal(err)
		}
		code(t, send(c, raw+strings.Repeat("x\r\n", 800)+"\r\n"), 552)
	})
	t.Run("overlong protocol line rejected", func(t *testing.T) {
		c := f.client(t, true)
		if err := c.Auth(sasl.NewPlainClient("", username, password)); err != nil {
			t.Fatal(err)
		}
		code(t, send(c, raw+strings.Repeat("x", 2048)+"\r\n"), 554)
	})
	t.Run("disabled account denied", func(t *testing.T) {
		if _, err := db.ExecContext(context.Background(), `UPDATE smtp_accounts SET enabled=false WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		c := f.client(t, true)
		code(t, c.Auth(sasl.NewPlainClient("", username, password)), 535)
	})
}

type failingRepo struct{}

func (failingRepo) Enqueue(context.Context, message.Submission) error {
	return errors.New("injected commit failure")
}
func TestPersistenceFailures(t *testing.T) {
	db := testDB(t)
	_, username, password := account(t, db)
	for _, kind := range []string{"disk", "database"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			var repo message.Repository = message.Postgres{DB: db}
			if kind == "disk" {
				root = filepath.Join(root, "not-directory")
				if err := os.WriteFile(root, []byte("file"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				repo = failingRepo{}
			}
			f := serve(t, db, message.Receiver{Store: storage.LocalStore{Root: root, MaxBytes: 1024}, Repo: repo}, 1024)
			c := f.client(t, true)
			if err := c.Auth(sasl.NewPlainClient("", username, password)); err != nil {
				t.Fatal(err)
			}
			code(t, send(c, raw), 451)
		})
	}
}

func TestDatabaseTransactionRollback(t *testing.T) {
	db := testDB(t)
	id, username, password := account(t, db)
	// Only the isolated test database receives this failure injection.
	_, err := db.ExecContext(context.Background(), `CREATE OR REPLACE FUNCTION fail_test_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected event write failure'; END; $$;
 CREATE TRIGGER fail_test_event BEFORE INSERT ON events FOR EACH ROW EXECUTE FUNCTION fail_test_event();`)
	if err != nil {
		t.Fatal(err)
	}
	defer db.ExecContext(context.Background(), `DROP TRIGGER fail_test_event ON events; DROP FUNCTION fail_test_event();`)
	f := serve(t, db, message.Receiver{Store: storage.LocalStore{Root: t.TempDir(), MaxBytes: 1024}, Repo: message.Postgres{DB: db}}, 1024)
	c := f.client(t, true)
	if err := c.Auth(sasl.NewPlainClient("", username, password)); err != nil {
		t.Fatal(err)
	}
	code(t, send(c, raw), 451)
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM messages WHERE smtp_account_id=$1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("message committed despite event failure")
	}
}

type observedReceiver struct {
	inner message.Receiver
	done  chan error
}

func (r observedReceiver) Receive(ctx context.Context, e message.Envelope, reader io.Reader) (string, error) {
	id, err := r.inner.Receive(ctx, e, reader)
	r.done <- err
	return id, err
}
func TestInterruptedDATA(t *testing.T) {
	db := testDB(t)
	id, username, password := account(t, db)
	root := t.TempDir()
	done := make(chan error, 1)
	f := serve(t, db, observedReceiver{inner: message.Receiver{Store: storage.LocalStore{Root: root, MaxBytes: 20000}, Repo: message.Postgres{DB: db}}, done: done}, 20000)
	c := f.client(t, true)
	if err := c.Auth(sasl.NewPlainClient("", username, password)); err != nil {
		t.Fatal(err)
	}
	if err := c.Mail("sender@example.test", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Rcpt("one@example.test", nil); err != nil {
		t.Fatal(err)
	}
	w, err := c.Data()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, raw+strings.Repeat("unfinished\r\n", 1000)); err != nil {
		t.Fatal(err)
	}
	// Deliberately omit the terminating dot and close the transport.
	_ = c.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("incomplete upload accepted")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("receiver did not finish")
	}
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM messages WHERE smtp_account_id=$1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("incomplete upload enqueued")
	}
	_ = filepath.Walk(root, func(p string, i os.FileInfo, err error) error {
		if err == nil && !i.IsDir() {
			t.Errorf("partial file leaked: %s", p)
		}
		return err
	})
}

type gatedRepo struct {
	inner   message.Postgres
	entered chan struct{}
	release chan struct{}
}

func (r gatedRepo) Enqueue(ctx context.Context, s message.Submission) error {
	close(r.entered)
	select {
	case <-r.release:
		return r.inner.Enqueue(ctx, s)
	case <-ctx.Done():
		return ctx.Err()
	}
}
func TestNoAcknowledgementBeforeCommitAndGracefulShutdown(t *testing.T) {
	db := testDB(t)
	_, username, password := account(t, db)
	entered := make(chan struct{})
	release := make(chan struct{})
	f := serve(t, db, message.Receiver{Store: storage.LocalStore{Root: t.TempDir(), MaxBytes: 1024}, Repo: gatedRepo{message.Postgres{DB: db}, entered, release}}, 1024)
	c := f.client(t, true)
	if err := c.Auth(sasl.NewPlainClient("", username, password)); err != nil {
		t.Fatal(err)
	}
	sent := make(chan error, 1)
	go func() {
		err := send(c, raw)
		if err == nil {
			err = c.Quit()
		}
		sent <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("transaction not reached")
	}
	select {
	case err := <-sent:
		t.Fatalf("SMTP completed before commit: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	stopped := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { stopped <- f.server.Shutdown(ctx) }()
	close(release)
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
}

func TestForceCloseAfterShutdownDeadline(t *testing.T) {
	db := testDB(t)
	f := serve(t, db, message.Receiver{Store: storage.LocalStore{Root: t.TempDir(), MaxBytes: 1024}, Repo: message.Postgres{DB: db}}, 1024)
	c := f.client(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := f.server.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected stalled connection shutdown timeout: %v", err)
	}
	if err := f.server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Noop(); err == nil {
		t.Fatal("connection remains open after forced close")
	}
}
