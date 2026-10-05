package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"relaytale/internal/api"
	"relaytale/internal/auth"
	"relaytale/internal/message"
	"relaytale/internal/queue"
	"relaytale/internal/testsmtp"
)

func streamHeader(stream string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("X-RelayTale-Stream", stream) }
}
func submitStream(t *testing.T, f *deliveryFixture, r message.APIReceiver, key api.SendKey, stream string) string {
	t.Helper()
	result, err := r.ReceiveStream(context.Background(), message.Envelope{Account: auth.Account{AllowedFrom: key.AllowedFrom}, From: key.AllowedFrom[0], Recipients: []string{"one@example.test"}}, strings.NewReader(f.raw()), key.ID, uuid.NewString(), stream)
	if err != nil {
		t.Fatal(err)
	}
	return result.MessageID
}
func TestMessageStreamAdmissionReplayAndRevocation(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, receiver := sendFixture(t, f)
	key.Budget = message.SubmissionBudget{Day: 3}
	key.Streams = []message.StreamPolicy{{Name: "normal", Weight: 1, Budget: message.SubmissionBudget{Day: 1}}, {Name: "otp", Weight: 3, Budget: message.SubmissionBudget{Day: 1}}}
	h := budgetHandler(t, key, receiver)
	requireStatus(t, sendRequest(h, key, "denied", f.raw(), streamHeader("security")), 403)
	requireStatus(t, sendRequest(h, key, "invalid", f.raw(), streamHeader("urgent")), 400)
	requireStatus(t, sendRequest(h, key, "normal", f.raw(), nil), 202)
	requireStatus(t, sendRequest(h, key, "normal", f.raw(), streamHeader("normal")), 202)
	first := sendRequest(h, key, "otp", f.raw(), streamHeader("otp"))
	requireStatus(t, first, 202)
	var result message.EnqueueResult
	if err := json.Unmarshal(first.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	w := sendRequest(h, key, "full", f.raw(), streamHeader("otp"))
	requireStatus(t, w, 429)
	if !strings.Contains(w.Body.String(), "stream_budget_exhausted") {
		t.Fatal(w.Body.String())
	}
	requireStatus(t, sendRequest(h, key, "otp", f.raw(), streamHeader("otp")), 202)
	requireStatus(t, sendRequest(h, key, "otp", f.raw(), nil), 409)
	if emlCount(t, f.root) != 2 {
		t.Fatal("stream rejection published files")
	}
	read := statusRequest(h, key, result.MessageID, "", nil)
	requireStatus(t, read, 200)
	if read.Header().Get("X-RelayTale-Stream") != "otp" {
		t.Fatal("missing stream metadata")
	}
	assertCount(t, f, 2, `SELECT sum(units) FROM api_submission_quota WHERE api_key_id=$1`, key.ID)
	if _, err := f.db.Exec(`UPDATE messages SET message_stream='normal' WHERE id=$1`, result.MessageID); err == nil {
		t.Fatal("stream changed after admission")
	}
	key.Streams = nil
	h = budgetHandler(t, key, receiver)
	requireStatus(t, sendRequest(h, key, "revoked", f.raw(), streamHeader("otp")), 403)
	// A stale process also rechecks DB policy before accepting a fresh request.
	if _, err := receiver.ReceiveStream(context.Background(), message.Envelope{Account: auth.Account{AllowedFrom: key.AllowedFrom}, From: key.AllowedFrom[0], Recipients: []string{"one@example.test"}}, strings.NewReader(f.raw()), key.ID, "stale", "otp"); err != message.ErrStreamDenied {
		t.Fatal(err)
	}
	runOne(t, f.worker)
	runOne(t, f.worker) // Revocation does not discard committed mail.
	assertCount(t, f, 2, `SELECT count(*) FROM messages WHERE api_key_id=$1 AND status='SMTP_ACCEPTED'`, key.ID)
	for i := 0; i < 2; i++ {
		if got := <-f.fake.Payloads; string(got) != f.raw() {
			t.Fatal("MIME changed")
		}
	}
}
func TestMessageStreamWeightedTurnsAndRestart(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, r := sendFixture(t, f)
	key.Streams = []message.StreamPolicy{{Name: "otp", Weight: 3}}
	budgetHandler(t, key, r)
	for i := 0; i < 3; i++ {
		submitStream(t, f, r, key, "normal")
	}
	for i := 0; i < 8; i++ {
		submitStream(t, f, r, key, "otp")
	}
	want := []string{"normal", "otp", "otp", "otp", "normal", "otp", "otp", "otp"}
	for i, stream := range want {
		worker := f.worker
		worker.Repo = queue.Repository{DB: f.db} // reconstruct each worker
		runOne(t, worker)
		var got string
		if err := f.db.QueryRow(`SELECT m.message_stream FROM delivery_attempts a JOIN messages m ON m.id=a.message_id WHERE m.api_key_id=$1 ORDER BY a.started_at DESC,a.id DESC LIMIT 1`, key.ID).Scan(&got); err != nil || got != stream {
			t.Fatal(i, got, stream, err)
		}
	}
	assertCount(t, f, 2, `SELECT count(*) FROM messages WHERE api_key_id=$1 AND message_stream='normal' AND status='SMTP_ACCEPTED'`, key.ID)
}
func TestMessageStreamIdentityRoundRobin(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	_, a, ra := sendFixture(t, f)
	_, b, rb := sendFixture(t, f)
	for i := 0; i < 6; i++ {
		submitStream(t, f, ra, a, "normal")
	}
	for i := 0; i < 2; i++ {
		submitStream(t, f, rb, b, "normal")
	}
	var order []string
	for i := 0; i < 4; i++ {
		runOne(t, f.worker)
		var owner string
		if err := f.db.QueryRow(`SELECT m.api_key_id FROM delivery_attempts t JOIN messages m ON m.id=t.message_id WHERE m.api_key_id IN ($1,$2) ORDER BY t.started_at DESC,t.id DESC LIMIT 1`, a.ID, b.ID).Scan(&owner); err != nil {
			t.Fatal(err)
		}
		order = append(order, owner)
	}
	if order[0] == order[1] || order[0] != order[2] || order[1] != order[3] {
		t.Fatal("one website monopolized claims", order)
	}
}
func TestMessageStreamQuantum(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	addresses := make([]string, 20)
	for i := range addresses {
		addresses[i] = fmt.Sprintf("user%d@example.test", i)
	}
	id, err := f.receiver().Receive(context.Background(), message.Envelope{Account: auth.Account{ID: f.accountID, AllowedFrom: []string{"sender@" + f.domain}}, From: "sender@" + f.domain, Recipients: addresses}, strings.NewReader(f.raw()))
	if err != nil {
		t.Fatal(err)
	}
	runOne(t, f.worker)
	if got := <-f.fake.Envelopes; len(got) != queue.FairRecipientQuantum {
		t.Fatal(len(got))
	}
	if f.status(t, id) != "QUEUED" {
		t.Fatal("remaining recipients lost")
	}
	runOne(t, f.worker)
	if got := <-f.fake.Envelopes; len(got) != 4 {
		t.Fatal("duplicate or missing remainder", got)
	}
	if f.status(t, id) != "SMTP_ACCEPTED" {
		t.Fatal(f.status(t, id))
	}
}
