package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"relaytale/internal/api"
	"relaytale/internal/message"
	"relaytale/internal/resource"
	"relaytale/internal/testsmtp"
	"relaytale/internal/webhook"
)

func sandboxFixture(t *testing.T, f *deliveryFixture) (http.Handler, api.SendKey, message.APIReceiver) {
	t.Helper()
	_, key, r := sendFixture(t, f)
	key.ID = uuid.NewString()
	key.Token = "rts_" + strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")
	key.Environment = "sandbox"
	r.Resources = resource.New(64<<20, 16<<20)
	raw, _ := json.Marshal([]api.SendKey{key})
	h, err := api.NewSendHandler(context.Background(), string(raw), r)
	if err != nil {
		t.Fatal(err)
	}
	return h, key, r
}
func toolRequest(h http.Handler, key api.SendKey, id, operation, idem string, body []byte) *httptest.ResponseRecorder {
	method := "GET"
	if operation == "simulations" {
		method = "POST"
	}
	r := httptest.NewRequest(method, "/v1/messages/"+id+"/"+operation, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+key.Token)
	if method == "POST" {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", idem)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestSandboxToolsInspectionIntegrityAndScope(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sandboxFixture(t, f)
	raw := strings.Replace(f.raw(), "Content-Type: text/plain", "Content-Type: multipart/mixed; boundary=test-boundary", 1)
	start := strings.Index(raw, "\r\n\r\n") + 4
	raw = raw[:start] + "--test-boundary\r\nContent-Type: text/html\r\n\r\n<script>PRIVATE BODY</script>\r\n--test-boundary\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=report.txt\r\nContent-Transfer-Encoding: base64\r\n\r\nYWJj\r\n--test-boundary--\r\n"
	id := submitStatusFixture(t, h, key, raw, nil)
	response := toolRequest(h, key, id, "inspection", "", nil)
	requireStatus(t, response, 200)
	if strings.Contains(response.Body.String(), "PRIVATE BODY") || !strings.Contains(response.Body.String(), "report.txt") || !strings.Contains(response.Body.String(), `"decoded_bytes":3`) {
		t.Fatal(response.Body.String())
	}
	other, otherKey, _ := sandboxFixture(t, f)
	requireStatus(t, toolRequest(other, otherKey, id, "inspection", "", nil), 404)
	prod, prodKey, _ := sendFixture(t, f)
	requireStatus(t, toolRequest(prod, prodKey, id, "inspection", "", nil), 403)
	var path string
	if err := f.db.QueryRow(`SELECT eml_path FROM messages WHERE id=$1`, id).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(raw+"extra"), 0600); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, toolRequest(h, key, id, "inspection", "", nil), 503)
	mustExec(t, f, `UPDATE messages SET archive_state='PURGED',eml_purged_at=now() WHERE id=$1`, id)
	requireStatus(t, toolRequest(h, key, id, "inspection", "", nil), 410)
	noWork(t, f)
	if f.fake.Connections.Load() != 0 {
		t.Fatal("inspection sent SMTP")
	}
}
func TestSandboxSimulationsReplayIsolationAndAtomicity(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	var suppressionCount int
	if err := f.db.QueryRow(`SELECT count(*) FROM suppression_entries`).Scan(&suppressionCount); err != nil {
		t.Fatal(err)
	}
	h, key, _ := sandboxFixture(t, f)
	endpoint := webhook.Endpoint{SchemaVersion: 2, ID: uuid.NewString(), APIKeyID: key.ID, URL: "https://example.com/sandbox", Keys: []webhook.Key{{ID: "test", Secret: strings.Repeat("ab", 32)}}}
	if err := webhook.Sync(context.Background(), f.db, []webhook.Endpoint{endpoint}); err != nil {
		t.Fatal(err)
	}
	id := submitStatusFixture(t, h, key, f.raw(), nil)
	var recipient string
	if err := f.db.QueryRow(`SELECT id FROM recipients WHERE message_id=$1`, id).Scan(&recipient); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"recipient_id": recipient, "outcome": "bounce"})
	first := toolRequest(h, key, id, "simulations", "bounce-test", body)
	requireStatus(t, first, 200)
	replay := toolRequest(h, key, id, "simulations", "bounce-test", body)
	requireStatus(t, replay, 200)
	var a, b map[string]any
	json.Unmarshal(first.Body.Bytes(), &a)
	json.Unmarshal(replay.Body.Bytes(), &b)
	if a["event_id"] != b["event_id"] || b["replayed"] != true {
		t.Fatal(a, b)
	}
	changed := bytes.Replace(body, []byte("bounce"), []byte("accepted"), 1)
	requireStatus(t, toolRequest(h, key, id, "simulations", "bounce-test", changed), 409)
	assertCount(t, f, 1, `SELECT count(*) FROM sandbox_simulations WHERE message_id=$1`, id)
	assertCount(t, f, 1, `SELECT count(*) FROM webhook_outbox WHERE message_id=$1 AND payload::jsonb->>'type'='SANDBOX_SIMULATED_BOUNCE' AND payload::jsonb->>'simulation'='true'`, id)
	assertCount(t, f, 0, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1`, id)
	assertCount(t, f, suppressionCount, `SELECT count(*) FROM suppression_entries`)
	if f.status(t, id) != "SANDBOX_STORED" || recipientStatus(t, f, id, "one@example.test") != "SANDBOX_STORED" {
		t.Fatal("simulation rewrote delivery state")
	}
	status := statusRequest(h, key, id, "?schema_version=2", nil)
	requireStatus(t, status, 200)
	if !strings.Contains(status.Body.String(), "SANDBOX_SIMULATED_BOUNCE") {
		t.Fatal(status.Body.String())
	}
	// Unknown enum/fields/duplicate JSON never create simulation evidence.
	requireStatus(t, toolRequest(h, key, id, "simulations", "bad", []byte(`{"recipient_id":"`+recipient+`","outcome":"bounce","outcome":"accepted"}`)), 400)
	requireStatus(t, toolRequest(h, key, id, "simulations", "bad", []byte(`{"recipient_id":"`+recipient+`","outcome":"delivered"}`)), 400)
	// Missing outbox fails the complete simulation transaction, not just its notification.
	mustExec(t, f, `CREATE OR REPLACE FUNCTION reject_sim_outbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.message_id='`+id+`'::uuid AND NEW.payload::jsonb->>'type' LIKE 'SANDBOX_SIMULATED_%' THEN RAISE EXCEPTION 'injected'; END IF; RETURN NEW; END $$`)
	mustExec(t, f, `CREATE TRIGGER reject_sim_capture BEFORE INSERT ON webhook_outbox FOR EACH ROW EXECUTE FUNCTION reject_sim_outbox()`)
	t.Cleanup(func() {
		f.db.Exec(`DROP TRIGGER IF EXISTS reject_sim_capture ON webhook_outbox`)
		f.db.Exec(`DROP FUNCTION IF EXISTS reject_sim_outbox()`)
	})
	requireStatus(t, toolRequest(h, key, id, "simulations", "atomic", changed), 503)
	assertCount(t, f, 1, `SELECT count(*) FROM sandbox_simulations WHERE message_id=$1`, id)
	assertCount(t, f, 0, `SELECT count(*) FROM events WHERE message_id=$1 AND event_type='SANDBOX_SIMULATED_ACCEPTED'`, id)
	mustExec(t, f, `DROP TRIGGER reject_sim_capture ON webhook_outbox`)
	// Unledgered SQL simulation rolls back at commit.
	if _, err := f.db.Exec(`INSERT INTO events(id,message_id,recipient_id,event_type,source,data) VALUES(gen_random_uuid(),$1,$2,'SANDBOX_SIMULATED_BOUNCE','sandbox','{}')`, id, recipient); err == nil {
		t.Fatal("unledgered simulation accepted")
	}
	prod, prodKey, _ := sendFixture(t, f)
	requireStatus(t, toolRequest(prod, prodKey, id, "simulations", "x", body), 403)
	other, otherKey, _ := sandboxFixture(t, f)
	requireStatus(t, toolRequest(other, otherKey, id, "simulations", "x", body), 404)
	noWork(t, f)
	if f.fake.Connections.Load() != 0 {
		t.Fatal("simulation connected SMTP")
	}
}
