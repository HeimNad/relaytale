package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"relaytale/internal/api"
	"relaytale/internal/message"
	"relaytale/internal/testsmtp"
)

func budgetHandler(t *testing.T, key api.SendKey, receiver message.APIReceiver) http.Handler {
	t.Helper()
	raw, err := json.Marshal([]api.SendKey{key})
	if err != nil {
		t.Fatal(err)
	}
	h, err := api.NewSendHandler(context.Background(), string(raw), receiver)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestWebsiteBudgetAdmissionReplayAndRestart(t *testing.T) {
	f := delivery(t, testsmtp.Options{DropFinal: true}, 1)
	_, key, receiver := sendFixture(t, f)
	key.Budget = message.SubmissionBudget{Minute: 2, Hour: 3, Day: 4}
	h := budgetHandler(t, key, receiver)
	two := func(r *http.Request) { r.Header.Add("X-RelayTale-To", "two@example.test") }
	first := sendRequest(h, key, "accepted", f.raw(), two)
	requireStatus(t, first, 202)
	var result message.EnqueueResult
	if err := json.Unmarshal(first.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	// A fresh handler has independent in-memory limits but shares durable usage.
	h = budgetHandler(t, key, receiver)
	requireStatus(t, sendRequest(h, key, "accepted", f.raw(), two), 202)
	requireStatus(t, sendRequest(h, key, "accepted", f.raw(), nil), 409)
	exhausted := sendRequest(h, key, "later", f.raw(), nil)
	requireStatus(t, exhausted, 429)
	if !strings.Contains(exhausted.Body.String(), "recipient_budget_exhausted") || exhausted.Header().Get("Retry-After") != "60" {
		t.Fatal(exhausted)
	}
	three := func(r *http.Request) { two(r); r.Header.Add("X-RelayTale-To", "three@example.test") }
	requireStatus(t, sendRequest(h, key, "oversized-batch", f.raw(), three), 422)
	assertCount(t, f, 2, `SELECT coalesce(sum(units),0) FROM api_submission_quota WHERE api_key_id=$1`, key.ID)
	assertCount(t, f, 1, `SELECT count(*) FROM api_submission_ledger WHERE api_key_id=$1`, key.ID)
	if emlCount(t, f.root) != 1 {
		t.Fatal("rejection or replay published an archive")
	}
	// Read access is not coupled to submission budget, including after UNKNOWN.
	runOne(t, f.worker)
	requireStatus(t, statusRequest(h, key, result.MessageID, "", nil), 200)
	requireStatus(t, sendRequest(h, key, "accepted", f.raw(), two), 202)
	assertCount(t, f, 2, `SELECT coalesce(sum(units),0) FROM api_submission_quota WHERE api_key_id=$1`, key.ID)
	other := key
	other.ID = uuid.NewString()
	other.Token = "rts_" + strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")
	requireStatus(t, sendRequest(budgetHandler(t, other, receiver), other, "own", f.raw(), nil), 202)
}

func TestWebsiteBudgetConcurrentHandlersDoNotOverspend(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, receiver := sendFixture(t, f)
	key.Budget = message.SubmissionBudget{Day: 1}
	handlers := []http.Handler{budgetHandler(t, key, receiver), budgetHandler(t, key, receiver)}
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 2)
	for i, h := range handlers {
		wg.Add(1)
		go func(i int, h http.Handler) {
			defer wg.Done()
			<-start
			results <- sendRequest(h, key, []string{"one", "two"}[i], f.raw(), nil)
		}(i, h)
	}
	close(start)
	wg.Wait()
	close(results)
	accepted, denied := 0, 0
	for w := range results {
		switch w.Code {
		case 202:
			accepted++
		case 429:
			denied++
		default:
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if accepted != 1 || denied != 1 {
		t.Fatal(accepted, denied)
	}
	assertCount(t, f, 1, `SELECT coalesce(sum(units),0) FROM api_submission_quota WHERE api_key_id=$1`, key.ID)
	if emlCount(t, f.root) != 1 {
		t.Fatal("losing admission published an archive")
	}
}

func TestWebsiteBudgetRollbackAndPolicyChanges(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, receiver := sendFixture(t, f)
	key.Budget = message.SubmissionBudget{Day: 1}
	failing := receiver
	failing.Store = rejectHTTPArchive{}
	requireStatus(t, sendRequest(budgetHandler(t, key, failing), key, "same", f.raw(), nil), 503)
	assertCount(t, f, 0, `SELECT count(*) FROM api_submission_quota WHERE api_key_id=$1`, key.ID)
	assertCount(t, f, 0, `SELECT count(*) FROM api_submission_ledger WHERE api_key_id=$1`, key.ID)
	h := budgetHandler(t, key, receiver)
	requireStatus(t, sendRequest(h, key, "same", f.raw(), nil), 202)
	// Removing a limit preserves reservations; turning it back on cannot reset it.
	key.Budget = message.SubmissionBudget{}
	requireStatus(t, sendRequest(budgetHandler(t, key, receiver), key, "unlimited", f.raw(), nil), 202)
	key.Budget = message.SubmissionBudget{Day: 1}
	h = budgetHandler(t, key, receiver)
	requireStatus(t, sendRequest(h, key, "new", f.raw(), nil), 429)
	requireStatus(t, sendRequest(h, key, "same", f.raw(), nil), 202)
	assertCount(t, f, 2, `SELECT coalesce(sum(units),0) FROM api_submission_quota WHERE api_key_id=$1`, key.ID)
	if _, err := f.db.Exec(`DELETE FROM api_submission_quota WHERE api_key_id=$1`, key.ID); err == nil {
		t.Fatal("budget history mutable")
	}
}

func TestWebsiteBudgetRollingWindows(t *testing.T) {
	for _, tc := range []struct {
		name, age string
		budget    message.SubmissionBudget
		want      int
	}{
		{"minute-blocked", "30 seconds", message.SubmissionBudget{Minute: 1}, 429},
		{"minute-released", "2 minutes", message.SubmissionBudget{Minute: 1}, 202},
		{"hour-blocked", "30 minutes", message.SubmissionBudget{Hour: 1}, 429},
		{"hour-released", "2 hours", message.SubmissionBudget{Hour: 1}, 202},
		{"day-blocked", "23 hours", message.SubmissionBudget{Day: 1}, 429},
		{"day-released", "25 hours", message.SubmissionBudget{Day: 1}, 202},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := delivery(t, testsmtp.Options{}, 1)
			_, key, receiver := sendFixture(t, f)
			// Seed a historical accepted submission without mutating append-only history.
			id := f.seed(t)
			mustExec(t, f, `UPDATE messages SET source_type='api',api_key_id=$2,smtp_account_id=NULL WHERE id=$1`, id, key.ID)
			mustExec(t, f, `INSERT INTO api_submission_ledger(api_key_id,key_sha256,request_sha256,message_id,created_at) VALUES($1,repeat('a',64),repeat('b',64),$2,clock_timestamp()-$3::interval)`, key.ID, id, tc.age)
			mustExec(t, f, `INSERT INTO api_submission_quota(message_id,api_key_id,units,reserved_at) VALUES($1,$2,1,clock_timestamp()-$3::interval)`, id, key.ID, tc.age)
			key.Budget = tc.budget
			requireStatus(t, sendRequest(budgetHandler(t, key, receiver), key, "new", f.raw(), nil), tc.want)
		})
	}
}
