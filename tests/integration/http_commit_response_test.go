package integration

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"relaytale/internal/api"
	"relaytale/internal/message"
	"relaytale/internal/testsmtp"
)

// commitResponseConn forwards real PostgreSQL protocol frames unchanged until
// the server reports COMMIT success. It drops that frame before pgx sees it.
// This is test-only plaintext transport; no production connection is wrapped.
type commitResponseConn struct {
	net.Conn
	armed   *atomic.Bool
	dropped *atomic.Bool
	pending []byte
}

func (c *commitResponseConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(c.pending) == 0 {
		header := make([]byte, 5)
		if _, err := io.ReadFull(c.Conn, header); err != nil {
			return 0, err
		}
		length := int(binary.BigEndian.Uint32(header[1:]))
		if length < 4 || length > 16<<20 {
			return 0, fmt.Errorf("invalid test protocol frame length %d", length)
		}
		body := make([]byte, length-4)
		if _, err := io.ReadFull(c.Conn, body); err != nil {
			return 0, err
		}
		if header[0] == 'C' && string(body) == "COMMIT\x00" && c.armed.CompareAndSwap(true, false) {
			c.dropped.Store(true)
			c.Conn.Close()
			return 0, io.ErrUnexpectedEOF
		}
		c.pending = append(header, body...)
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func TestHTTPSubmissionDatabaseCommitResponseLost(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, receiver := sendFixture(t, f)
	config, err := pgx.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	// Only the isolated integration database is used; wrapping TLS ciphertext
	// would not exercise the PostgreSQL frame boundary.
	config.TLSConfig = nil
	config.Fallbacks = nil
	var armed, dropped atomic.Bool
	config.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &commitResponseConn{Conn: conn, armed: &armed, dropped: &dropped}, nil
	}
	db := stdlib.OpenDB(*config)
	defer db.Close()
	db.SetMaxOpenConns(10)
	receiver.Repo = message.Postgres{DB: db}
	raw, _ := json.Marshal([]api.SendKey{key})
	handler, err := api.NewSendHandler(context.Background(), string(raw), receiver)
	if err != nil {
		t.Fatal(err)
	}
	// Startup credential registration has committed before the fault is armed.
	armed.Store(true)
	requireStatus(t, sendRequest(handler, key, "lost-db-commit", f.raw(), nil), 503)
	if !dropped.Load() {
		t.Fatal("COMMIT success frame was not intercepted")
	}
	// Observe durable truth using an independent, unmodified connection pool.
	var original string
	if err = f.db.QueryRow(`SELECT message_id FROM api_submission_ledger WHERE api_key_id=$1`, key.ID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	assertCount(t, f, 1, `SELECT count(*) FROM messages WHERE api_key_id=$1`, key.ID)
	if emlCount(t, f.root) != 1 {
		t.Fatal("committed archive lost")
	}
	assertHTTPResourcesReleased(t, receiver)
	retry := sendRequest(handler, key, "lost-db-commit", f.raw(), nil)
	requireStatus(t, retry, 202)
	var result message.EnqueueResult
	if err = json.Unmarshal(retry.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Replayed || result.MessageID != original {
		t.Fatal(result, original)
	}
	if emlCount(t, f.root) != 1 {
		t.Fatal("retry published duplicate archive")
	}
	runOne(t, f.worker)
	if f.status(t, original) != "SMTP_ACCEPTED" {
		t.Fatal("recovered message did not send")
	}
	assertCount(t, f, 1, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1`, original)
}
