package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"relaytale/internal/metrics"
	"relaytale/internal/resource"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"relaytale/internal/auth"
	"relaytale/internal/message"
	"relaytale/internal/queue"
	"relaytale/internal/testsmtp"
)

func scheduleHeader(at time.Time) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("X-RelayTale-Schedule-At", at.Format(time.RFC3339)) }
}
func TestMessageScheduleAdmissionReplayAndGate(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, receiver := sendFixture(t, f)
	for i, bad := range []string{"", "2026-10-08T12:00:00", "2026-10-08T12:00:00.001Z"} {
		requireStatus(t, sendRequest(h, key, fmt.Sprint("bad-schedule", i), f.raw(), func(r *http.Request) { r.Header.Set("X-RelayTale-Schedule-At", bad) }), 400)
	}
	requireStatus(t, sendRequest(h, key, "duplicate-schedule", f.raw(), func(r *http.Request) {
		r.Header.Add("X-RelayTale-Schedule-At", "2026-10-08T12:00:00Z")
		r.Header.Add("X-RelayTale-Schedule-At", "2026-10-08T12:00:01Z")
	}), 400)
	now := time.Now().UTC().Truncate(time.Second)
	requireStatus(t, sendRequest(h, key, "past-schedule", f.raw(), scheduleHeader(now.Add(-time.Hour))), 422)
	requireStatus(t, sendRequest(h, key, "far-schedule", f.raw(), scheduleHeader(now.Add(31*24*time.Hour))), 422)
	at := now.Add(time.Hour)
	requireStatus(t, sendRequest(h, key, "invalid-window", f.raw(), func(r *http.Request) { scheduleHeader(at)(r); expiryHeader(at)(r) }), 422)
	first := sendRequest(h, key, "scheduled", f.raw(), scheduleHeader(at))
	requireStatus(t, first, 202)
	var result message.EnqueueResult
	if err := json.Unmarshal(first.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, sendRequest(h, key, "scheduled", f.raw(), scheduleHeader(at.In(time.FixedZone("offset", 3600)))), 202)
	requireStatus(t, sendRequest(h, key, "scheduled", f.raw(), scheduleHeader(at.Add(time.Second))), 409)
	status := statusRequest(h, key, result.MessageID, "", nil)
	requireStatus(t, status, 200)
	if status.Header().Get("X-RelayTale-Schedule-At") != at.Format(time.RFC3339) {
		t.Fatal("missing schedule status header")
	}
	// Even a moved retry clock cannot bypass the immutable earliest time.
	mustExec(t, f, `UPDATE messages SET next_attempt_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, result.MessageID)
	if _, err := f.worker.Repo.Claim(context.Background()); !errors.Is(err, queue.ErrNoJob) {
		t.Fatal(err)
	}
	metric := httptest.NewRecorder()
	(&metrics.Handler{DB: f.db, Resources: resource.New(64<<20, 256<<20)}).ServeHTTP(metric, httptest.NewRequest("GET", "/metrics", nil))
	if metric.Code != 200 || !strings.Contains(metric.Body.String(), "relaytale_queue_oldest_seconds 0\n") {
		t.Fatal("future schedule counted as late queue", metric.Body.String())
	}
	var attempts int
	if err := f.db.QueryRow(`SELECT count(*) FROM delivery_attempts WHERE message_id=$1`, result.MessageID).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatal(attempts, err)
	}
	if _, err := f.db.Exec(`UPDATE messages SET schedule_at=clock_timestamp() WHERE id=$1`, result.MessageID); err == nil {
		t.Fatal("schedule mutation allowed")
	}
	if _, err := f.db.Exec(`UPDATE messages SET status='SENDING' WHERE id=$1`, result.MessageID); err == nil {
		t.Fatal("early claim allowed")
	}
	if _, err := f.db.Exec(`INSERT INTO delivery_attempts(id,message_id,provider_id,attempt_number,result,data_armed_at) VALUES($1,$2,$3,1,'IN_PROGRESS',clock_timestamp())`, uuid.NewString(), result.MessageID, providerID(t, f)); err == nil || !strings.Contains(err.Error(), "scheduled message cannot authorize early DATA") {
		t.Fatal("early DATA insert guard missing", err)
	}
	if emlCount(t, f.root) != 1 {
		t.Fatal("rejections/replays published archives")
	}
	// A separate short schedule becomes eligible without editing its metadata.
	due := time.Now().UTC().Truncate(time.Second).Add(2 * time.Second)
	if _, err := receiver.ReceiveScheduled(context.Background(), message.Envelope{Account: auth.Account{AllowedFrom: key.AllowedFrom}, From: key.AllowedFrom[0], Recipients: []string{"one@example.test"}}, strings.NewReader(f.raw()), key.ID, "due-schedule", "normal", time.Time{}, due); err != nil {
		t.Fatal(err)
	}
	waitExpiry(t, f, due)
	if _, err := receiver.ReceiveScheduled(context.Background(), message.Envelope{Account: auth.Account{AllowedFrom: key.AllowedFrom}, From: key.AllowedFrom[0], Recipients: []string{"one@example.test"}}, strings.NewReader(f.raw()), key.ID, "due-schedule", "normal", time.Time{}, due); err != nil {
		t.Fatal(err)
	}
	runOne(t, f.worker)
	var accepted int
	if err := f.db.QueryRow(`SELECT count(*) FROM messages WHERE status='SMTP_ACCEPTED' AND api_key_id=$1`, key.ID).Scan(&accepted); err != nil || accepted != 1 {
		t.Fatal(accepted, err)
	}
}

func TestMessageScheduleCapacity(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, receiver := sendFixture(t, f)
	at := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	submit := func(k string, at time.Time) (message.EnqueueResult, error) {
		return receiver.ReceiveScheduled(context.Background(), message.Envelope{Account: auth.Account{AllowedFrom: key.AllowedFrom}, From: key.AllowedFrom[0], Recipients: []string{"one@example.test"}}, strings.NewReader(f.raw()), key.ID, k, "normal", time.Time{}, at)
	}
	for i := 0; i < 99; i++ {
		if _, err := submit(fmt.Sprintf("future-%d", i), at); err != nil {
			t.Fatal(err)
		}
	}
	// Two final admissions serialize on the identity lock; only one gets slot 100.
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func(i int) { _, err := submit(fmt.Sprintf("race-%d", i), at); done <- err }(i)
	}
	admitted, rejected := 0, 0
	for i := 0; i < 2; i++ {
		err := <-done
		if err == nil {
			admitted++
		} else if errors.Is(err, message.ErrScheduleCapacity) {
			rejected++
		} else {
			t.Fatal(err)
		}
	}
	if admitted != 1 || rejected != 1 {
		t.Fatal(admitted, rejected)
	}
	before := emlCount(t, f.root)
	requireStatus(t, sendRequest(h, key, "http-excess", f.raw(), scheduleHeader(at)), 429)
	if _, err := submit("future-excess", at); !errors.Is(err, message.ErrScheduleCapacity) {
		t.Fatal(err)
	}
	if result, err := submit("future-0", at); err != nil || !result.Replayed {
		t.Fatal(result, err)
	}
	if emlCount(t, f.root) != before {
		t.Fatal("capacity rejection published EML")
	}
	var placeholders int
	if err := f.db.QueryRow(`SELECT count(*) FROM api_submission_ledger WHERE api_key_id=$1`, key.ID).Scan(&placeholders); err != nil || placeholders != 100 {
		t.Fatal(placeholders, err)
	}
	if _, err := submit("immediate", time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func TestMessageScheduleByteCapacity(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sendFixture(t, f)
	at := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	id := submitStatusFixture(t, h, key, f.raw(), scheduleHeader(at))
	// Synthetic metadata exercises the byte accounting boundary without allocating
	// a 128 MiB payload. Large-message streaming has its own resource acceptance.
	mustExec(t, f, `UPDATE messages SET eml_size=128*1024*1024 WHERE id=$1`, id)
	before := emlCount(t, f.root)
	requireStatus(t, sendRequest(h, key, "byte-excess", f.raw(), scheduleHeader(at)), 429)
	if emlCount(t, f.root) != before {
		t.Fatal("byte rejection published archive")
	}
}

func TestMessageScheduleAdmissionWaitUsesCurrentClock(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sendFixture(t, f)
	hold, err := f.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback()
	if _, err = hold.Exec(`SELECT id FROM api_keys WHERE id=$1 FOR NO KEY UPDATE`, key.ID); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Second).Add(2 * time.Second)
	done := make(chan int, 1)
	go func() { done <- sendRequest(h, key, "waiting-schedule", f.raw(), scheduleHeader(at)).Code }()
	waitExpiry(t, f, at)
	if err = hold.Commit(); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 422 {
		t.Fatal(code)
	}
	if emlCount(t, f.root) != 0 {
		t.Fatal("elapsed admission published EML")
	}
	var n int
	if err = f.db.QueryRow(`SELECT count(*) FROM api_submission_ledger WHERE api_key_id=$1`, key.ID).Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}
