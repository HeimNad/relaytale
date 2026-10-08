package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"relaytale/internal/api"
	"relaytale/internal/resource"
	"relaytale/internal/testsmtp"
)

func jsonSubmit(h http.Handler, k api.SendKey, body, idem string, edit func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+k.Token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", idem)
	r.Header.Set("X-RelayTale-From", k.AllowedFrom[0])
	r.Header.Add("X-RelayTale-To", "one@example.test")
	if edit != nil {
		edit(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestJSONSubmissionDurableReplayAndSandbox(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sendFixture(t, f)
	body := `{"date":"2026-10-07T12:00:00Z","subject":"中文通知","text":"hello","html":"<p>hello</p>","from_name":"网站"}`
	w := jsonSubmit(h, key, body, "json-replay", nil)
	requireStatus(t, w, 202)
	var result struct {
		MessageID string `json:"message_id"`
		Replayed  bool   `json:"replayed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var path string
	if err := f.db.QueryRow("SELECT eml_path FROM messages WHERE id=$1", result.MessageID).Scan(&path); err != nil {
		t.Fatal(err)
	}
	archived, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reordered := `{"html":"<p>hello</p>","text":"hello","from_name":"网站","subject":"中文通知","date":"2026-10-07T14:00:00+02:00"}`
	w = jsonSubmit(h, key, reordered, "json-replay", nil)
	requireStatus(t, w, 202)
	json.Unmarshal(w.Body.Bytes(), &result)
	if !result.Replayed {
		t.Fatal("retry did not replay")
	}
	w = jsonSubmit(h, key, strings.Replace(body, "hello", "changed", 1), "json-replay", nil)
	requireStatus(t, w, 409)
	assertCount(t, f, 1, "SELECT count(*) FROM messages")
	runOne(t, f.worker)
	assertCount(t, f, 1, "SELECT count(*) FROM messages WHERE status='SMTP_ACCEPTED'")
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(archived) {
		t.Fatal("archive changed", err)
	}
	// Isolated testing identity must never become a production queue entry.
	sandbox, sk, _ := sandboxFixture(t, f)
	w = jsonSubmit(sandbox, sk, body, "json-sandbox", nil)
	requireStatus(t, w, 202)
	assertCount(t, f, 1, "SELECT count(*) FROM messages WHERE status='SANDBOX_STORED'")
	assertCount(t, f, 1, "SELECT count(*) FROM delivery_attempts")
}
func TestJSONSubmissionRejectsInvalidBeforeEnqueue(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sendFixture(t, f)
	good := `{"date":"2026-10-07T12:00:00Z","text":"hello"}`
	for i, body := range []string{`{"date":"x","text":"hello"}`, `{"date":"x","date":"y","text":"hello"}`, `{"date":"2026-10-07T12:00:00Z","text":null}`, `{"date":"2026-10-07T12:00:00Z","text":"x","attachments":"url"}`, `{"date":"2026-10-07T12:00:00Z","text":"x","subject":"bad\r\nBcc: x"}`} {
		requireStatus(t, jsonSubmit(h, key, body, string(rune('a'+i)), nil), 400)
	}
	requireStatus(t, jsonSubmit(h, key, strings.Repeat("x", (32<<10)+1), "too-big", nil), 413)
	requireStatus(t, jsonSubmit(h, key, good, "denied", func(r *http.Request) { r.Header.Set("X-RelayTale-From", "other@example.test") }), 403)
	requireStatus(t, jsonSubmit(h, key, good, "origin", func(r *http.Request) { r.Header.Set("Origin", "https://other.test") }), 403)
	assertCount(t, f, 0, "SELECT count(*) FROM messages")
	assertCount(t, f, 0, "SELECT count(*) FROM api_submission_ledger")
}

func TestJSONSubmissionWaitsForResourcesBeforeReading(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, receiver := sendFixture(t, f)
	limiter := resource.New(resource.ReceiveMemory, 16<<20)
	receiver.Resources = limiter
	config, _ := json.Marshal([]api.SendKey{key})
	h, err := api.NewSendHandler(context.Background(), string(config), receiver)
	if err != nil {
		t.Fatal(err)
	}
	release, err := limiter.Acquire(context.Background(), resource.ReceiveMemory, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body := &countedJSONBody{Reader: strings.NewReader(`{"date":"2026-10-07T12:00:00Z","text":"hello"}`)}
	r := httptest.NewRequest("POST", "/v1/messages", nil).WithContext(ctx)
	r.Body = body
	r.Header.Set("Authorization", "Bearer "+key.Token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "wait-resources")
	r.Header.Set("X-RelayTale-From", key.AllowedFrom[0])
	r.Header.Set("X-RelayTale-To", "one@example.test")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { w := httptest.NewRecorder(); h.ServeHTTP(w, r); done <- w }()
	for limiter.Stats().Waiting == 0 {
		select {
		case <-ctx.Done():
			t.Fatal("request never waited for memory")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if body.reads.Load() != 0 {
		t.Fatal("JSON read before resource admission")
	}
	cancel()
	w := <-done
	requireStatus(t, w, 503)
	if body.reads.Load() != 0 {
		t.Fatal("cancelled request read JSON")
	}
	release()
	// The exact one-operation budget can succeed; there is no nested reservation.
	requireStatus(t, jsonSubmit(h, key, `{"date":"2026-10-07T12:00:00Z","text":"hello"}`, "after-release", nil), 202)
}

type countedJSONBody struct {
	io.Reader
	reads atomic.Int32
}

func (r *countedJSONBody) Read(p []byte) (int, error) { r.reads.Add(1); return r.Reader.Read(p) }
func (r *countedJSONBody) Close() error               { return nil }

func TestJSONAttachmentsDurableReplay(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sendFixture(t, f)
	body := `{"date":"2026-10-07T12:00:00Z","text":"invoice","attachments":[{"filename":"订单.pdf","content_type":"application/pdf","content_base64":"AP8NCg=="}]}`
	w := jsonSubmit(h, key, body, "attachment", nil)
	requireStatus(t, w, 202)
	var result struct {
		MessageID string `json:"message_id"`
		Replayed  bool   `json:"replayed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var path string
	if err := f.db.QueryRow("SELECT eml_path FROM messages WHERE id=$1", result.MessageID).Scan(&path); err != nil {
		t.Fatal(err)
	}
	archived, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(archived), "json-v2") {
		t.Fatal(err)
	}
	w = jsonSubmit(h, key, strings.Replace(body, "application/pdf", "APPLICATION/PDF", 1), "attachment", nil)
	requireStatus(t, w, 202)
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || !result.Replayed {
		t.Fatal(err)
	}
	requireStatus(t, jsonSubmit(h, key, strings.Replace(body, "AP8NCg==", "eA==", 1), "attachment", nil), 409)
	requireStatus(t, jsonSubmit(h, key, strings.Replace(body, "订单.pdf", "../bad.pdf", 1), "bad-attachment", nil), 400)
	assertCount(t, f, 1, "SELECT count(*) FROM messages")
	runOne(t, f.worker)
	assertCount(t, f, 1, "SELECT count(*) FROM messages WHERE status='SMTP_ACCEPTED'")
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(archived) {
		t.Fatal("archive changed", err)
	}
}
