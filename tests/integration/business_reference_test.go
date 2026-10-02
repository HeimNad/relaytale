package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"relaytale/internal/api"
	"relaytale/internal/message"
	"relaytale/internal/testsmtp"
	"relaytale/internal/webhook"
)

func businessRequest(reference string, tags ...string) func(*http.Request) {
	return func(r *http.Request) {
		if reference != "" {
			r.Header.Set("X-RelayTale-Client-Reference", reference)
		}
		for _, tag := range tags {
			r.Header.Add("X-RelayTale-Tag", tag)
		}
	}
}
func businessList(h http.Handler, key api.SendKey, q string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/v1/messages?"+q, nil)
	r.Header.Set("Authorization", "Bearer "+key.Token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func decodeList(t *testing.T, w *httptest.ResponseRecorder) api.MessageList {
	t.Helper()
	requireStatus(t, w, 200)
	var out api.MessageList
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestBusinessSubmissionIdempotencyAndMIME(t *testing.T) {
	f := delivery(t, testsmtp.Options{DropFinal: true}, 1)
	h, key, _ := sendFixture(t, f)
	w := sendRequest(h, key, "business-key", f.raw(), businessRequest("order:123", "receipt", "order"))
	requireStatus(t, w, 202)
	var first message.EnqueueResult
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	replay := sendRequest(h, key, "business-key", f.raw(), businessRequest("order:123", "order", "receipt"))
	requireStatus(t, replay, 202)
	var same message.EnqueueResult
	json.Unmarshal(replay.Body.Bytes(), &same)
	if !same.Replayed || same.MessageID != first.MessageID {
		t.Fatal(same)
	}
	for _, edit := range []func(*http.Request){businessRequest("order:124", "order", "receipt"), businessRequest("order:123", "order"), nil} {
		requireStatus(t, sendRequest(h, key, "business-key", f.raw(), edit), 409)
	}
	if emlCount(t, f.root) != 1 {
		t.Fatal("replays published new archives")
	}
	var path string
	if err := f.db.QueryRow(`SELECT eml_path FROM messages WHERE id=$1`, first.MessageID).Scan(&path); err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(path)
	if err != nil || string(bytes) != f.raw() {
		t.Fatal("MIME changed", err)
	}
	assertCount(t, f, 1, `SELECT count(*) FROM events WHERE message_id=$1 AND event_type='MESSAGE_RECEIVED' AND data->'business'->>'client_reference'='order:123'`, first.MessageID)
	legacy := decodeStatus(t, statusRequest(h, key, first.MessageID, "", nil))
	if legacy.SchemaVersion != 1 || legacy.Business != nil {
		t.Fatal("legacy schema changed")
	}
	current := decodeStatus(t, statusRequest(h, key, first.MessageID, "?schema_version=2", nil))
	if current.SchemaVersion != 2 || current.Business.ClientReference != "order:123" || !slices.Equal(current.Business.Tags, []string{"order", "receipt"}) {
		t.Fatal(current)
	}
	runOne(t, f.worker)
	after := decodeStatus(t, statusRequest(h, key, first.MessageID, "?schema_version=2", nil))
	if after.Status != "DELIVERY_UNKNOWN" || after.Business.ClientReference != "order:123" {
		t.Fatal(after)
	}
	assertCount(t, f, 1, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1`, first.MessageID)
	for _, query := range []string{`UPDATE messages SET client_reference='different' WHERE id=$1`, `UPDATE messages SET business_tags=ARRAY['other'] WHERE id=$1`, `UPDATE messages SET source_type='smtp',api_key_id=NULL WHERE id=$1`} {
		if _, err = f.db.Exec(query, first.MessageID); err == nil {
			t.Fatal("immutable business changed")
		}
	}
}
func TestBusinessScopedSearchPaginationAndRevocation(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sendFixture(t, f)
	a := submitStatusFixture(t, h, key, f.raw(), businessRequest("order:shared", "order"))
	b := submitStatusFixture(t, h, key, f.raw(), businessRequest("order:shared", "order", "receipt"))
	submitStatusFixture(t, h, key, f.raw(), businessRequest("other", "other"))
	// Equal times force the UUID tie-breaker.
	at := time.Now().UTC()
	mustExec(t, f, `UPDATE messages SET created_at=$3 WHERE id IN ($1,$2)`, a, b, at)
	other, otherKey, _ := sendFixture(t, f)
	foreign := submitStatusFixture(t, other, otherKey, f.raw(), businessRequest("order:shared", "order"))
	first := decodeList(t, businessList(h, key, "client_reference=order:shared&limit=1"))
	if len(first.Messages) != 1 || !first.HasMore || first.SchemaVersion != 2 {
		t.Fatal(first)
	}
	second := decodeList(t, businessList(h, key, "client_reference=order:shared&limit=1&before="+first.NextBefore))
	if len(second.Messages) != 1 || second.HasMore || second.Messages[0].MessageID == first.Messages[0].MessageID {
		t.Fatal(second)
	}
	both := decodeList(t, businessList(h, key, "client_reference=order:shared&tag=receipt"))
	if len(both.Messages) != 1 || both.Messages[0].MessageID != b {
		t.Fatal(both)
	}
	own := decodeList(t, businessList(other, otherKey, "tag=order"))
	if len(own.Messages) != 1 || own.Messages[0].MessageID != foreign {
		t.Fatal(own)
	}
	hidden := businessList(h, key, "tag=order&before="+foreign)
	absent := businessList(h, key, "tag=order&before="+uuid.NewString())
	requireStatus(t, hidden, 400)
	requireStatus(t, absent, 400)
	if hidden.Body.String() != absent.Body.String() {
		t.Fatal("cursor leaked")
	}
	none := decodeList(t, businessList(h, key, "client_reference=missing"))
	if len(none.Messages) != 0 || none.Messages == nil {
		t.Fatal(none)
	}
	mustExec(t, f, `UPDATE api_keys SET enabled=false WHERE id=$1`, key.ID)
	requireStatus(t, businessList(h, key, "tag=order"), 401)
}
func TestBusinessWebhookSchemaAndFrozenBacklog(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sendFixture(t, f)
	ctx := context.Background()
	endpoint := webhook.Endpoint{ID: uuid.NewString(), APIKeyID: key.ID, URL: "https://example.com/events", Keys: []webhook.Key{{ID: "v1", Secret: strings.Repeat("ab", 32)}}}
	if err := webhook.Sync(ctx, f.db, []webhook.Endpoint{endpoint}); err != nil {
		t.Fatal(err)
	}
	id := submitStatusFixture(t, h, key, f.raw(), businessRequest("order:123", "receipt"))
	var old string
	if err := f.db.QueryRow(`SELECT payload FROM webhook_outbox WHERE message_id=$1`, id).Scan(&old); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(old, "business") || !strings.Contains(old, `"schema_version": 1`) {
		t.Fatal(old)
	}
	endpoint.SchemaVersion = 2
	if err := webhook.Sync(ctx, f.db, []webhook.Endpoint{endpoint}); err != nil {
		t.Fatal(err)
	}
	eventID := uuid.NewString()
	mustExec(t, f, `INSERT INTO events(id,message_id,event_type,source,data) VALUES($1,$2,'DELIVERY_UNKNOWN','worker','{}')`, eventID, id)
	var fresh, unchanged string
	if err := f.db.QueryRow(`SELECT payload FROM webhook_outbox WHERE event_id=$1`, eventID).Scan(&fresh); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fresh, `"schema_version": 2`) || !strings.Contains(fresh, `"client_reference": "order:123"`) {
		t.Fatal(fresh)
	}
	if err := f.db.QueryRow(`SELECT payload FROM webhook_outbox WHERE message_id=$1 ORDER BY sequence LIMIT 1`, id).Scan(&unchanged); err != nil || unchanged != old {
		t.Fatal("backlog rewritten", err)
	}
}
func TestBusinessInvalidHeadersBeforeArchive(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sendFixture(t, f)
	for _, edit := range []func(*http.Request){businessRequest(strings.Repeat("x", 129)), businessRequest("valid", "a", "a"), businessRequest("valid", "bad tag"), func(r *http.Request) { r.Header.Add("X-RelayTale-Client-Reference", "") }, func(r *http.Request) {
		r.Header.Add("X-RelayTale-Client-Reference", "a")
		r.Header.Add("X-RelayTale-Client-Reference", "b")
	}} {
		requireStatus(t, sendRequest(h, key, uuid.NewString(), f.raw(), edit), 400)
	}
	if emlCount(t, f.root) != 0 {
		t.Fatal("invalid association archived")
	}
}

func TestBusinessConcurrentFingerprintConflict(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	identity := testAPIKey(t, f)
	a := prepareAPI(t, f, "one@example.test")
	b := prepareAPI(t, f, "one@example.test")
	a.Business = message.Business{ClientReference: "order:a"}
	b.Business = message.Business{ClientReference: "order:b"}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, sub := range []message.Submission{a, b} {
		go func(s message.Submission) {
			<-start
			_, err := (message.Postgres{DB: f.db}).EnqueueIdempotent(context.Background(), s, identity, "concurrent-business")
			results <- err
		}(sub)
	}
	close(start)
	success, conflict := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, message.ErrIdempotencyConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal(success, conflict)
	}
	assertCount(t, f, 1, `SELECT count(*) FROM messages WHERE api_key_id=$1`, identity)
}
