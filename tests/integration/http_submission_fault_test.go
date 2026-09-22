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
	"relaytale/internal/message"
	"relaytale/internal/storage"
	"relaytale/internal/testsmtp"
)

func TestHTTPSubmissionLostResponse(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sendFixture(t, f)
	var calls atomic.Int32
	committed := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) != 1 {
			h.ServeHTTP(w, r)
			return
		}
		// Run the real handler through commit, then lose the entire HTTP response.
		result := httptest.NewRecorder()
		h.ServeHTTP(result, r)
		if result.Code != 202 {
			t.Errorf("before response loss: %d", result.Code)
		}
		var accepted message.EnqueueResult
		if err := json.Unmarshal(result.Body.Bytes(), &accepted); err != nil {
			t.Error(err)
		}
		committed <- accepted.MessageID
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	defer server.Close()
	request := func() *http.Request {
		r, err := http.NewRequest("POST", server.URL+"/v1/messages", strings.NewReader(f.raw()))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+key.Token)
		r.Header.Set("Content-Type", "message/rfc822")
		r.Header.Set("Idempotency-Key", "lost-response")
		r.Header.Set("X-RelayTale-From", key.AllowedFrom[0])
		r.Header.Set("X-RelayTale-To", "one@example.test")
		return r
	}
	client := server.Client()
	client.Timeout = 5 * time.Second
	response, err := client.Do(request())
	if response != nil {
		response.Body.Close()
	}
	if err == nil {
		t.Fatal("expected lost response")
	}
	var original string
	select {
	case original = <-committed:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach commit")
	}
	response, err = client.Do(request())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var replay message.EnqueueResult
	if err = json.NewDecoder(response.Body).Decode(&replay); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 202 || !replay.Replayed || replay.MessageID != original {
		t.Fatal(response.StatusCode, replay, original)
	}
	assertCount(t, f, 1, `SELECT count(*) FROM messages WHERE api_key_id=$1`, key.ID)
	if emlCount(t, f.root) != 1 {
		t.Fatal("duplicate archive")
	}
	runOne(t, f.worker)
	assertCount(t, f, 1, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1`, original)
}

func TestHTTPSubmissionInterruptedBody(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, receiver := sendFixture(t, f)
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- sendRequest(h, key, "interrupted", f.raw(), func(r *http.Request) { r.Body = reader; r.ContentLength = -1 })
	}()
	if _, err := writer.Write([]byte(f.raw()[:20])); err != nil {
		t.Fatal(err)
	}
	writer.CloseWithError(io.ErrUnexpectedEOF)
	select {
	case result := <-done:
		requireStatus(t, result, 503)
	case <-time.After(5 * time.Second):
		t.Fatal("interrupted upload did not release admission")
	}
	reader.Close()
	assertCount(t, f, 0, `SELECT count(*) FROM api_submission_ledger WHERE api_key_id=$1`, key.ID)
	if emlCount(t, f.root) != 0 {
		t.Fatal("partial archive published")
	}
	assertHTTPResourcesReleased(t, receiver)
	requireStatus(t, sendRequest(h, key, "interrupted", f.raw(), nil), 202)
}

type cancelAfterHTTPArchive struct {
	store  storage.LocalStore
	cancel context.CancelFunc
}

func (s cancelAfterHTTPArchive) Save(ctx context.Context, id string, at time.Time, r io.Reader) (storage.Archive, error) {
	archive, err := s.store.Save(ctx, id, at, r)
	s.cancel()
	return archive, err
}
func TestHTTPSubmissionCancellationAfterArchive(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	normal, key, receiver := sendFixture(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	receiver.Store = cancelAfterHTTPArchive{storage.LocalStore{Root: f.root, MaxBytes: receiver.MaxBytes}, cancel}
	config, _ := json.Marshal([]api.SendKey{key})
	handler, err := api.NewSendHandler(context.Background(), string(config), receiver)
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, sendRequest(handler, key, "cancel-after-save", f.raw(), func(r *http.Request) { *r = *r.WithContext(ctx) }), 503)
	assertCount(t, f, 0, `SELECT count(*) FROM messages WHERE api_key_id=$1`, key.ID)
	assertCount(t, f, 0, `SELECT count(*) FROM api_submission_ledger WHERE api_key_id=$1`, key.ID)
	// Published originals are deliberately retained, even for this known rollback.
	if emlCount(t, f.root) != 1 {
		t.Fatal("published evidence removed")
	}
	assertHTTPResourcesReleased(t, receiver)
	requireStatus(t, sendRequest(normal, key, "cancel-after-save", f.raw(), nil), 202)
	assertCount(t, f, 1, `SELECT count(*) FROM messages WHERE api_key_id=$1`, key.ID)
	if emlCount(t, f.root) != 2 {
		t.Fatal("retained candidate or retry archive missing")
	}
}
func assertHTTPResourcesReleased(t *testing.T, r message.APIReceiver) {
	t.Helper()
	stats := r.Resources.Stats()
	if stats.Memory != 0 || stats.Spool != 0 {
		t.Fatal(stats)
	}
	if files, err := os.ReadDir(r.SnapshotDir); err != nil || len(files) != 0 {
		t.Fatal(files, err)
	}
}

func TestHTTPSubmissionCommitFailureRetainsArchive(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, receiver := sendFixture(t, f)
	// The constraint fires only at COMMIT and only for this fixture's identity.
	name := "fail_http_" + strings.ReplaceAll(key.ID, "-", "")
	mustExec(t, f, `CREATE FUNCTION `+name+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.api_key_id='`+key.ID+`'::uuid THEN RAISE EXCEPTION 'injected HTTP commit failure'; END IF; RETURN NEW; END $$`)
	mustExec(t, f, `CREATE CONSTRAINT TRIGGER `+name+` AFTER INSERT ON messages DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION `+name+`() `)
	t.Cleanup(func() {
		f.db.Exec(`DROP TRIGGER IF EXISTS ` + name + ` ON messages`)
		f.db.Exec(`DROP FUNCTION IF EXISTS ` + name + `()`)
	})
	requireStatus(t, sendRequest(h, key, "commit-failure", f.raw(), nil), 503)
	assertCount(t, f, 0, `SELECT count(*) FROM messages WHERE api_key_id=$1`, key.ID)
	assertCount(t, f, 0, `SELECT count(*) FROM api_submission_ledger WHERE api_key_id=$1`, key.ID)
	if emlCount(t, f.root) != 1 {
		t.Fatal("commit error removed published original")
	}
	assertHTTPResourcesReleased(t, receiver)
	mustExec(t, f, `DROP TRIGGER `+name+` ON messages`)
	requireStatus(t, sendRequest(h, key, "commit-failure", f.raw(), nil), 202)
	assertCount(t, f, 1, `SELECT count(*) FROM messages WHERE api_key_id=$1`, key.ID)
	if emlCount(t, f.root) != 2 {
		t.Fatal("expected retained candidate and committed archive")
	}
}
