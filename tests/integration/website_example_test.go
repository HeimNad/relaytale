package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"relaytale/examples/website/site"
	"relaytale/internal/testsmtp"
	"relaytale/internal/webhook"
)

func TestWebsiteExampleDurableSubmissionSignedInboxAndReconciliation(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sandboxFixture(t, f)
	var loseFirst atomic.Bool
	loseFirst.Store(true)
	var blockRead atomic.Bool
	entered, unblock := make(chan struct{}, 1), make(chan struct{})
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Produce a real committed gateway result and then lose its HTTP acknowledgement.
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		if r.Method == "POST" && r.URL.Path == "/v1/messages" && loseFirst.CompareAndSwap(true, false) {
			w.Header().Set("X-RelayTale-Environment", "sandbox")
			w.WriteHeader(503)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/messages/") && r.Method == "GET" && blockRead.CompareAndSwap(true, false) {
			entered <- struct{}{}
			<-unblock
		}
		for name, values := range rr.Header() {
			for _, v := range values {
				w.Header().Add(name, v)
			}
		}
		w.WriteHeader(rr.Code)
		_, _ = w.Write(rr.Body.Bytes())
	}))
	defer gateway.Close()
	defer func() {
		select {
		case <-unblock:
		default:
			close(unblock)
		}
	}()
	keys := []webhook.Key{{ID: "new", Secret: strings.Repeat("cd", 32)}, {ID: "old", Secret: strings.Repeat("ab", 32)}}
	config := site.Config{DB: f.db, GatewayURL: gateway.URL, GatewayToken: key.Token, DemoToken: strings.Repeat("d", 32), Keys: keys}
	app, err := site.New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := webhook.Endpoint{SchemaVersion: 2, ID: uuid.NewString(), APIKeyID: key.ID, URL: "https://example.com/relaytale/hooks", Keys: keys}
	if err = webhook.Sync(context.Background(), f.db, []webhook.Endpoint{endpoint}); err != nil {
		t.Fatal(err)
	}
	request := site.Submission{From: key.AllowedFrom[0], To: []string{"one@example.test"}, MIME: f.raw()}
	idem := uuid.NewString()
	if _, err = app.Submit(context.Background(), idem, request); !errors.Is(err, site.ErrUnavailable) {
		t.Fatal("response loss not reported", err)
	}
	// A fresh backend instance (as after restart) resumes the same durable request.
	restarted, err := site.New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	record, err := restarted.Submit(context.Background(), idem, request)
	if err != nil || record.MessageID == nil {
		t.Fatal(record, err)
	}
	request.MIME += "changed\r\n"
	if _, err = restarted.Submit(context.Background(), idem, request); !errors.Is(err, site.ErrConflict) {
		t.Fatal("changed retry accepted", err)
	}
	assertCount(t, f, 1, `SELECT count(*) FROM messages WHERE api_key_id=$1`, key.ID)
	assertCount(t, f, 1, `SELECT count(*) FROM relaytale_demo_submissions WHERE id=$1`, record.ID)
	mid := *record.MessageID
	var rid string
	if err = f.db.QueryRow(`SELECT id FROM recipients WHERE message_id=$1`, mid).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"recipient_id": rid, "outcome": "bounce"})
	requireStatus(t, toolRequest(h, key, mid, "simulations", "website-bounce", body), 200)
	var stored, simulated string
	if err = f.db.QueryRow(`SELECT payload FROM webhook_outbox WHERE message_id=$1 AND payload::jsonb->>'type'='SANDBOX_STORED'`, mid).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if err = f.db.QueryRow(`SELECT payload FROM webhook_outbox WHERE message_id=$1 AND payload::jsonb->>'type'='SANDBOX_SIMULATED_BOUNCE'`, mid).Scan(&simulated); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(restarted)
	defer server.Close()
	// Exercise the public demonstration submission and query handlers as a website client.
	request.MIME = f.raw()
	encoded, _ := json.Marshal(request)
	demoRequest, _ := http.NewRequest("POST", server.URL+"/demo/submissions", bytes.NewReader(encoded))
	demoRequest.Header.Set("Authorization", "Bearer "+config.DemoToken)
	demoRequest.Header.Set("Content-Type", "application/json")
	demoRequest.Header.Set("Idempotency-Key", idem)
	demoResponse, err := server.Client().Do(demoRequest)
	if err != nil {
		t.Fatal(err)
	}
	var replayRecord site.Record
	decodeErr := json.NewDecoder(demoResponse.Body).Decode(&replayRecord)
	_ = demoResponse.Body.Close()
	if demoResponse.StatusCode != 202 || decodeErr != nil || replayRecord.ID != record.ID {
		t.Fatal("demo HTTP replay", demoResponse.StatusCode, decodeErr)
	}
	query, _ := http.NewRequest("GET", server.URL+"/demo/submissions/"+record.ID, nil)
	query.Header.Set("Authorization", "Bearer "+config.DemoToken)
	queryResponse, err := server.Client().Do(query)
	if err != nil {
		t.Fatal(err)
	}
	_ = queryResponse.Body.Close()
	if queryResponse.StatusCode != 200 {
		t.Fatal("demo HTTP query", queryResponse.StatusCode)
	}
	deliverNotice := func(payload string, at time.Time, signingKeys []webhook.Key) int {
		t.Helper()
		stamp, sig := webhook.Sign(signingKeys, at, []byte(payload))
		req, _ := http.NewRequest("POST", server.URL+"/relaytale/hooks", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-RelayTale-Timestamp", stamp)
		req.Header.Set("X-RelayTale-Signature", sig)
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		return response.StatusCode
	}
	// Signature on exact bytes, stale timestamp and wrong keys are rejected.
	if got := deliverNotice(simulated, time.Now().Add(-6*time.Minute), keys); got != 400 {
		t.Fatal(got)
	}
	if got := deliverNotice(simulated, time.Now(), []webhook.Key{{ID: "old", Secret: strings.Repeat("ef", 32)}}); got != 400 {
		t.Fatal(got)
	}
	// New event, duplicate, then older event. Each event is durably accepted once.
	if got := deliverNotice(simulated, time.Now(), keys); got != 204 {
		t.Fatal(got)
	}
	if got := deliverNotice(simulated, time.Now(), keys[1:]); got != 204 {
		t.Fatal("rotation overlap rejected", got)
	}
	if got := deliverNotice(stored, time.Now(), keys); got != 204 {
		t.Fatal(got)
	}
	assertCount(t, f, 2, `SELECT count(*) FROM relaytale_demo_inbox WHERE submission_id=$1`, record.ID)
	if err = restarted.Reconcile(context.Background(), record.ID); err != nil {
		t.Fatal(err)
	}
	current, err := restarted.Get(context.Background(), record.ID)
	if err != nil || current.NeedsRefresh || !bytes.Contains(current.Snapshot, []byte("SANDBOX_SIMULATED_BOUNCE")) {
		t.Fatal(current, err)
	}
	// A newer callback arriving during a status read must leave reconciliation pending.
	blockRead.Store(true)
	done := make(chan error, 1)
	go func() { done <- restarted.Reconcile(context.Background(), record.ID) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("status read not blocked")
	}
	accepted, _ := json.Marshal(map[string]string{"recipient_id": rid, "outcome": "accepted"})
	requireStatus(t, toolRequest(h, key, mid, "simulations", "website-accepted", accepted), 200)
	var acceptedPayload string
	if err = f.db.QueryRow(`SELECT payload FROM webhook_outbox WHERE message_id=$1 AND payload::jsonb->>'type'='SANDBOX_SIMULATED_ACCEPTED'`, mid).Scan(&acceptedPayload); err != nil {
		t.Fatal(err)
	}
	if got := deliverNotice(acceptedPayload, time.Now(), keys); got != 204 {
		t.Fatal(got)
	}
	close(unblock)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	current, err = restarted.Get(context.Background(), record.ID)
	if err != nil || !current.NeedsRefresh {
		t.Fatal("notification lost during reconciliation", current, err)
	}
	if err = restarted.ReconcilePending(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, err = restarted.Get(context.Background(), record.ID)
	if err != nil || current.NeedsRefresh || !bytes.Contains(current.Snapshot, []byte("SANDBOX_SIMULATED_ACCEPTED")) {
		t.Fatal(current, err)
	}
	// Real notification worker sends the same immutable events through the reference
	// receiver. Earlier manual deliveries are acknowledged as duplicates.
	noticeWorker := webhook.Worker{DB: f.db, Sender: webhookSender(func(_ context.Context, e webhook.Endpoint, b []byte) (int, error) {
		return deliverNotice(string(b), time.Now(), e.Keys), nil
	})}
	for range 3 {
		if ok, err := noticeWorker.RunOne(context.Background(), endpoint); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	assertCount(t, f, 3, `SELECT count(*) FROM webhook_outbox WHERE message_id=$1 AND state='DELIVERED'`, mid)
	assertCount(t, f, 3, `SELECT count(*) FROM relaytale_demo_inbox WHERE submission_id=$1`, record.ID)
	// Neither website callbacks nor simulated results release the SMTP queue.
	noWork(t, f)
	if f.fake.Connections.Load() != 0 {
		t.Fatal("website demo sent SMTP")
	}
	assertCount(t, f, 0, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1`, mid)
}

func TestWebsiteExampleRefusesProductionCredentialBeforeSubmission(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sendFixture(t, f)
	gateway := httptest.NewServer(h)
	defer gateway.Close()
	app, err := site.New(context.Background(), site.Config{DB: f.db, GatewayURL: gateway.URL, GatewayToken: key.Token, DemoToken: strings.Repeat("d", 32), Keys: []webhook.Key{{ID: "demo", Secret: strings.Repeat("ab", 32)}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.Submit(context.Background(), uuid.NewString(), site.Submission{From: key.AllowedFrom[0], To: []string{"one@example.test"}, MIME: f.raw()})
	if !errors.Is(err, site.ErrUnavailable) {
		t.Fatal("production identity not rejected", err)
	}
	assertCount(t, f, 0, `SELECT count(*) FROM messages WHERE api_key_id=$1`, key.ID)
	noWork(t, f)
}
