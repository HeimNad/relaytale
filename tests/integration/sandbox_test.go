package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"relaytale/internal/api"
	"relaytale/internal/queue"
	"relaytale/internal/testsmtp"
	"relaytale/internal/webhook"
)

func TestSandboxIsolationReplayAndNotification(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, receiver := sendFixture(t, f)
	// An existing production identity cannot be converted, even with no messages.
	key.Environment = "sandbox"
	raw, _ := json.Marshal([]api.SendKey{key})
	if _, err := api.NewSendHandler(context.Background(), string(raw), receiver); err == nil {
		t.Fatal("production identity converted")
	}
	key.ID = uuid.NewString()
	key.Token = "rts_" + strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")
	raw, _ = json.Marshal([]api.SendKey{key})
	h, err := api.NewSendHandler(context.Background(), string(raw), receiver)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := webhook.Endpoint{ID: uuid.NewString(), APIKeyID: key.ID, URL: "https://example.com/sandbox", Keys: []webhook.Key{{ID: "test", Secret: strings.Repeat("ab", 32)}}}
	if err = webhook.Sync(context.Background(), f.db, []webhook.Endpoint{endpoint}); err != nil {
		t.Fatal(err)
	}
	first := sendRequest(h, key, "sandbox-one", f.raw(), nil)
	requireStatus(t, first, 202)
	var result struct {
		MessageID  string `json:"message_id"`
		Status     string `json:"status"`
		Simulation bool   `json:"simulation"`
		Replayed   bool   `json:"replayed"`
	}
	if err = json.Unmarshal(first.Body.Bytes(), &result); err != nil || !result.Simulation || result.Status != "SANDBOX_STORED" {
		t.Fatal(result, err)
	}
	id := result.MessageID
	repeat := sendRequest(h, key, "sandbox-one", f.raw(), nil)
	requireStatus(t, repeat, 202)
	if err = json.Unmarshal(repeat.Body.Bytes(), &result); err != nil || !result.Replayed || result.MessageID != id {
		t.Fatal(result, err)
	}
	assertCount(t, f, 1, `SELECT count(*) FROM events WHERE message_id=$1 AND event_type='SANDBOX_STORED'`, id)
	assertCount(t, f, 0, `SELECT count(*) FROM events WHERE message_id=$1 AND event_type IN ('MESSAGE_QUEUED','SMTP_ACCEPTED','DELIVERED')`, id)
	assertCount(t, f, 1, `SELECT count(*) FROM webhook_outbox WHERE message_id=$1 AND payload::jsonb->>'simulation'='true' AND payload::jsonb->>'environment'='sandbox' AND payload::jsonb->>'type'='SANDBOX_STORED'`, id)
	status := statusRequest(h, key, id, "", nil)
	requireStatus(t, status, 200)
	var projection api.MessageStatus
	if err = json.Unmarshal(status.Body.Bytes(), &projection); err != nil || !projection.Simulation || projection.Environment != "sandbox" || projection.Status != "SANDBOX_STORED" || len(projection.Recipients) != 1 || projection.Recipients[0].AttemptCount != 0 {
		t.Fatal(projection, err)
	}
	if status.Header().Get("X-RelayTale-Environment") != "sandbox" {
		t.Fatal("missing environment")
	}
	// Real worker and DATA gate both refuse test mail, with no TCP connection.
	noWork(t, f)
	if err = f.worker.Repo.ArmData(context.Background(), queue.Job{ID: id, Token: uuid.NewString(), AttemptID: uuid.NewString()}); err == nil {
		t.Fatal("sandbox DATA authorized")
	}
	if f.fake.Connections.Load() != 0 {
		t.Fatal("sandbox contacted SMTP")
	}
	assertCount(t, f, 0, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1`, id)
	// SQL bypasses cannot convert, route, schedule, or forge accepted facts.
	for _, stmt := range []string{
		`UPDATE api_keys SET environment='production' WHERE id=$1`,
	} {
		if _, err = f.db.Exec(stmt, key.ID); err == nil {
			t.Fatal("identity bypass", stmt)
		}
	}
	for _, stmt := range []string{
		`UPDATE messages SET environment='production',status='QUEUED' WHERE id=$1`,
		`UPDATE messages SET status='QUEUED' WHERE id=$1`,
		`UPDATE messages SET next_attempt_at=now() WHERE id=$1`,
		`UPDATE messages SET api_key_id=(SELECT id FROM api_keys WHERE environment='production' LIMIT 1) WHERE id=$1`,
		`INSERT INTO events(id,message_id,event_type,source,data) VALUES(gen_random_uuid(),$1,'SMTP_ACCEPTED','worker','{}')`,
		`UPDATE recipients SET status='SMTP_ACCEPTED' WHERE message_id=$1`,
		`INSERT INTO delivery_attempts(id,message_id,provider_id,attempt_number,result,claim_token) VALUES(gen_random_uuid(),$1,$2,1,'IN_PROGRESS',gen_random_uuid())`,
	} {
		args := []any{id}
		if strings.HasPrefix(stmt, "INSERT INTO delivery_attempts") {
			args = append(args, f.workerProviderID(t))
		}
		_, err = f.db.Exec(stmt, args...)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || (pgerr.Code != "P0001" && pgerr.Code != "23514") {
			t.Fatal("expected sandbox guard, not an unrelated SQL error", stmt, err)
		}
	}
	// Production credential cannot inspect a test identity's mail.
	prod, prodKey, _ := sendFixture(t, f)
	requireStatus(t, statusRequest(prod, prodKey, id, "", nil), 404)
	// Ordinary production MIME headers cannot opt into sandbox.
	prodID := submitStatusFixture(t, prod, prodKey, strings.Replace(f.raw(), "\r\n\r\n", "\r\nX-RelayTale-Environment: sandbox\r\n\r\n", 1), func(r *http.Request) { r.Header.Set("X-RelayTale-Environment", "sandbox") })
	assertCount(t, f, 1, `SELECT count(*) FROM messages WHERE id=$1 AND environment='production' AND status='QUEUED'`, prodID)
	runOne(t, f.worker)
	if f.status(t, prodID) != "SMTP_ACCEPTED" {
		t.Fatal("production regressed")
	}
	if f.status(t, id) != "SANDBOX_STORED" {
		t.Fatal("test queue released")
	}
}

func (f *deliveryFixture) workerProviderID(t *testing.T) string {
	t.Helper()
	var id string
	if err := f.db.QueryRow(`SELECT id FROM providers WHERE $1=ANY(from_domains)`, f.domain).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
