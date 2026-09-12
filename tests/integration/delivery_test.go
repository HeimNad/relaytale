package integration

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/google/uuid"
	"relaytale/internal/auth"
	"relaytale/internal/encryption"
	"relaytale/internal/message"
	"relaytale/internal/provider"
	"relaytale/internal/queue"
	"relaytale/internal/smtpclient"
	"relaytale/internal/storage"
	"relaytale/internal/testsmtp"
)

type deliveryFixture struct {
	db                                          *sql.DB
	box                                         *encryption.Box
	root, domain, accountID, username, password string
	fake                                        *testsmtp.Server
	worker                                      queue.Worker
}

func delivery(t *testing.T, o testsmtp.Options, connections int) *deliveryFixture {
	t.Helper()
	db := testDB(t)
	id, user, password := account(t, db)
	box, err := encryption.New(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	domain := uuid.NewString() + ".example.test"
	root := t.TempDir()
	fake := testsmtp.Start(t, o)
	f := &deliveryFixture{db: db, box: box, root: root, domain: domain, accountID: id, username: user, password: password, fake: fake}
	f.addProvider(t, fake, 10, connections)
	_, err = db.ExecContext(context.Background(), `UPDATE smtp_accounts SET allowed_from=ARRAY[$2] WHERE id=$1`, id, "sender@"+domain)
	if err != nil {
		t.Fatal(err)
	}
	f.worker = queue.Worker{Repo: queue.Repository{DB: db}, Box: box, Sender: smtpclient.Client{Domain: "relaytale.test", RootCAs: fake.Roots}, StorageRoot: root, MaxBytes: 1024 * 1024, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return f
}
func (f *deliveryFixture) addProvider(t *testing.T, fake *testsmtp.Server, priority, connections int) string {
	t.Helper()
	host, port, _ := net.SplitHostPort(fake.Addr)
	number, _ := strconv.Atoi(port)
	id, err := provider.Create(context.Background(), f.db, f.box, provider.Provider{Name: uuid.NewString(), Host: host, Port: number, Security: "starttls", Username: "provider-user", Priority: priority, MaxConnections: connections, Timeout: 3 * time.Second}, "provider-password", []string{f.domain})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.db.ExecContext(context.Background(), `UPDATE providers SET enabled=false WHERE id=$1`, id)
	})
	return id
}
func (f *deliveryFixture) raw() string {
	return strings.ReplaceAll(raw, "sender@example.test", "sender@"+f.domain)
}
func (f *deliveryFixture) receiver() message.Receiver {
	return message.Receiver{Store: storage.LocalStore{Root: f.root, MaxBytes: 1024 * 1024}, Repo: message.Postgres{DB: f.db}}
}
func (f *deliveryFixture) seed(t *testing.T) string {
	t.Helper()
	id, err := f.receiver().Receive(context.Background(), message.Envelope{Account: auth.Account{ID: f.accountID, AllowedFrom: []string{"sender@" + f.domain}}, From: "sender@" + f.domain, Recipients: []string{"one@example.test", "two@example.test"}}, strings.NewReader(f.raw()))
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func (f *deliveryFixture) status(t *testing.T, id string) string {
	t.Helper()
	var status string
	if err := f.db.QueryRowContext(context.Background(), `SELECT status FROM messages WHERE id=$1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}
func (f *deliveryFixture) expire(t *testing.T, id string) {
	t.Helper()
	if _, err := f.db.ExecContext(context.Background(), `UPDATE messages SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
}
func runOne(t *testing.T, w queue.Worker) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	worked, err := w.RunOne(ctx)
	if err != nil || !worked {
		t.Fatalf("worker: worked=%v error=%v", worked, err)
	}
}

func TestIngressToProvider(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	ingress := serve(t, f.db, f.receiver(), 1024*1024)
	c := ingress.client(t, true)
	if err := c.Auth(sasl.NewPlainClient("", f.username, f.password)); err != nil {
		t.Fatal(err)
	}
	if err := c.Mail("sender@"+f.domain, nil); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"one@example.test", "two@example.test"} {
		if err := c.Rcpt(address, nil); err != nil {
			t.Fatal(err)
		}
	}
	data, err := c.Data()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(data, f.raw()); err != nil {
		t.Fatal(err)
	}
	if err := data.Close(); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := f.db.QueryRowContext(context.Background(), `SELECT id FROM messages WHERE smtp_account_id=$1`, f.accountID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if f.status(t, id) != "QUEUED" {
		t.Fatal("ingress bypassed queue")
	}
	runOne(t, f.worker)
	if f.status(t, id) != smtpclient.Accepted {
		t.Fatal("provider acceptance not recorded")
	}
	select {
	case got := <-f.fake.Payloads:
		if string(got) != f.raw() {
			t.Fatal("payload modified between ingress and provider")
		}
	default:
		t.Fatal("provider did not receive payload")
	}
	var n int
	for _, q := range []string{`SELECT count(*) FROM delivery_attempts WHERE message_id=$1 AND result='SMTP_ACCEPTED' AND final_response_at IS NOT NULL AND data_armed_at IS NOT NULL`, `SELECT count(*) FROM events WHERE message_id=$1 AND event_type='SMTP_ACCEPTED'`} {
		if err := f.db.QueryRowContext(context.Background(), q, id).Scan(&n); err != nil || n != 1 {
			t.Fatalf("missing attempt or event: %d %v", n, err)
		}
	}
	worked, err := f.worker.RunOne(context.Background())
	if err != nil || worked {
		t.Fatal("accepted message claimed again")
	}
}
func TestDeliveryOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    testsmtp.Options
		want string
	}{
		{"partial recipients", testsmtp.Options{RejectRecipients: map[string]int{"two@example.test": 550}}, smtpclient.Partial},
		{"temporary final rejection", testsmtp.Options{FinalCode: 451}, smtpclient.Temporary},
		{"permanent final rejection", testsmtp.Options{FinalCode: 550}, smtpclient.Permanent},
		{"unknown final", testsmtp.Options{DropFinal: true}, smtpclient.Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := delivery(t, tc.o, 1)
			id := f.seed(t)
			runOne(t, f.worker)
			if got := f.status(t, id); got != tc.want {
				t.Fatalf("want %s got %s", tc.want, got)
			}
			worked, err := f.worker.RunOne(context.Background())
			if err != nil || worked {
				t.Fatal("terminal/held message automatically retried")
			}
			var n int
			if err := f.db.QueryRowContext(context.Background(), `SELECT count(*) FROM attempt_recipients ar JOIN delivery_attempts a ON a.id=ar.attempt_id WHERE a.message_id=$1`, id).Scan(&n); err != nil || n != 2 {
				t.Fatal("missing recipient attempt results")
			}
			if tc.want == smtpclient.Partial {
				var status string
				if err := f.db.QueryRowContext(context.Background(), `SELECT status FROM recipients WHERE message_id=$1 AND address='two@example.test'`, id).Scan(&status); err != nil || status != smtpclient.Permanent {
					t.Fatal("rejected recipient lost")
				}
			}
		})
	}
}
func TestClaimFencingAndRecovery(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	id := f.seed(t)
	results := make(chan queue.Job, 8)
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, err := f.worker.Repo.Claim(context.Background())
			if err != nil {
				errs <- err
			} else {
				results <- j
			}
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	var first queue.Job
	count := 0
	for j := range results {
		first = j
		count++
	}
	if count != 1 {
		t.Fatalf("concurrent claims: %d", count)
	}
	for err := range errs {
		if !errors.Is(err, queue.ErrNoJob) {
			t.Fatal(err)
		}
	}
	f.expire(t, id)
	if err := f.worker.Repo.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.status(t, id) != "QUEUED" {
		t.Fatal("pre-DATA claim not recovered")
	}
	second, err := f.worker.Repo.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Token == second.Token {
		t.Fatal("claim token reused")
	}
	if err := f.worker.Repo.ArmData(context.Background(), first); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatal("stale worker allowed to transmit")
	}
	if err := f.worker.Repo.ArmData(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	f.expire(t, id)
	if err := f.worker.Repo.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.status(t, id) != smtpclient.Unknown {
		t.Fatal("armed expired claim automatically retried")
	}
	if _, err := f.worker.Repo.Claim(context.Background()); !errors.Is(err, queue.ErrNoJob) {
		t.Fatal("unknown message reclaimed")
	}
	if err := f.worker.Repo.Finish(context.Background(), second, smtpclient.Result{}); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatal("stale completion overwrote recovery")
	}
}
func TestProviderConcurrencyLimit(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	f.seed(t)
	f.seed(t)
	j, err := f.worker.Repo.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.worker.Repo.Claim(context.Background()); !errors.Is(err, queue.ErrNoJob) {
		t.Fatal("provider connection limit exceeded")
	}
	// Hold both fixture messages so later tests cannot pick them up.
	if err := f.worker.Repo.ArmData(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	f.expire(t, j.ID)
	if err := f.worker.Repo.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestIntegrityAndCredentialFailuresDoNotConnect(t *testing.T) {
	for _, kind := range []string{"archive", "key"} {
		t.Run(kind, func(t *testing.T) {
			f := delivery(t, testsmtp.Options{}, 1)
			id := f.seed(t)
			if kind == "archive" {
				var path string
				if err := f.db.QueryRowContext(context.Background(), `SELECT eml_path FROM messages WHERE id=$1`, id).Scan(&path); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("tampered"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				var err error
				f.worker.Box, err = encryption.New(strings.Repeat("cd", 32))
				if err != nil {
					t.Fatal(err)
				}
			}
			runOne(t, f.worker)
			if f.status(t, id) != smtpclient.Temporary || f.fake.Connections.Load() != 0 {
				t.Fatal("unsafe delivery despite failed preparation")
			}
		})
	}
}
func TestLostCompletionCommitDoesNotResend(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	id := f.seed(t)
	_, err := f.db.ExecContext(context.Background(), `CREATE OR REPLACE FUNCTION fail_delivery_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event_type='SMTP_ACCEPTED' THEN RAISE EXCEPTION 'injected completion failure'; END IF; RETURN NEW; END; $$; CREATE TRIGGER fail_delivery_event BEFORE INSERT ON events FOR EACH ROW EXECUTE FUNCTION fail_delivery_event();`)
	if err != nil {
		t.Fatal(err)
	}
	defer f.db.ExecContext(context.Background(), `DROP TRIGGER fail_delivery_event ON events; DROP FUNCTION fail_delivery_event();`)
	worked, err := f.worker.RunOne(context.Background())
	if !worked || err == nil {
		t.Fatal("completion failure not reported")
	}
	if f.status(t, id) != "SENDING" {
		t.Fatal("partial completion transaction leaked")
	}
	select {
	case <-f.fake.Payloads:
	default:
		t.Fatal("provider did not accept before injected commit failure")
	}
	f.expire(t, id)
	if err := f.worker.Repo.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.status(t, id) != smtpclient.Unknown {
		t.Fatal("lost commit did not become unknown")
	}
	if worked, err := f.worker.RunOne(context.Background()); worked || err != nil {
		t.Fatal("duplicate delivery after lost commit")
	}
}
func TestNoAutomaticProviderFailover(t *testing.T) {
	f := delivery(t, testsmtp.Options{RejectAuth: true}, 1)
	backup := testsmtp.Start(t, testsmtp.Options{})
	f.addProvider(t, backup, 20, 1)
	id := f.seed(t)
	runOne(t, f.worker)
	if f.status(t, id) != smtpclient.Temporary || backup.Connections.Load() != 0 {
		t.Fatal("unexpected automatic failover")
	}
}
func TestWorkerPoolStopsAndSendsOnce(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 4)
	const total = 8
	for i := 0; i < total; i++ {
		f.seed(t)
	}
	claims, stopClaims := context.WithCancel(context.Background())
	defer stopClaims()
	operations, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); f.worker.Run(claims, operations, 4) }()
	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()
	for {
		var n int
		err := f.db.QueryRowContext(operations, `SELECT count(*) FROM messages WHERE smtp_account_id=$1 AND status='SMTP_ACCEPTED'`, f.accountID).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n == total {
			break
		}
		select {
		case <-operations.Done():
			t.Fatal("workers did not finish")
		case <-ticker.C:
		}
	}
	stopClaims()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker pool did not stop")
	}
	if f.fake.Connections.Load() != total {
		t.Fatalf("expected %d submissions, got %d", total, f.fake.Connections.Load())
	}
}
