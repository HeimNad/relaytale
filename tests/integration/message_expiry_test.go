package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"relaytale/internal/api"
	"relaytale/internal/auth"
	"relaytale/internal/message"
	"relaytale/internal/operations"
	"relaytale/internal/queue"
	"relaytale/internal/smtpclient"
	"relaytale/internal/testsmtp"
	"relaytale/internal/webhook"
)

func expiryHeader(at time.Time) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("X-RelayTale-Expires-At", at.Format(time.RFC3339)) }
}
func soonExpiry(t *testing.T, f *deliveryFixture) time.Time {
	t.Helper()
	var at time.Time
	if err := f.db.QueryRow(`SELECT date_trunc('second',clock_timestamp())+interval '2 seconds'`).Scan(&at); err != nil {
		t.Fatal(err)
	}
	mustExec(t, f, `UPDATE providers SET timeout_seconds=10 WHERE id=$1`, providerID(t, f))
	return at
}
func waitExpiry(t *testing.T, f *deliveryFixture, at time.Time) {
	t.Helper()
	if _, err := f.db.Exec(`SELECT pg_sleep(greatest(0,extract(epoch FROM $1::timestamptz-clock_timestamp()))+0.03)`, at); err != nil {
		t.Fatal(err)
	}
}
func expiringSeed(t *testing.T, f *deliveryFixture, r message.APIReceiver, key api.SendKey, at time.Time, addresses ...string) string {
	t.Helper()
	result, err := r.ReceiveExpiring(context.Background(), message.Envelope{Account: auth.Account{AllowedFrom: key.AllowedFrom}, From: key.AllowedFrom[0], Recipients: addresses}, strings.NewReader(f.raw()), key.ID, uuid.NewString(), "normal", at)
	if err != nil {
		t.Fatal(err)
	}
	return result.MessageID
}
func TestMessageExpiryAdmissionAndReplay(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sendFixture(t, f)
	for i, bad := range []string{"", "2026-10-05T12:00:00", "2026-10-05T12:00:00.001Z"} {
		requireStatus(t, sendRequest(h, key, fmt.Sprint("invalid", i), f.raw(), func(r *http.Request) { r.Header.Set("X-RelayTale-Expires-At", bad) }), 400)
	}
	requireStatus(t, sendRequest(h, key, "duplicate", f.raw(), func(r *http.Request) {
		r.Header.Add("X-RelayTale-Expires-At", "2026-10-05T12:00:00Z")
		r.Header.Add("X-RelayTale-Expires-At", "2026-10-05T12:00:01Z")
	}), 400)
	requireStatus(t, sendRequest(h, key, "past", f.raw(), expiryHeader(time.Now().UTC().Truncate(time.Second).Add(-time.Hour))), 410)
	requireStatus(t, sendRequest(h, key, "far", f.raw(), expiryHeader(time.Now().UTC().Truncate(time.Second).Add(31*24*time.Hour))), 422)
	at := soonExpiry(t, f)
	first := sendRequest(h, key, "deadline", f.raw(), expiryHeader(at))
	requireStatus(t, first, 202)
	var result message.EnqueueResult
	if err := json.Unmarshal(first.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, sendRequest(h, key, "deadline", f.raw(), expiryHeader(at.Add(time.Second))), 409)
	waitExpiry(t, f, at)
	// The original committed identity wins even after its deadline elapsed.
	requireStatus(t, sendRequest(h, key, "deadline", f.raw(), expiryHeader(at.In(time.FixedZone("offset", 3600)))), 202)
	if emlCount(t, f.root) != 1 {
		t.Fatal("expiry rejection published archives")
	}
	assertCount(t, f, 1, `SELECT count(*) FROM api_submission_ledger WHERE api_key_id=$1`, key.ID)
	assertCount(t, f, 1, `SELECT sum(units) FROM api_submission_quota WHERE api_key_id=$1`, key.ID)
	if _, err := f.db.Exec(`UPDATE messages SET expires_at=expires_at+interval '1 hour' WHERE id=$1`, result.MessageID); err == nil {
		t.Fatal("deadline extended")
	}
	read := statusRequest(h, key, result.MessageID, "", nil)
	requireStatus(t, read, 200)
	if read.Header().Get("X-RelayTale-Expires-At") != at.UTC().Format(time.RFC3339) {
		t.Fatal("deadline metadata missing")
	}
}
func TestMessageExpirySweepWithoutProviderAndWebhook(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, r := sendFixture(t, f)
	e := webhook.Endpoint{ID: uuid.NewString(), APIKeyID: key.ID, URL: "https://example.com/hook", Keys: []webhook.Key{{ID: "v1", Secret: strings.Repeat("ab", 32)}}}
	if err := webhook.Sync(context.Background(), f.db, []webhook.Endpoint{e}); err != nil {
		t.Fatal(err)
	}
	at := soonExpiry(t, f)
	id := expiringSeed(t, f, r, key, at, "one@example.test", "two@example.test")
	mustExec(t, f, `UPDATE providers SET enabled=false WHERE id=$1`, providerID(t, f))
	waitExpiry(t, f, at)
	for range 2 {
		noWork(t, f)
	}
	if f.status(t, id) != "EXPIRED" {
		t.Fatal(f.status(t, id))
	}
	assertCount(t, f, 0, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1`, id)
	assertCount(t, f, 2, `SELECT count(*) FROM events WHERE message_id=$1 AND event_type='RECIPIENT_EXPIRED'`, id)
	assertCount(t, f, 2, `SELECT count(*) FROM webhook_outbox WHERE message_id=$1 AND payload::jsonb->>'type'='RECIPIENT_EXPIRED'`, id)
	assertCount(t, f, 2, `SELECT sum(units) FROM api_submission_quota WHERE message_id=$1`, id)
	read := statusRequest(h, key, id, "", nil)
	requireStatus(t, read, 200)
	if !strings.Contains(read.Body.String(), "EXPIRED") || !strings.Contains(read.Body.String(), "RECIPIENT_EXPIRED") {
		t.Fatal(read.Body.String())
	}
}
func TestMessageExpiryGatePreservesSMTPRejections(t *testing.T) {
	f := delivery(t, testsmtp.Options{RejectRecipients: map[string]int{"two@example.test": 550}}, 1)
	_, key, r := sendFixture(t, f)
	at := soonExpiry(t, f)
	id := expiringSeed(t, f, r, key, at, "one@example.test", "two@example.test")
	f.worker.Sender = suppressionSender{f.worker.Sender, func(ctx context.Context, gate func(context.Context) error) error {
		waitExpiry(t, f, at)
		return gate(ctx)
	}}
	runOne(t, f.worker)
	if recipientStatus(t, f, id, "one@example.test") != "EXPIRED" || recipientStatus(t, f, id, "two@example.test") != "PERM_FAILED" {
		t.Fatal("SMTP rejection or expiry lost")
	}
	select {
	case <-f.fake.Payloads:
		t.Fatal("DATA sent after expiry")
	default:
	}
	assertCount(t, f, 1, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1 AND expiry_blocked_at IS NOT NULL AND data_armed_at IS NULL AND bytes_sent=0 AND health_outcome='IGNORED'`, id)
	assertCount(t, f, 1, `SELECT count(*) FROM attempt_recipients ar JOIN recipients r ON r.id=ar.recipient_id WHERE r.message_id=$1 AND r.address='one@example.test' AND ar.status='TEMP_FAILED' AND ar.protocol_stage='EXPIRY_BLOCKED'`, id)
	var aid string
	if err := f.db.QueryRow(`SELECT id FROM delivery_attempts WHERE message_id=$1`, id).Scan(&aid); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE delivery_attempts SET data_armed_at=clock_timestamp() WHERE id=$1`, aid); err == nil {
		t.Fatal("expiry and DATA authorization coexist")
	}
	noWork(t, f)
}
func TestMessageExpiryAfterAuthorizationRetainsResult(t *testing.T) {
	for _, drop := range []bool{false, true} {
		t.Run(fmt.Sprint(drop), func(t *testing.T) {
			f := delivery(t, testsmtp.Options{DropFinal: drop}, 1)
			_, key, r := sendFixture(t, f)
			at := soonExpiry(t, f)
			id := expiringSeed(t, f, r, key, at, "one@example.test")
			f.worker.Sender = suppressionSender{f.worker.Sender, func(ctx context.Context, gate func(context.Context) error) error {
				if err := gate(ctx); err != nil {
					return err
				}
				waitExpiry(t, f, at)
				return nil
			}}
			runOne(t, f.worker)
			want := smtpclient.Accepted
			if drop {
				want = smtpclient.Unknown
			}
			if f.status(t, id) != want {
				t.Fatal("expiry erased acceptance evidence", f.status(t, id))
			}
			if err := f.worker.Repo.SweepExpired(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertCount(t, f, 0, `SELECT count(*) FROM events WHERE message_id=$1 AND event_type='RECIPIENT_EXPIRED'`, id)
			if drop {
				var rid, aid string
				if err := f.db.QueryRow(`SELECT r.id,a.id FROM recipients r JOIN delivery_attempts a ON a.message_id=r.message_id WHERE r.message_id=$1`, id).Scan(&rid, &aid); err != nil {
					t.Fatal(err)
				}
				_, err := f.worker.Repo.ResolveUnknown(context.Background(), queue.Resolution{RecipientID: rid, ExpectedAttempt: aid, Action: "retry", Actor: "test", Reason: "expiry", AcknowledgeDuplicate: true})
				if !errors.Is(err, smtpclient.ErrExpired) {
					t.Fatal("expired unknown retried", err)
				}
				if f.status(t, id) != smtpclient.Unknown {
					t.Fatal("rejected resolution erased UNKNOWN")
				}
			}
			if got := <-f.fake.Payloads; string(got) != f.raw() {
				t.Fatal("MIME changed")
			}
		})
	}
}
func TestMessageExpiryLeaseRecovery(t *testing.T) {
	for _, armed := range []bool{false, true} {
		t.Run(fmt.Sprint(armed), func(t *testing.T) {
			f := delivery(t, testsmtp.Options{}, 1)
			_, key, r := sendFixture(t, f)
			at := soonExpiry(t, f)
			id := expiringSeed(t, f, r, key, at, "one@example.test")
			j, err := f.worker.Repo.Claim(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if armed {
				if err = f.worker.Repo.ArmData(context.Background(), j); err != nil {
					t.Fatal(err)
				}
			}
			waitExpiry(t, f, at)
			if !armed {
				if _, err = f.db.Exec(`UPDATE delivery_attempts SET data_armed_at=clock_timestamp() WHERE id=$1`, j.AttemptID); err == nil {
					t.Fatal("database authorized expired DATA without gate")
				}
			}
			mustExec(t, f, `UPDATE messages SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, id)
			if err = f.worker.Repo.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := "EXPIRED"
			if armed {
				want = smtpclient.Unknown
			}
			if f.status(t, id) != want {
				t.Fatal(f.status(t, id), want)
			}
			noWork(t, f)
			assertCount(t, f, 1, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1`, id)
		})
	}
}
func TestMessageExpiryPartialQuantum(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, r := sendFixture(t, f)
	at := soonExpiry(t, f)
	addresses := make([]string, 20)
	for i := range addresses {
		addresses[i] = fmt.Sprintf("user%d@example.test", i)
	}
	id := expiringSeed(t, f, r, key, at, addresses...)
	runOne(t, f.worker)
	waitExpiry(t, f, at)
	noWork(t, f)
	if f.status(t, id) != "PARTIAL_ACCEPTED" {
		t.Fatal(f.status(t, id))
	}
	assertCount(t, f, 16, `SELECT count(*) FROM recipients WHERE message_id=$1 AND status='SMTP_ACCEPTED'`, id)
	assertCount(t, f, 4, `SELECT count(*) FROM recipients WHERE message_id=$1 AND status='EXPIRED'`, id)
	assertCount(t, f, 1, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1`, id)
}

