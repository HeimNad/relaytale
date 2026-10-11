package integration

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"relaytale/internal/api"
	"relaytale/internal/auth"
	"relaytale/internal/message"
	"relaytale/internal/operations"
	"relaytale/internal/queue"
	"relaytale/internal/testsmtp"
	"relaytale/internal/webhook"
)

func controlSubmit(t *testing.T, f *deliveryFixture, receiver message.APIReceiver, key api.SendKey, idem, replaces string, at time.Time) message.EnqueueResult {
	t.Helper()
	result, err := receiver.ReceiveReplacing(context.Background(), message.Envelope{Account: auth.Account{AllowedFrom: key.AllowedFrom}, From: key.AllowedFrom[0], Recipients: []string{"one@example.test"}}, strings.NewReader(f.raw()), key.ID, idem, "normal", time.Time{}, at, replaces)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func cancelRequest(h http.Handler, key api.SendKey, id string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/v1/messages/"+id+"/cancel", nil)
	r.Header.Set("Authorization", "Bearer "+key.Token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestMessageControlCancellationScopeReplayAndWebhook(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, receiver := sendFixture(t, f)
	endpoint := webhook.Endpoint{ID: uuid.NewString(), APIKeyID: key.ID, URL: "https://example.com/hook", Keys: []webhook.Key{{ID: "v1", Secret: strings.Repeat("ab", 32)}}}
	if err := webhook.Sync(context.Background(), f.db, []webhook.Endpoint{endpoint}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	first := controlSubmit(t, f, receiver, key, "cancel-old", "", at)
	other, otherKey, _ := sendFixture(t, f)
	requireStatus(t, cancelRequest(other, otherKey, first.MessageID), 404)
	requireStatus(t, cancelRequest(h, key, first.MessageID), 200)
	repeated := cancelRequest(h, key, first.MessageID)
	requireStatus(t, repeated, 200)
	if !strings.Contains(repeated.Body.String(), `"replayed":true`) {
		t.Fatal(repeated.Body.String())
	}
	if f.status(t, first.MessageID) != "CANCELLED" {
		t.Fatal("not cancelled")
	}
	assertCount(t, f, 1, `SELECT count(*) FROM events WHERE message_id=$1 AND event_type='MESSAGE_CANCELLED'`, first.MessageID)
	assertCount(t, f, 1, `SELECT count(*) FROM webhook_outbox o JOIN events e ON e.id=o.event_id WHERE o.message_id=$1 AND e.event_type='MESSAGE_CANCELLED'`, first.MessageID)
	assertCount(t, f, 1, `SELECT count(*) FROM api_submission_quota WHERE message_id=$1`, first.MessageID)
	requireStatus(t, statusRequest(h, key, first.MessageID, "", nil), 200)
	replay := controlSubmit(t, f, receiver, key, "cancel-old", "", at)
	// Replays retain the original scheduled request identity after cancellation.
	if !replay.Replayed || replay.MessageID != first.MessageID {
		t.Fatal(replay)
	}
	if _, err := f.worker.Repo.Claim(context.Background()); !errors.Is(err, queue.ErrNoJob) {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE messages SET status='QUEUED',next_attempt_at=clock_timestamp() WHERE id=$1`, first.MessageID); err == nil {
		t.Fatal("cancel resurrection allowed")
	}
	if _, err := f.db.Exec(`UPDATE messages SET cancelled_at=NULL WHERE id=$1`, first.MessageID); err == nil {
		t.Fatal("cancel erase allowed")
	}
	if _, err := f.db.Exec(`INSERT INTO delivery_attempts(id,message_id,provider_id,attempt_number,result) VALUES($1,$2,$3,1,'IN_PROGRESS')`, uuid.NewString(), first.MessageID, providerID(t, f)); err == nil {
		t.Fatal("cancelled attempt allowed")
	}
	mustExec(t, f, `UPDATE api_keys SET enabled=false WHERE id=$1`, key.ID)
	requireStatus(t, cancelRequest(h, key, first.MessageID), 401)
}
func TestMessageControlClaimAndDataWin(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, receiver := sendFixture(t, f)
	result := controlSubmit(t, f, receiver, key, "active", "", time.Time{})
	job, err := f.worker.Repo.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, cancelRequest(h, key, result.MessageID), 409)
	if err = f.worker.Repo.ArmData(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, cancelRequest(h, key, result.MessageID), 409)
	mustExec(t, f, `UPDATE messages SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, result.MessageID)
	if err = f.worker.Repo.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, cancelRequest(h, key, result.MessageID), 409)
	if f.status(t, result.MessageID) != "DELIVERY_UNKNOWN" {
		t.Fatal("uncertainty lost")
	}
	assertCount(t, f, 0, `SELECT count(*) FROM events WHERE message_id=$1 AND event_type='MESSAGE_CANCELLED'`, result.MessageID)
}
func TestMessageControlReplacementAtomicityReplayAndReschedule(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, receiver := sendFixture(t, f)
	at := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	old := controlSubmit(t, f, receiver, key, "old", "", at)
	failing := receiver
	failing.Store = rejectHTTPArchive{}
	_, err := failing.ReceiveReplacing(context.Background(), message.Envelope{Account: auth.Account{AllowedFrom: key.AllowedFrom}, From: key.AllowedFrom[0], Recipients: []string{"one@example.test"}}, strings.NewReader(f.raw()), key.ID, "replacement", "normal", time.Time{}, at.Add(time.Hour), old.MessageID)
	if err == nil {
		t.Fatal("archive failure hidden")
	}
	if f.status(t, old.MessageID) != "QUEUED" {
		t.Fatal("failed replacement cancelled old")
	}
	assertCount(t, f, 0, `SELECT count(*) FROM events WHERE message_id=$1 AND event_type='MESSAGE_CANCELLED'`, old.MessageID)
	fresh := controlSubmit(t, f, receiver, key, "replacement", old.MessageID, at.Add(time.Hour))
	replay := controlSubmit(t, f, receiver, key, "replacement", old.MessageID, at.Add(time.Hour))
	if !replay.Replayed || replay.MessageID != fresh.MessageID {
		t.Fatal(replay)
	}
	if f.status(t, old.MessageID) != "CANCELLED" || f.status(t, fresh.MessageID) != "QUEUED" {
		t.Fatal("replacement states")
	}
	assertCount(t, f, 1, `SELECT count(*) FROM messages WHERE id=$1 AND replaces_message_id=$2 AND schedule_at=$3`, fresh.MessageID, old.MessageID, at.Add(time.Hour))
	status := statusRequest(h, key, fresh.MessageID, "", nil)
	requireStatus(t, status, 200)
	if status.Header().Get("X-RelayTale-Replaces") != old.MessageID {
		t.Fatal("replacement link absent")
	}
	if emlCount(t, f.root) != 2 {
		t.Fatal("replacement replay created archives")
	}
	requireStatus(t, sendRequest(h, key, "another", f.raw(), func(r *http.Request) { r.Header.Set("X-RelayTale-Replaces", old.MessageID) }), 409)
	requireStatus(t, sendRequest(h, key, "bad", f.raw(), func(r *http.Request) { r.Header.Set("X-RelayTale-Replaces", "bad") }), 400)
	assertCount(t, f, 2, `SELECT count(*) FROM api_submission_quota WHERE api_key_id=$1`, key.ID)
	// A changed replacement target is part of the immutable request fingerprint.
	requireStatus(t, sendRequest(h, key, "replacement", f.raw(), func(r *http.Request) {
		r.Header.Set("X-RelayTale-Replaces", fresh.MessageID)
		scheduleHeader(at.Add(time.Hour))(r)
	}), 409)
}
func TestMessageControlConcurrentReplacementSingleWinner(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, receiver := sendFixture(t, f)
	old := controlSubmit(t, f, receiver, key, "old", "", time.Time{})
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, k := range []string{"new-a", "new-b"} {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			_, err := receiver.ReceiveReplacing(context.Background(), message.Envelope{Account: auth.Account{AllowedFrom: key.AllowedFrom}, From: key.AllowedFrom[0], Recipients: []string{"one@example.test"}}, strings.NewReader(f.raw()), key.ID, k, "normal", time.Time{}, time.Time{}, old.MessageID)
			results <- err
		}(k)
	}
	wg.Wait()
	close(results)
	won := 0
	for err := range results {
		if err == nil {
			won++
		} else if !errors.Is(err, message.ErrNotCancellable) {
			t.Fatal(err)
		}
	}
	if won != 1 {
		t.Fatal(won)
	}
	assertCount(t, f, 1, `SELECT count(*) FROM messages WHERE replaces_message_id=$1`, old.MessageID)
	if emlCount(t, f.root) != 2 {
		t.Fatal("loser published archive")
	}
}

func TestMessageControlCommitFailureAndCapacityRollback(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, receiver := sendFixture(t, f)
	at := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	old := controlSubmit(t, f, receiver, key, "old", "", at)
	mustExec(t, f, `CREATE FUNCTION fail_control_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event_type='MESSAGE_CANCELLED' THEN RAISE EXCEPTION 'injected control commit failure'; END IF; RETURN NEW; END $$; CREATE CONSTRAINT TRIGGER fail_control_commit AFTER INSERT ON events DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_control_commit()`)
	envelope := message.Envelope{Account: auth.Account{AllowedFrom: key.AllowedFrom}, From: key.AllowedFrom[0], Recipients: []string{"one@example.test"}}
	_, err := receiver.ReceiveReplacing(context.Background(), envelope, strings.NewReader(f.raw()), key.ID, "new", "normal", time.Time{}, at, old.MessageID)
	if err == nil {
		t.Fatal("commit failure hidden")
	}
	if f.status(t, old.MessageID) != "QUEUED" {
		t.Fatal("partial cancellation committed")
	}
	assertCount(t, f, 1, `SELECT count(*) FROM api_submission_ledger WHERE api_key_id=$1`, key.ID)
	mustExec(t, f, `DROP TRIGGER fail_control_commit ON events; DROP FUNCTION fail_control_commit()`)
	// Published EML after a failed/uncertain commit remains an orphan; retry uses
	// the same key, then commits exactly one new message and cancellation.
	fresh := controlSubmit(t, f, receiver, key, "new", old.MessageID, at)
	if f.status(t, old.MessageID) != "CANCELLED" {
		t.Fatal("retry did not cancel")
	}
	assertCount(t, f, 1, `SELECT count(*) FROM messages WHERE replaces_message_id=$1`, old.MessageID)
	// Exhausted website quota must roll back the old cancellation before publish.
	mustExec(t, f, `UPDATE api_keys SET submission_day_limit=2 WHERE id=$1`, key.ID)
	files := emlCount(t, f.root)
	_, err = receiver.ReceiveReplacing(context.Background(), envelope, strings.NewReader(f.raw()), key.ID, "over-budget", "normal", time.Time{}, at, fresh.MessageID)
	if !errors.Is(err, message.ErrSubmissionBudget) {
		t.Fatal(err)
	}
	if f.status(t, fresh.MessageID) != "QUEUED" || emlCount(t, f.root) != files {
		t.Fatal("budget failure changed old message/archive")
	}
}

func TestMessageControlHTTPReplacementAndUnarmedRecovery(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sendFixture(t, f)
	old := submitStatusFixture(t, h, key, f.raw(), nil)
	job, err := f.worker.Repo.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, sendRequest(h, key, "too-late", f.raw(), func(r *http.Request) { r.Header.Set("X-RelayTale-Replaces", old) }), 409)
	f.expire(t, job.ID)
	if err = f.worker.Repo.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A recovered attempt without DATA evidence is safe to replace. Its attempt
	// and recipient history remain available after cancellation.
	fresh := sendRequest(h, key, "after-recovery", f.raw(), func(r *http.Request) { r.Header.Set("X-RelayTale-Replaces", old) })
	requireStatus(t, fresh, 202)
	if f.status(t, old) != "CANCELLED" {
		t.Fatal("unarmed recovery not cancellable")
	}
	assertCount(t, f, 1, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1 AND data_armed_at IS NULL`, old)
	requireStatus(t, sendRequest(h, key, "after-recovery", f.raw(), func(r *http.Request) { r.Header.Set("X-RelayTale-Replaces", old) }), 202)
	mustExec(t, f, `UPDATE delivery_attempts SET finished_at=clock_timestamp()-interval '40 days',raw_debug_log='private-debug' WHERE message_id=$1`, old)
	report, err := operations.Cleanup(context.Background(), f.db, f.root, operations.Retention{DebugDays: 30, Batch: 100, Apply: true, Actor: "control_test"})
	if err != nil || report.DebugCleared != 1 {
		t.Fatal("cancellation blocked debug cleanup", report, err)
	}
	requireStatus(t, cancelRequest(h, key, f.seed(t)), 404)
}

func TestMessageControlRecorderKeepsMessageChildLockOrder(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, receiver := sendFixture(t, f)
	controlSubmit(t, f, receiver, key, "lock-order", "", time.Time{})
	job, err := f.worker.Repo.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`SELECT id FROM messages WHERE id=$1 FOR UPDATE`, job.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// Recorder updates an already existing child row without taking its parent
	// lock. Otherwise a concurrent Finish holding parent could deadlock with it.
	if _, err = f.db.ExecContext(ctx, `UPDATE delivery_attempts SET bytes_sent=0 WHERE id=$1`, job.AttemptID); err != nil {
		t.Fatal("recorder acquired parent lock after child", err)
	}
}
