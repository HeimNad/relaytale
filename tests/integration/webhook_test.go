package integration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"relaytale/internal/testsmtp"
	"relaytale/internal/webhook"
)

type webhookSender func(context.Context, webhook.Endpoint, []byte) (int, error)

func (f webhookSender) Send(c context.Context, e webhook.Endpoint, b []byte) (int, error) {
	return f(c, e, b)
}
func TestWebhookTransactionalIsolationAndRetry(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sendFixture(t, f)
	endpoint := webhook.Endpoint{ID: uuid.NewString(), APIKeyID: key.ID, URL: "https://example.com/hook", Keys: []webhook.Key{{ID: "v1", Secret: strings.Repeat("ab", 32)}}}
	ctx := context.Background()
	if err := webhook.Sync(ctx, f.db, []webhook.Endpoint{endpoint}); err != nil {
		t.Fatal(err)
	}
	id := submitStatusFixture(t, h, key, f.raw(), nil)
	assertCount(t, f, 1, `SELECT count(*) FROM webhook_outbox WHERE message_id=$1`, id)
	other, otherKey, _ := sendFixture(t, f)
	otherID := submitStatusFixture(t, other, otherKey, f.raw(), nil)
	assertCount(t, f, 0, `SELECT count(*) FROM webhook_outbox WHERE message_id=$1`, otherID)
	smtpID := f.seed(t)
	assertCount(t, f, 0, `SELECT count(*) FROM webhook_outbox WHERE message_id=$1`, smtpID)
	var sequence int64
	var original string
	if err := f.db.QueryRow(`SELECT sequence,payload FROM webhook_outbox WHERE message_id=$1`, id).Scan(&sequence, &original); err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(original), &event); err != nil || event["message_id"] != id || event["feedback_availability"] != "NOT_EXPOSED" {
		t.Fatal(event, err)
	}
	// Event insertion and outbox insertion roll back together.
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	eid := uuid.NewString()
	if _, err = tx.Exec(`INSERT INTO events(id,message_id,event_type,source,data) VALUES($1,$2,'DELIVERY_UNKNOWN','worker','{"secret":"PRIVATE"}')`, eid, id); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	assertCount(t, f, 0, `SELECT count(*) FROM webhook_outbox WHERE event_id=$1`, eid)
	// Callback failure cannot mutate the mail queue or create SMTP attempts.
	calls := 0
	worker := webhook.Worker{DB: f.db, Sender: webhookSender(func(_ context.Context, _ webhook.Endpoint, b []byte) (int, error) {
		calls++
		if string(b) != original {
			t.Fatal("payload changed")
		}
		return 503, nil
	})}
	if ok, err := worker.RunOne(ctx, endpoint); err != nil || !ok {
		t.Fatal(ok, err)
	}
	assertCount(t, f, 1, `SELECT count(*) FROM webhook_outbox WHERE sequence=$1 AND state='PENDING' AND attempts=1 AND next_at>clock_timestamp()`, sequence)
	if ok, err := worker.RunOne(ctx, endpoint); err != nil || ok {
		t.Fatal("early retry", ok, err)
	}
	assertCount(t, f, 0, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1`, id)
	if _, err = f.db.Exec(`UPDATE webhook_outbox SET payload='{}' WHERE sequence=$1`, sequence); err == nil {
		t.Fatal("mutable payload")
	}
	if _, err = f.db.Exec(`DELETE FROM webhook_delivery_log WHERE sequence=$1`, sequence); err == nil {
		t.Fatal("mutable audit")
	}
	mustExec(t, f, `UPDATE webhook_outbox SET next_at=clock_timestamp()-interval '1 second' WHERE sequence=$1`, sequence)
	worker.Sender = webhookSender(func(_ context.Context, _ webhook.Endpoint, b []byte) (int, error) {
		calls++
		if string(b) != original {
			t.Fatal("retry bytes changed")
		}
		return 204, nil
	})
	if _, err = worker.RunOne(ctx, endpoint); err != nil {
		t.Fatal(err)
	}
	assertCount(t, f, 1, `SELECT count(*) FROM webhook_outbox WHERE sequence=$1 AND state='DELIVERED' AND attempts=2`, sequence)
	if calls != 2 {
		t.Fatal(calls)
	}
	// Permanent mail facts are captured in the original finish transaction.
	// Each identity gets a turn; the subscribed message need not be first.
	for range 3 {
		runOne(t, f.worker)
	}
	assertCount(t, f, 1, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1`, id)
	assertCount(t, f, 0, `SELECT count(*) FROM webhook_outbox WHERE message_id IN ($1,$2)`, otherID, smtpID)
	var n int
	if err = f.db.QueryRow(`SELECT count(*) FROM webhook_outbox WHERE message_id=$1`, id).Scan(&n); err != nil || n < 2 {
		t.Fatal(n, err)
	}
}