func TestMessageExpiryGateChecksClockAfterLockWait(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, r := sendFixture(t, f)
	at := soonExpiry(t, f)
	id := expiringSeed(t, f, r, key, at, "one@example.test")
	j, err := f.worker.Repo.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := f.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err = blocker.Exec(`SELECT id FROM messages WHERE id=$1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	var blockerPID int
	if err = blocker.QueryRow(`SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.worker.Repo.ArmData(context.Background(), j) }()
	// ArmData began before the deadline, but message serialization holds it back.
	deadline := time.Now().Add(time.Second)
	for {
		var n int
		if err = f.db.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))`, blockerPID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gate never waited on message fence")
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitExpiry(t, f, at)
	if err = blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if !errors.Is(err, smtpclient.ErrExpired) {
			t.Fatal("transaction-start clock authorized expired DATA", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("gate stuck")
	}
	assertCount(t, f, 1, `SELECT count(*) FROM delivery_attempts WHERE id=$1 AND expiry_blocked_at IS NOT NULL AND data_armed_at IS NULL`, j.AttemptID)
	// A contradictory result cannot clear the fence or manufacture acceptance.
	now := time.Now().UTC()
	out := smtpclient.Result{Status: smtpclient.Accepted, StartedAt: now, FinishedAt: now, DataStartedAt: now, FinalResponseAt: now, Code: 250, Recipients: []smtpclient.RecipientResult{{ID: j.Recipients[0].ID, Status: smtpclient.Accepted}}}
	if err = f.worker.Repo.Finish(context.Background(), j, out); err == nil {
		t.Fatal("contradictory expiry finish accepted")
	}
	assertCount(t, f, 1, `SELECT count(*) FROM delivery_attempts WHERE id=$1 AND result='IN_PROGRESS'`, j.AttemptID)
	mustExec(t, f, `UPDATE messages SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, id)
	if err = f.worker.Repo.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.status(t, id) != "EXPIRED" {
		t.Fatal("blocked gate recovery", f.status(t, id))
	}
}
func TestMessageExpirySweepRollback(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, r := sendFixture(t, f)
	at := soonExpiry(t, f)
	id := expiringSeed(t, f, r, key, at, "one@example.test")
	waitExpiry(t, f, at)
	mustExec(t, f, `CREATE FUNCTION fail_expiry_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event_type='RECIPIENT_EXPIRED' THEN RAISE EXCEPTION 'injected expiry audit failure'; END IF; RETURN NEW; END; $$; CREATE TRIGGER fail_expiry_event BEFORE INSERT ON events FOR EACH ROW EXECUTE FUNCTION fail_expiry_event()`)
	err := f.worker.Repo.SweepExpired(context.Background())
	mustExec(t, f, `DROP TRIGGER fail_expiry_event ON events; DROP FUNCTION fail_expiry_event()`)
	if err == nil {
		t.Fatal("missing audit committed expiry")
	}
	if f.status(t, id) != "QUEUED" || recipientStatus(t, f, id, "one@example.test") != "QUEUED" {
		t.Fatal("partial expiry commit")
	}
	assertCount(t, f, 0, `SELECT count(*) FROM events WHERE message_id=$1 AND event_type='RECIPIENT_EXPIRED'`, id)
	noWork(t, f)
	if f.status(t, id) != "EXPIRED" {
		t.Fatal(f.status(t, id))
	}
}
func TestMessageExpiryStopsScheduledRetry(t *testing.T) {
	f := delivery(t, testsmtp.Options{DataCode: 451}, 1)
	_, key, r := sendFixture(t, f)
	f.worker.Repo.RetryEnabled = true
	at := soonExpiry(t, f)
	id := expiringSeed(t, f, r, key, at, "one@example.test")
	runOne(t, f.worker)
	assertCount(t, f, 1, `SELECT count(*) FROM recipients WHERE message_id=$1 AND retry_at IS NOT NULL`, id)
	waitExpiry(t, f, at)
	noWork(t, f)
	if f.status(t, id) != "EXPIRED" {
		t.Fatal(f.status(t, id))
	}
	assertCount(t, f, 0, `SELECT count(*) FROM recipients WHERE message_id=$1 AND (retry_at IS NOT NULL OR retry_automatic)`, id)
	assertCount(t, f, 1, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1`, id)
}

func TestMessageExpiryManualRetryRetainsHistoricalUnknown(t *testing.T) {
	f := delivery(t, testsmtp.Options{DropFinal: true}, 1)
	_, key, r := sendFixture(t, f)
	at := soonExpiry(t, f)
	id := expiringSeed(t, f, r, key, at, "one@example.test")
	runOne(t, f.worker)
	var rid, aid string
	if err := f.db.QueryRow(`SELECT r.id,a.id FROM recipients r JOIN delivery_attempts a ON a.message_id=r.message_id WHERE r.message_id=$1`, id).Scan(&rid, &aid); err != nil {
		t.Fatal(err)
	}
	_, err := f.worker.Repo.ResolveUnknown(context.Background(), queue.Resolution{RecipientID: rid, ExpectedAttempt: aid, Action: "retry", Actor: "test", Reason: "verified duplicate risk", AcknowledgeDuplicate: true})
	if err != nil {
		t.Fatal(err)
	}
	waitExpiry(t, f, at)
	noWork(t, f)
	if f.status(t, id) != "EXPIRED" {
		t.Fatal(f.status(t, id))
	}
	assertCount(t, f, 1, `SELECT count(*) FROM recipients WHERE id=$1 AND decision->>'may_have_delivered'='true'`, rid)
	assertCount(t, f, 1, `SELECT count(*) FROM events WHERE recipient_id=$1 AND event_type='RECIPIENT_EXPIRED' AND data->>'may_have_delivered'='true'`, rid)
	assertCount(t, f, 1, `SELECT count(*) FROM attempt_recipients WHERE recipient_id=$1 AND status='DELIVERY_UNKNOWN'`, rid)
	report, err := operations.Cleanup(context.Background(), f.db, f.root, operations.Retention{EMLDays: 1, Batch: 1000, Now: time.Now().Add(48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range report.Archives {
		if item.ID == id {
			t.Fatal("expiry made historical UNKNOWN purgeable")
		}
	}

}

func TestMessageExpiryAdmissionChecksAfterBudgetLock(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, r := sendFixture(t, f)
	at := soonExpiry(t, f)
	blocker, err := f.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err = blocker.Exec(`SELECT id FROM api_keys WHERE id=$1 FOR NO KEY UPDATE`, key.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, e := r.ReceiveExpiring(context.Background(), message.Envelope{Account: auth.Account{AllowedFrom: key.AllowedFrom}, From: key.AllowedFrom[0], Recipients: []string{"one@example.test"}}, strings.NewReader(f.raw()), key.ID, "lock-expiry", "normal", at)
		done <- e
	}()
	deadline := time.Now().Add(time.Second)
	for {
		var n int
		if err = f.db.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE 'SELECT submission_minute_limit,%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("admission never waited on identity lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitExpiry(t, f, at)
	if err = blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if !errors.Is(err, message.ErrExpiredSubmission) {
			t.Fatal("expired admission succeeded after lock wait", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("admission stuck")
	}
	if emlCount(t, f.root) != 0 {
		t.Fatal("expired locked request published archive")
	}
	assertCount(t, f, 0, `SELECT count(*) FROM api_submission_ledger WHERE api_key_id=$1`, key.ID)
	assertCount(t, f, 0, `SELECT count(*) FROM api_submission_quota WHERE api_key_id=$1`, key.ID)
}

func TestMessageExpiryPendingRecipientsBesideUnknown(t *testing.T) {
	f := delivery(t, testsmtp.Options{DropFinal: true}, 1)
	_, key, r := sendFixture(t, f)
	at := soonExpiry(t, f)
	addresses := make([]string, 20)
	for i := range addresses {
		addresses[i] = fmt.Sprintf("user%d@example.test", i)
	}
	id := expiringSeed(t, f, r, key, at, addresses...)
	runOne(t, f.worker)
	waitExpiry(t, f, at)
	noWork(t, f)
	if f.status(t, id) != smtpclient.Unknown {
		t.Fatal("pending expiry erased UNKNOWN", f.status(t, id))
	}
	assertCount(t, f, 16, `SELECT count(*) FROM recipients WHERE message_id=$1 AND status='DELIVERY_UNKNOWN'`, id)
	assertCount(t, f, 4, `SELECT count(*) FROM recipients WHERE message_id=$1 AND status='EXPIRED'`, id)
	assertCount(t, f, 1, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1`, id)
}
