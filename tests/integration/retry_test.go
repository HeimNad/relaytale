package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	decision "mailgateway/internal/delivery"
	"mailgateway/internal/queue"
	"mailgateway/internal/smtpclient"
	"mailgateway/internal/testsmtp"
)

func TestRetryOnlyRejectedRecipient(t *testing.T) {
	f := deliveryFixtureRetry(t, testsmtp.Options{RecipientCode: func(n int, address string) int {
		if n == 1 && address == "two@example.test" {
			return 450
		}
		return 0
	}})
	id := f.seed(t)
	runOne(t, f.worker)
	if f.status(t, id) != smtpclient.Partial {
		t.Fatal("lost partial result")
	}
	first := <-f.fake.Envelopes
	if len(first) != 1 || first[0] != "one@example.test" {
		t.Fatal(first)
	}
	if got := string(<-f.fake.Payloads); got != f.raw() {
		t.Fatal("original altered")
	}
	if worked, err := f.worker.RunOne(context.Background()); worked || err != nil {
		t.Fatal("retry before deadline", err)
	}
	// A higher-priority backup appearing later must not attract a retry.
	backup := testsmtp.Start(t, testsmtp.Options{})
	f.addProvider(t, backup, 1, 1)
	var rid string
	if err := f.db.QueryRow(`SELECT id FROM recipients WHERE message_id=$1 AND address='two@example.test'`, id).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE recipients SET retry_at=now()-interval '1 second' WHERE id=$1`, rid); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- f.worker.Repo.Schedule(context.Background()) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	// Construct a fresh repository/worker to demonstrate no in-memory retry state is needed.
	fresh := f.worker
	fresh.Repo = queue.Repository{DB: f.db, RetryEnabled: true}
	runOne(t, fresh)
	second := <-f.fake.Envelopes
	if len(second) != 1 || second[0] != "two@example.test" {
		t.Fatal("resent accepted recipient", second)
	}
	if got := string(<-f.fake.Payloads); got != f.raw() {
		t.Fatal("retry MIME changed")
	}
	if backup.Connections.Load() != 0 || f.status(t, id) != smtpclient.Accepted {
		t.Fatal("route or aggregate incorrect")
	}
	var n int
	for _, q := range []string{`SELECT count(*) FROM attempt_recipients ar JOIN delivery_attempts a ON a.id=ar.attempt_id WHERE a.message_id=$1 AND a.attempt_number=2`, `SELECT count(*) FROM events WHERE message_id=$1 AND event_type='RETRY_QUEUED'`} {
		if err := f.db.QueryRow(q, id).Scan(&n); err != nil || n != 1 {
			t.Fatal("duplicate schedule or recipients", n, err)
		}
	}
}
func deliveryFixtureRetry(t *testing.T, o testsmtp.Options) *deliveryFixture {
	t.Helper()
	f := delivery(t, o, 1)
	f.worker.Repo.RetryEnabled = true
	return f
}

func TestRetryBudgetAndDisable(t *testing.T) {
	for _, kind := range []string{"attempts", "window", "disabled"} {
		t.Run(kind, func(t *testing.T) {
			f := deliveryFixtureRetry(t, testsmtp.Options{FinalCode: 451})
			id := f.seed(t)
			runOne(t, f.worker)
			if _, err := f.db.Exec(`UPDATE recipients SET retry_at=now()-interval '1 second' WHERE message_id=$1`, id); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "attempts":
				f.db.Exec(`UPDATE recipients SET attempt_count=7 WHERE message_id=$1`, id)
			case "window":
				f.db.Exec(`UPDATE recipients SET retry_started_at=now()-interval '25 hours' WHERE message_id=$1`, id)
			case "disabled":
				if err := f.worker.Repo.Schedule(context.Background()); err != nil {
					t.Fatal(err)
				}
				f.worker.Repo.RetryEnabled = false
			}
			if worked, err := f.worker.RunOne(context.Background()); worked || err != nil {
				t.Fatal("unsafe retry", err)
			}
			if f.fake.Connections.Load() != 1 {
				t.Fatal("unexpected connection")
			}
			if kind != "disabled" {
				var raw []byte
				if err := f.db.QueryRow(`SELECT decision FROM recipients WHERE message_id=$1 LIMIT 1`, id).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var d decision.Decision
				if err := json.Unmarshal(raw, &d); err != nil || d.Action != decision.Manual || d.Terminal {
					t.Fatal(d, err)
				}
			}
		})
	}
}
func TestAuthRequiresIntervention(t *testing.T) {
	f := deliveryFixtureRetry(t, testsmtp.Options{RejectAuth: true})
	id := f.seed(t)
	runOne(t, f.worker)
	var pending int
	if err := f.db.QueryRow(`SELECT count(*) FROM recipients WHERE message_id=$1 AND retry_at IS NOT NULL`, id).Scan(&pending); err != nil || pending != 0 {
		t.Fatal("AUTH scheduled", err)
	}
	if worked, err := f.worker.RunOne(context.Background()); worked || err != nil {
		t.Fatal("AUTH retried", err)
	}
	var action string
	if err := f.db.QueryRow(`SELECT decision->>'action' FROM recipients WHERE message_id=$1 LIMIT 1`, id).Scan(&action); err != nil || action != string(decision.Manual) {
		t.Fatal(action, err)
	}
}
func TestUnknownResolutionDoesNotRewriteEvidence(t *testing.T) {
	f := deliveryFixtureRetry(t, testsmtp.Options{DropFinal: true})
	id := f.seed(t)
	runOne(t, f.worker)
	if worked, err := f.worker.RunOne(context.Background()); worked || err != nil {
		t.Fatal("UNKNOWN retried", err)
	}
	rows, err := f.db.Query(`SELECT r.id,a.id FROM recipients r JOIN attempt_recipients ar ON ar.recipient_id=r.id JOIN delivery_attempts a ON a.id=ar.attempt_id WHERE r.message_id=$1 ORDER BY r.id`, id)
	if err != nil {
		t.Fatal(err)
	}
	var resolutions []queue.Resolution
	for rows.Next() {
		v := queue.Resolution{Action: "mark-delivered", Actor: "integration", Reason: "operator verified recipient copy"}
		if err := rows.Scan(&v.RecipientID, &v.ExpectedAttempt); err != nil {
			t.Fatal(err)
		}
		resolutions = append(resolutions, v)
	}
	rows.Close()
	for _, v := range resolutions {
		if _, err := f.worker.Repo.ResolveUnknown(context.Background(), v); err != nil {
			t.Fatal(err)
		}
	}
	if f.status(t, id) != "DELIVERED" {
		t.Fatal("manual projection missing")
	}
	var original string
	if err := f.db.QueryRow(`SELECT result FROM delivery_attempts WHERE message_id=$1`, id).Scan(&original); err != nil || original != smtpclient.Unknown {
		t.Fatal("SMTP evidence rewritten", original, err)
	}
	if _, err := f.worker.Repo.ResolveUnknown(context.Background(), resolutions[0]); !errors.Is(err, queue.ErrStaleResolution) {
		t.Fatal("stale resolution accepted", err)
	}
	var n int
	if err := f.db.QueryRow(`SELECT count(*) FROM events WHERE message_id=$1 AND event_type='UNKNOWN_RESOLVED' AND source='operator'`, id).Scan(&n); err != nil || n != 2 {
		t.Fatal("missing audit", err)
	}
}
func TestManualRetryConcurrentAndAcknowledged(t *testing.T) {
	f := deliveryFixtureRetry(t, testsmtp.Options{DropFinal: true})
	id := f.seed(t)
	runOne(t, f.worker)
	var v queue.Resolution
	if err := f.db.QueryRow(`SELECT r.id,a.id FROM recipients r JOIN attempt_recipients ar ON ar.recipient_id=r.id JOIN delivery_attempts a ON a.id=ar.attempt_id WHERE r.message_id=$1 LIMIT 1`, id).Scan(&v.RecipientID, &v.ExpectedAttempt); err != nil {
		t.Fatal(err)
	}
	v.Actor = "integration"
	v.Reason = "explicit duplicate risk test"
	v.Action = "retry"
	if _, err := f.worker.Repo.ResolveUnknown(context.Background(), v); err == nil {
		t.Fatal("missing duplicate acknowledgement accepted")
	}
	v.AcknowledgeDuplicate = true
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, err := f.worker.Repo.ResolveUnknown(context.Background(), v); errs <- err }()
	}
	successes := 0
	for i := 0; i < 2; i++ {
		err := <-errs
		if err == nil {
			successes++
		} else if !errors.Is(err, queue.ErrStaleResolution) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatal("concurrent duplicate resolution", successes)
	}
	// Another recipient is still UNKNOWN, so no automatic or manual work may begin yet.
	if _, err := f.worker.Repo.Claim(context.Background()); !errors.Is(err, queue.ErrNoJob) {
		t.Fatal("unresolved message sent", err)
	}
	var other string
	if err := f.db.QueryRow(`SELECT id FROM recipients WHERE message_id=$1 AND status='DELIVERY_UNKNOWN'`, id).Scan(&other); err != nil {
		t.Fatal(err)
	}
	v.RecipientID = other
	v.Action = "mark-failed"
	if _, err := f.worker.Repo.ResolveUnknown(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	j, err := f.worker.Repo.Claim(context.Background())
	if err != nil || len(j.Recipients) != 1 {
		t.Fatal("manual retry claim", j, err)
	}
}

// A stopped process must not bypass retry expiry merely because the scheduler already queued it.
func TestQueuedRetryExpiresBeforeClaim(t *testing.T) {
	f := deliveryFixtureRetry(t, testsmtp.Options{FinalCode: 451})
	id := f.seed(t)
	runOne(t, f.worker)
	if _, err := f.db.Exec(`UPDATE recipients SET retry_at=now()-interval '1 second' WHERE message_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := f.worker.Repo.Schedule(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE recipients SET retry_started_at=now()-interval '25 hours' WHERE message_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.worker.Repo.Claim(context.Background()); !errors.Is(err, queue.ErrNoJob) {
		t.Fatal("expired queued retry claimed", err)
	}
	if err := f.worker.Repo.Schedule(context.Background()); err != nil {
		t.Fatal(err)
	}
	var due sql.NullTime
	if err := f.db.QueryRow(`SELECT next_attempt_at FROM messages WHERE id=$1`, id).Scan(&due); err != nil || due.Valid {
		t.Fatal("expired plan remains", err)
	}
}

func TestPreDataRecoveryBudget(t *testing.T) {
	f := deliveryFixtureRetry(t, testsmtp.Options{})
	id := f.seed(t)
	if _, err := f.worker.Repo.Claim(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE recipients SET attempt_count=7 WHERE message_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	f.expire(t, id)
	if err := f.worker.Repo.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.status(t, id) != smtpclient.Temporary {
		t.Fatal("crash recovery exceeded budget")
	}
	if _, err := f.worker.Repo.Claim(context.Background()); !errors.Is(err, queue.ErrNoJob) {
		t.Fatal("exhausted recovery claimed", err)
	}
}

func TestRetryScheduleRollback(t *testing.T) {
	f := deliveryFixtureRetry(t, testsmtp.Options{FinalCode: 451})
	id := f.seed(t)
	runOne(t, f.worker)
	if _, err := f.db.Exec(`UPDATE recipients SET retry_at=now()-interval '1 second' WHERE message_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	_, err := f.db.Exec(`CREATE FUNCTION fail_retry_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event_type='RETRY_QUEUED' THEN RAISE EXCEPTION 'injected scheduler failure'; END IF; RETURN NEW; END; $$; CREATE TRIGGER fail_retry_event BEFORE INSERT ON events FOR EACH ROW EXECUTE FUNCTION fail_retry_event();`)
	if err != nil {
		t.Fatal(err)
	}
	defer f.db.Exec(`DROP TRIGGER fail_retry_event ON events; DROP FUNCTION fail_retry_event();`)
	if err := f.worker.Repo.Schedule(context.Background()); err == nil {
		t.Fatal("injected failure ignored")
	}
	var n int
	if err := f.db.QueryRow(`SELECT count(*) FROM recipients WHERE message_id=$1 AND status='TEMP_FAILED' AND retry_at IS NOT NULL`, id).Scan(&n); err != nil || n != 2 {
		t.Fatal("partial schedule commit", n, err)
	}
	if f.status(t, id) != smtpclient.Temporary {
		t.Fatal("message projection escaped transaction")
	}
}