func TestWebhookLeaseReplayRotationAndPause(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sendFixture(t, f)
	ctx := context.Background()
	e := webhook.Endpoint{ID: uuid.NewString(), APIKeyID: key.ID, URL: "https://example.com/hook", Keys: []webhook.Key{{ID: "old", Secret: strings.Repeat("ab", 32)}}}
	if err := webhook.Sync(ctx, f.db, []webhook.Endpoint{e}); err != nil {
		t.Fatal(err)
	}
	id := submitStatusFixture(t, h, key, f.raw(), nil)
	var seq int64
	var payload string
	if err := f.db.QueryRow(`SELECT sequence,payload FROM webhook_outbox WHERE message_id=$1`, id).Scan(&seq, &payload); err != nil {
		t.Fatal(err)
	}
	// Simulated crash after side effect, before final database acknowledgement.
	mustExec(t, f, `UPDATE webhook_outbox SET state='IN_FLIGHT',attempts=1,lease_token=$2,lease_until=clock_timestamp()-interval '1 second' WHERE sequence=$1`, seq, uuid.NewString())
	calls := 0
	worker := webhook.Worker{DB: f.db, Sender: webhookSender(func(_ context.Context, _ webhook.Endpoint, b []byte) (int, error) {
		calls++
		if string(b) != payload {
			t.Fatal("recovery changed bytes")
		}
		return 0, errors.New("response lost")
	})}
	if ok, err := worker.RunOne(ctx, e); err != nil || !ok {
		t.Fatal(ok, err)
	}
	mustExec(t, f, `UPDATE webhook_outbox SET attempts=8,next_at=clock_timestamp()-interval '1 second' WHERE sequence=$1`, seq)
	if ok, err := worker.RunOne(ctx, e); err != nil || ok {
		t.Fatal(ok, err)
	}
	assertCount(t, f, 1, `SELECT count(*) FROM webhook_outbox WHERE sequence=$1 AND state='DEAD'`, seq)
	if err := webhook.Replay(ctx, f.db, seq, 0, "operator", "test replay"); err != nil {
		t.Fatal(err)
	}
	if err := webhook.Replay(ctx, f.db, seq, 0, "operator", "duplicate"); err == nil {
		t.Fatal("stale replay accepted")
	}
	if err := webhook.Pause(ctx, f.db, e.ID, "operator", "test pause"); err != nil {
		t.Fatal(err)
	}
	if ok, err := worker.RunOne(ctx, e); err != nil || ok {
		t.Fatal("paused sent", ok, err)
	}
	rotated := e
	rotated.Keys = []webhook.Key{{ID: "new", Secret: strings.Repeat("cd", 32)}, e.Keys[0]}
	if err := webhook.Sync(ctx, f.db, []webhook.Endpoint{rotated}); err != nil {
		t.Fatal(err)
	}
	if ok, err := worker.RunOne(ctx, e); err != nil || ok {
		t.Fatal("stale process sent", ok, err)
	}
	if ok, err := worker.RunOne(ctx, rotated); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
	mustExec(t, f, `UPDATE api_keys SET enabled=false WHERE id=$1`, key.ID)
	mustExec(t, f, `UPDATE webhook_outbox SET next_at=clock_timestamp()-interval '1 second' WHERE sequence=$1`, seq)
	if ok, err := worker.RunOne(ctx, rotated); err != nil || ok {
		t.Fatal("revoked identity sent", ok, err)
	}
	mustExec(t, f, `UPDATE api_keys SET enabled=true WHERE id=$1`, key.ID)
	changed := rotated
	changed.URL = "https://other.example/hook"
	if err := webhook.Sync(ctx, f.db, []webhook.Endpoint{changed}); err == nil {
		t.Fatal("destination silently reassigned")
	}
	// A passed deadline retires the notification without a network call.
	mustExec(t, f, `UPDATE webhook_outbox SET deadline=clock_timestamp()-interval '1 second' WHERE sequence=$1`, seq)
	if ok, err := worker.RunOne(ctx, rotated); ok || err != nil {
		t.Fatal("expired notification sent", ok, err)
	}
	assertCount(t, f, 1, `SELECT count(*) FROM webhook_outbox WHERE sequence=$1 AND state='DEAD'`, seq)
	if calls != 2 {
		t.Fatal(calls)
	}
	assertCount(t, f, 0, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1`, id)
}

func TestWebhookConcurrentClaim(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sendFixture(t, f)
	ctx := context.Background()
	e := webhook.Endpoint{ID: uuid.NewString(), APIKeyID: key.ID, URL: "https://example.com/hook", Keys: []webhook.Key{{ID: "v1", Secret: strings.Repeat("ab", 32)}}}
	if err := webhook.Sync(ctx, f.db, []webhook.Endpoint{e}); err != nil {
		t.Fatal(err)
	}
	submitStatusFixture(t, h, key, f.raw(), nil)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	worker := webhook.Worker{DB: f.db, Sender: webhookSender(func(context.Context, webhook.Endpoint, []byte) (int, error) {
		once.Do(func() { close(entered) })
		<-release
		return 204, nil
	})}
	done := make(chan error, 1)
	go func() { _, err := worker.RunOne(ctx, e); done <- err }()
	<-entered
	if ok, err := worker.RunOne(ctx, e); err != nil || ok {
		t.Fatal("duplicate simultaneous claim", ok, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWebhookFinalCommitFailureAndLeaseFence(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sendFixture(t, f)
	ctx := context.Background()
	e := webhook.Endpoint{ID: uuid.NewString(), APIKeyID: key.ID, URL: "https://example.com/hook", Keys: []webhook.Key{{ID: "v1", Secret: strings.Repeat("ab", 32)}}}
	if err := webhook.Sync(ctx, f.db, []webhook.Endpoint{e}); err != nil {
		t.Fatal(err)
	}
	id := submitStatusFixture(t, h, key, f.raw(), nil)
	var seq int64
	var original string
	if err := f.db.QueryRow(`SELECT sequence,payload FROM webhook_outbox WHERE message_id=$1`, id).Scan(&seq, &original); err != nil {
		t.Fatal(err)
	}
	mustExec(t, f, `CREATE OR REPLACE FUNCTION fail_webhook_ack() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.outcome='ACKNOWLEDGED' THEN RAISE EXCEPTION 'synthetic ack failure'; END IF; RETURN NEW; END $$`)
	t.Cleanup(func() {
		f.db.Exec(`DROP TRIGGER IF EXISTS fail_ack ON webhook_delivery_log`)
		f.db.Exec(`DROP FUNCTION IF EXISTS fail_webhook_ack()`)
	})
	mustExec(t, f, `CREATE TRIGGER fail_ack BEFORE INSERT ON webhook_delivery_log FOR EACH ROW EXECUTE FUNCTION fail_webhook_ack()`)
	calls := 0
	worker := webhook.Worker{DB: f.db, Sender: webhookSender(func(_ context.Context, _ webhook.Endpoint, b []byte) (int, error) {
		calls++
		if string(b) != original {
			t.Fatal("changed payload")
		}
		return 204, nil
	})}
	if ok, err := worker.RunOne(ctx, e); !ok || err == nil {
		t.Fatal("ack write failure ignored", ok, err)
	}
	assertCount(t, f, 1, `SELECT count(*) FROM webhook_outbox WHERE sequence=$1 AND state='IN_FLIGHT' AND attempts=1`, seq)
	mustExec(t, f, `DROP TRIGGER fail_ack ON webhook_delivery_log`)
	mustExec(t, f, `UPDATE webhook_outbox SET lease_until=clock_timestamp()-interval '1 second' WHERE sequence=$1`, seq)
	if ok, err := worker.RunOne(ctx, e); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if calls != 2 {
		t.Fatal("notification did not retry", calls)
	}
	assertCount(t, f, 1, `SELECT count(*) FROM webhook_outbox WHERE sequence=$1 AND state='DELIVERED'`, seq)
	assertCount(t, f, 0, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1`, id)
	// A superseded lease must not acknowledge the new owner's delivery.
	id2 := submitStatusFixture(t, h, key, f.raw(), nil)
	worker.Sender = webhookSender(func(context.Context, webhook.Endpoint, []byte) (int, error) {
		mustExec(t, f, `UPDATE webhook_outbox SET lease_token=$2 WHERE message_id=$1`, id2, uuid.NewString())
		return 204, nil
	})
	if ok, err := worker.RunOne(ctx, e); !ok || err == nil {
		t.Fatal("stale lease acknowledged", ok, err)
	}
	assertCount(t, f, 1, `SELECT count(*) FROM webhook_outbox WHERE message_id=$1 AND state='IN_FLIGHT'`, id2)
}

