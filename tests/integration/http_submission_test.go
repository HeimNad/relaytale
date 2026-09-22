package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"relaytale/internal/api"
	"relaytale/internal/message"
	"relaytale/internal/resource"
	"relaytale/internal/storage"
	"relaytale/internal/testsmtp"
)

func sendFixture(t *testing.T, f *deliveryFixture) (http.Handler, api.SendKey, message.APIReceiver) {
	t.Helper()
	key := api.SendKey{ID: uuid.NewString(), Token: "rts_" + strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", ""), AllowedFrom: []string{"sender@" + f.domain}}
	receiver := message.APIReceiver{Store: storage.LocalStore{Root: f.root, MaxBytes: 1 << 20}, Repo: message.Postgres{DB: f.db}, SnapshotDir: t.TempDir(), Resources: resource.New(8<<20, 16<<20), MaxBytes: 1 << 20}
	raw, _ := json.Marshal([]api.SendKey{key})
	h, e := api.NewSendHandler(context.Background(), string(raw), receiver)
	if e != nil {
		t.Fatal(e)
	}
	return h, key, receiver
}
func sendRequest(h http.Handler, key api.SendKey, idem, raw string, edit func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(raw))
	r.Header.Set("Authorization", "Bearer "+key.Token)
	r.Header.Set("Content-Type", "message/rfc822")
	r.Header.Set("Idempotency-Key", idem)
	r.Header.Set("X-RelayTale-From", key.AllowedFrom[0])
	r.Header.Add("X-RelayTale-To", "one@example.test")
	if edit != nil {
		edit(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func emlCount(t *testing.T, root string) int {
	t.Helper()
	n := 0
	e := filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if !d.IsDir() && strings.HasSuffix(p, ".eml") {
			n++
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	return n
}
func TestHTTPSubmissionReplayAuthorizationAndFiles(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, receiver := sendFixture(t, f)
	first := sendRequest(h, key, "one", f.raw(), nil)
	requireStatus(t, first, 202)
	var result message.EnqueueResult
	if e := json.Unmarshal(first.Body.Bytes(), &result); e != nil {
		t.Fatal(e)
	}
	repeat := sendRequest(h, key, "one", f.raw(), nil)
	requireStatus(t, repeat, 202)
	var replay message.EnqueueResult
	json.Unmarshal(repeat.Body.Bytes(), &replay)
	if !replay.Replayed || replay.MessageID != result.MessageID {
		t.Fatal(replay, result)
	}
	requireStatus(t, sendRequest(h, key, "one", f.raw()+"changed\r\n", nil), 409)
	requireStatus(t, sendRequest(h, key, "bad", f.raw(), func(r *http.Request) { r.Header.Set("X-RelayTale-From", "other@example.test") }), 403)
	requireStatus(t, sendRequest(h, key, "bad", strings.ReplaceAll(f.raw(), "\r\n", "\n"), nil), 400)
	requireStatus(t, sendRequest(h, key, "bad", f.raw(), func(r *http.Request) { r.Header.Set("Origin", "https://example.test") }), 403)
	requireStatus(t, sendRequest(h, key, "bad", f.raw(), func(r *http.Request) { r.Header.Add("Idempotency-Key", "duplicate") }), 400)
	if emlCount(t, f.root) != 1 {
		t.Fatal("replay or rejected request published extra archives")
	}
	entries, e := os.ReadDir(receiver.SnapshotDir)
	if e != nil || len(entries) != 0 {
		t.Fatal(entries, e)
	}
	runOne(t, f.worker)
	if f.status(t, result.MessageID) != "SMTP_ACCEPTED" {
		t.Fatal("worker failed API message")
	}
	if got := <-f.fake.Payloads; string(got) != f.raw() {
		t.Fatal("raw MIME changed")
	}
	mustExec(t, f, `UPDATE api_keys SET enabled=false WHERE id=$1`, key.ID)
	requireStatus(t, sendRequest(h, key, "one", f.raw(), nil), 401)
	// Explicit restart-time rotation preserves the namespace while revoking old tokens.
	old := key
	key.Token = "rts_" + strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")
	raw, _ := json.Marshal([]api.SendKey{key})
	h, e = api.NewSendHandler(context.Background(), string(raw), receiver)
	if e != nil {
		t.Fatal(e)
	}
	requireStatus(t, sendRequest(h, old, "one", f.raw(), nil), 401)
	rotated := sendRequest(h, key, "one", f.raw(), nil)
	requireStatus(t, rotated, 202)
	json.Unmarshal(rotated.Body.Bytes(), &replay)
	if replay.MessageID != result.MessageID {
		t.Fatal("rotation changed namespace")
	}
	if emlCount(t, f.root) != 1 {
		t.Fatal("rotation replay published archive")
	}
}
func TestHTTPSubmissionConcurrentBounds(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, receiver := sendFixture(t, f)
	var wg sync.WaitGroup
	codes := make(chan int, 8)
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; codes <- sendRequest(h, key, "parallel", f.raw(), nil).Code }()
	}
	close(start)
	wg.Wait()
	close(codes)
	accepted := 0
	for code := range codes {
		if code == 202 {
			accepted++
		} else if code != 429 && code != 503 {
			t.Fatal(code)
		}
	}
	if accepted < 1 || emlCount(t, f.root) != 1 {
		t.Fatal(accepted, "duplicate archives")
	}
	assertCount(t, f, 1, `SELECT count(*) FROM messages WHERE api_key_id=$1`, key.ID)
	if entries, e := os.ReadDir(receiver.SnapshotDir); e != nil || len(entries) != 0 {
		t.Fatal(entries, e)
	}
}
func TestHTTPSubmissionSizeRateAndDisabled(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, receiver := sendFixture(t, f)
	requireStatus(t, sendRequest(h, key, "huge", f.raw(), func(r *http.Request) { r.ContentLength = receiver.MaxBytes + 1 }), 413)
	requireStatus(t, sendRequest(h, key, "chunked", strings.Repeat("x", int(receiver.MaxBytes)+1), func(r *http.Request) { r.ContentLength = -1 }), 413)
	// Replays consume admission budget too; otherwise they can exhaust disk reads.
	var last int
	for i := 0; i < 15; i++ {
		last = sendRequest(h, key, "same", f.raw(), nil).Code
	}
	if last != 429 {
		t.Fatal(last)
	}
	disabled, e := api.NewSendHandler(context.Background(), "", receiver)
	if e != nil || disabled != nil {
		t.Fatal("default enabled")
	}
	server := api.Handler(f.db, func() error { return nil }, nil, nil, disabled)
	requireStatus(t, sendRequest(server, key, "no", f.raw(), nil), 404)
}

type rejectHTTPArchive struct{}

func (rejectHTTPArchive) Save(context.Context, string, time.Time, io.Reader) (storage.Archive, error) {
	return storage.Archive{}, errors.New("injected archive failure")
}
func TestHTTPSubmissionArchiveFailure(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, receiver := sendFixture(t, f)
	receiver.Store = rejectHTTPArchive{}
	raw, _ := json.Marshal([]api.SendKey{key})
	h, e := api.NewSendHandler(context.Background(), string(raw), receiver)
	if e != nil {
		t.Fatal(e)
	}
	requireStatus(t, sendRequest(h, key, "failed-archive", f.raw(), nil), 503)
	assertCount(t, f, 0, `SELECT count(*) FROM api_submission_ledger WHERE api_key_id=$1`, key.ID)
	assertCount(t, f, 0, `SELECT count(*) FROM messages WHERE api_key_id=$1`, key.ID)
	stats := receiver.Resources.Stats()
	if stats.Memory != 0 || stats.Spool != 0 {
		t.Fatal(stats)
	}
	if entries, e := os.ReadDir(receiver.SnapshotDir); e != nil || len(entries) != 0 {
		t.Fatal(entries, e)
	}
}

func TestHTTPSubmissionThroughTLSRouter(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sendFixture(t, f)
	server := httptest.NewTLSServer(api.Handler(f.db, func() error { return nil }, nil, nil, h))
	defer server.Close()
	request, e := http.NewRequest("POST", server.URL+"/v1/messages", strings.NewReader(f.raw()))
	if e != nil {
		t.Fatal(e)
	}
	request.Header.Set("Authorization", "Bearer "+key.Token)
	request.Header.Set("Content-Type", "message/rfc822")
	request.Header.Set("Idempotency-Key", "tls-request")
	request.Header.Set("X-RelayTale-From", key.AllowedFrom[0])
	request.Header.Add("X-RelayTale-To", "one@example.test")
	request.Header.Add("X-RelayTale-To", "two@example.test")
	response, e := server.Client().Do(request)
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	if response.StatusCode != 202 || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal(response.StatusCode)
	}
	var result message.EnqueueResult
	if e = json.NewDecoder(response.Body).Decode(&result); e != nil {
		t.Fatal(e)
	}
	assertCount(t, f, 2, `SELECT count(*) FROM recipients WHERE message_id=$1`, result.MessageID)
	assertCount(t, f, 0, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1`, result.MessageID)
}