func TestWebhookEventFailureRollsBackAndEndpointReplacement(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sendFixture(t, f)
	ctx := context.Background()
	e := webhook.Endpoint{ID: uuid.NewString(), APIKeyID: key.ID, URL: "https://example.com/hook", Keys: []webhook.Key{{ID: "v1", Secret: strings.Repeat("ab", 32)}}}
	if err := webhook.Sync(ctx, f.db, []webhook.Endpoint{e}); err != nil {
		t.Fatal(err)
	}
	id := submitStatusFixture(t, h, key, f.raw(), nil)
	mustExec(t, f, `CREATE OR REPLACE FUNCTION fail_webhook_capture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic capture failure'; END $$`)
	t.Cleanup(func() {
		f.db.Exec(`DROP TRIGGER IF EXISTS fail_capture ON webhook_outbox`)
		f.db.Exec(`DROP FUNCTION IF EXISTS fail_webhook_capture()`)
	})
	mustExec(t, f, `CREATE TRIGGER fail_capture BEFORE INSERT ON webhook_outbox FOR EACH ROW EXECUTE FUNCTION fail_webhook_capture()`)
	event := uuid.NewString()
	if _, err := f.db.Exec(`INSERT INTO events(id,message_id,event_type,source,data) VALUES($1,$2,'DELIVERY_UNKNOWN','worker','{}')`, event, id); err == nil {
		t.Fatal("event survived missing notification")
	}
	assertCount(t, f, 0, `SELECT count(*) FROM events WHERE id=$1`, event)
	mustExec(t, f, `DROP TRIGGER fail_capture ON webhook_outbox`)
	next := e
	next.ID = uuid.NewString()
	next.URL = "https://other.example/hook"
	if err := webhook.Sync(ctx, f.db, []webhook.Endpoint{next}); err != nil {
		t.Fatal(err)
	}
	assertCount(t, f, 1, `SELECT count(*) FROM webhook_outbox WHERE message_id=$1 AND endpoint_id=$2`, id, e.ID)
	worker := webhook.Worker{DB: f.db, Sender: webhookSender(func(context.Context, webhook.Endpoint, []byte) (int, error) {
		t.Error("old notification redirected")
		return 204, nil
	})}
	if ok, err := worker.RunOne(ctx, e); ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, err := worker.RunOne(ctx, next); ok || err != nil {
		t.Fatal(ok, err)
	}
}
