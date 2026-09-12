package integration

import (
	"context"
	"errors"
	"sync"
	"testing"

	"relaytale/internal/queue"
	"relaytale/internal/smtpclient"
	"relaytale/internal/testsmtp"
)

func providerID(t *testing.T, f *deliveryFixture) string {
	t.Helper()
	var id string
	if err := f.db.QueryRow(`SELECT id FROM providers WHERE $1=ANY(from_domains) ORDER BY priority LIMIT 1`, f.domain).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
func circuit(t *testing.T, f *deliveryFixture, pid string) string {
	t.Helper()
	var state string
	if err := f.db.QueryRow(`SELECT circuit_state FROM provider_health WHERE provider_id=$1`, pid).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}
func tripCircuit(t *testing.T, f *deliveryFixture) string {
	t.Helper()
	f.worker.Repo.HealthEnabled = true
	for i := 0; i < 5; i++ {
		f.seed(t)
		runOne(t, f.worker)
	}
	pid := providerID(t, f)
	if circuit(t, f, pid) != "OPEN" {
		t.Fatal("circuit did not open")
	}
	return pid
}
func noWork(t *testing.T, f *deliveryFixture) {
	t.Helper()
	if worked, err := f.worker.RunOne(context.Background()); worked || err != nil {
		t.Fatal("unexpected work", err)
	}
}

func TestQuotaSplitsRecipientsAndUsesRollingWindows(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 2)
	pid := providerID(t, f)
	mustExec(t, f, `UPDATE providers SET hourly_limit=1,daily_limit=2 WHERE id=$1`, pid)
	id := f.seed(t)
	runOne(t, f.worker)
	first := <-f.fake.Envelopes
	if len(first) != 1 || f.status(t, id) != "QUEUED" {
		t.Fatal("quota did not split")
	}
	noWork(t, f)
	mustExec(t, f, `UPDATE provider_quota SET reserved_at=clock_timestamp()-interval '61 minutes' WHERE provider_id=$1`, pid)
	runOne(t, f.worker)
	second := <-f.fake.Envelopes
	if len(second) != 1 || second[0] == first[0] || f.status(t, id) != smtpclient.Accepted {
		t.Fatal("duplicate or incomplete split")
	}
	f.seed(t)
	mustExec(t, f, `UPDATE provider_quota SET reserved_at=clock_timestamp()-interval '61 minutes' WHERE provider_id=$1`, pid)
	noWork(t, f) // hourly released, daily still full
	mustExec(t, f, `UPDATE provider_quota SET reserved_at=clock_timestamp()-interval '25 hours' WHERE provider_id=$1`, pid)
	runOne(t, f.worker)
}

func TestQuotaConcurrentClaimsAndCrashNoRefund(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 8)
	pid := providerID(t, f)
	mustExec(t, f, `UPDATE providers SET hourly_limit=3 WHERE id=$1`, pid)
	for i := 0; i < 4; i++ {
		f.seed(t)
	}
	var wg sync.WaitGroup
	ch := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := f.worker.Repo.Claim(context.Background()); ch <- err }()
	}
	wg.Wait()
	close(ch)
	for err := range ch {
		if err != nil && !errors.Is(err, queue.ErrNoJob) {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := f.worker.Repo.Claim(context.Background()); err != nil && !errors.Is(err, queue.ErrNoJob) {
			t.Fatal(err)
		}
	}
	var units int
	if err := f.db.QueryRow(`SELECT sum(units) FROM provider_quota WHERE provider_id=$1`, pid).Scan(&units); err != nil || units != 3 {
		t.Fatal("quota oversubscribed", units, err)
	}
	mustExec(t, f, `UPDATE messages SET lease_expires_at=now()-interval '1 second' WHERE smtp_account_id=$1 AND status='SENDING'`, f.accountID)
	if err := f.worker.Repo.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	noWork(t, f)
}

func TestQuotaReservationRollback(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	pid := providerID(t, f)
	id := f.seed(t)
	mustExec(t, f, `CREATE FUNCTION fail_quota_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event_type='QUOTA_RESERVED' THEN RAISE EXCEPTION 'injected'; END IF; RETURN NEW; END; $$; CREATE TRIGGER fail_quota_event BEFORE INSERT ON events FOR EACH ROW EXECUTE FUNCTION fail_quota_event();`)
	_, err := f.worker.Repo.Claim(context.Background())
	mustExec(t, f, `DROP TRIGGER fail_quota_event ON events; DROP FUNCTION fail_quota_event();`)
	if err == nil {
		t.Fatal("reservation failure ignored")
	}
	var n int
	if err := f.db.QueryRow(`SELECT count(*) FROM provider_quota WHERE provider_id=$1`, pid).Scan(&n); err != nil || n != 0 {
		t.Fatal("quota leaked", err)
	}
	if f.status(t, id) != "QUEUED" {
		t.Fatal("claim leaked")
	}
	runOne(t, f.worker)
}

func TestCircuitHalfOpenSuccessAndFailure(t *testing.T) {
	for _, success := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed", true: "recovered"}[success], func(t *testing.T) {
			f := delivery(t, testsmtp.Options{DataCode: 451}, 4)
			pid := tripCircuit(t, f)
			f.seed(t)
			noWork(t, f)
			if success {
				b := testsmtp.Start(t, testsmtp.Options{})
				bid := f.addProvider(t, b, 20, 1)
				mustExec(t, f, `UPDATE providers p SET host=b.host,port=b.port FROM providers b WHERE p.id=$1 AND b.id=$2`, pid, bid)
				mustExec(t, f, `UPDATE providers SET enabled=false WHERE id=$1`, bid)
				f.worker.Sender = smtpclient.Client{Domain: "relaytale.test", RootCAs: b.Roots}
			}
			mustExec(t, f, `UPDATE provider_health SET open_until=now()-interval '1 second' WHERE provider_id=$1`, pid)
			runOne(t, f.worker)
			want := "OPEN"
			if success {
				want = "CLOSED"
			}
			if circuit(t, f, pid) != want {
				t.Fatal("half-open transition")
			}
			if success {
				f.seed(t)
				runOne(t, f.worker)
				if circuit(t, f, pid) != "CLOSED" {
					t.Fatal("old failures reopened circuit")
				}
			}
		})
	}
}

func TestCircuitSingleProbeAndCrashRecovery(t *testing.T) {
	f := delivery(t, testsmtp.Options{DataCode: 451}, 8)
	pid := tripCircuit(t, f)
	for i := 0; i < 4; i++ {
		f.seed(t)
	}
	mustExec(t, f, `UPDATE provider_health SET open_until=now()-interval '1 second' WHERE provider_id=$1`, pid)
	ch := make(chan queue.Job, 8)
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, err := f.worker.Repo.Claim(context.Background())
			if err == nil {
				ch <- j
			} else {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(ch)
	close(errs)
	for err := range errs {
		if !errors.Is(err, queue.ErrNoJob) {
			t.Fatal(err)
		}
	}
	var probe queue.Job
	count := 0
	for j := range ch {
		probe = j
		count++
	}
	if count != 1 || circuit(t, f, pid) != "HALF_OPEN" {
		t.Fatal("multiple probes", count)
	}
	// An armed probe must become UNKNOWN while the circuit gets another cooldown.
	if err := f.worker.Repo.ArmData(context.Background(), probe); err != nil {
		t.Fatal(err)
	}
	f.expire(t, probe.ID)
	if err := f.worker.Repo.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if circuit(t, f, pid) != "OPEN" || f.status(t, probe.ID) != smtpclient.Unknown {
		t.Fatal("unsafe probe recovery")
	}
	if err := f.worker.Repo.Finish(context.Background(), probe, smtpclient.Result{}); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatal("stale completion", err)
	}
	noWork(t, f)
}

func TestCircuitIgnoresRecipientAndConfigurationErrors(t *testing.T) {
	for _, o := range []testsmtp.Options{{RejectAuth: true}, {RejectRecipients: map[string]int{"one@example.test": 550, "two@example.test": 450}}} {
		f := delivery(t, o, 1)
		f.worker.Repo.HealthEnabled = true
		for i := 0; i < 5; i++ {
			f.seed(t)
			runOne(t, f.worker)
		}
		if circuit(t, f, providerID(t, f)) != "CLOSED" {
			t.Fatal("recipient or configuration counted against availability")
		}
	}
}

func TestCircuitOptOutAndWindow(t *testing.T) {
	f := delivery(t, testsmtp.Options{DataCode: 451}, 1)
	pid := providerID(t, f)
	for i := 0; i < 5; i++ {
		f.seed(t)
		runOne(t, f.worker)
	} // observed, not enforced
	if circuit(t, f, pid) != "CLOSED" {
		t.Fatal("default breaker enabled")
	}
	mustExec(t, f, `UPDATE delivery_attempts SET health_observed_at=clock_timestamp()-interval '11 minutes' WHERE provider_id=$1`, pid)
	f.worker.Repo.HealthEnabled = true
	f.seed(t)
	runOne(t, f.worker)
	if circuit(t, f, pid) != "CLOSED" {
		t.Fatal("expired samples counted")
	}
	mustExec(t, f, `UPDATE providers SET health_check_enabled=false WHERE id=$1`, pid)
	for i := 0; i < 5; i++ {
		f.seed(t)
		runOne(t, f.worker)
	}
	if circuit(t, f, pid) != "CLOSED" {
		t.Fatal("provider opt-out ignored")
	}
}

func TestUnknownStillConsumesQuota(t *testing.T) {
	f := delivery(t, testsmtp.Options{DropFinal: true}, 1)
	pid := providerID(t, f)
	mustExec(t, f, `UPDATE providers SET daily_limit=2 WHERE id=$1`, pid)
	id := f.seed(t)
	runOne(t, f.worker)
	if f.status(t, id) != smtpclient.Unknown {
		t.Fatal("unknown lost")
	}
	f.seed(t)
	noWork(t, f)
}

func TestQuotaSplitFailoverAuditsEveryRecipient(t *testing.T) {
	f := deliveryFixtureRetry(t, testsmtp.Options{DataCode: 451})
	f.worker.Repo.FailoverEnabled = true
	id := f.seed(t)
	runOne(t, f.worker)
	b := testsmtp.Start(t, testsmtp.Options{})
	bid := f.addProvider(t, b, 20, 1)
	mustExec(t, f, `UPDATE providers SET hourly_limit=1 WHERE id=$1`, bid)
	dueFailover(t, f, id)
	f.worker.Sender = smtpclient.Client{Domain: "relaytale.test", RootCAs: b.Roots}
	runOne(t, f.worker)
	mustExec(t, f, `UPDATE provider_quota SET reserved_at=clock_timestamp()-interval '61 minutes' WHERE provider_id=$1`, bid)
	runOne(t, f.worker)
	var n int
	if err := f.db.QueryRow(`SELECT count(*) FROM events WHERE message_id=$1 AND event_type='PROVIDER_FAILOVER'`, id).Scan(&n); err != nil || n != 2 {
		t.Fatal("missing split failover audit", n, err)
	}
	if f.status(t, id) != smtpclient.Accepted {
		t.Fatal("split failover incomplete")
	}
}

func TestCircuitTransitionRollback(t *testing.T) {
	f := delivery(t, testsmtp.Options{DataCode: 451}, 1)
	f.worker.Repo.HealthEnabled = true
	pid := providerID(t, f)
	for i := 0; i < 4; i++ {
		f.seed(t)
		runOne(t, f.worker)
	}
	id := f.seed(t)
	mustExec(t, f, `CREATE FUNCTION fail_circuit_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event_type='PROVIDER_CIRCUIT_OPEN' THEN RAISE EXCEPTION 'injected'; END IF; RETURN NEW; END; $$; CREATE TRIGGER fail_circuit_event BEFORE INSERT ON events FOR EACH ROW EXECUTE FUNCTION fail_circuit_event();`)
	worked, err := f.worker.RunOne(context.Background())
	mustExec(t, f, `DROP TRIGGER fail_circuit_event ON events; DROP FUNCTION fail_circuit_event();`)
	if !worked || err == nil {
		t.Fatal("circuit event failure ignored")
	}
	if circuit(t, f, pid) != "CLOSED" || f.status(t, id) != "SENDING" {
		t.Fatal("partial health commit")
	}
	f.expire(t, id)
	if err := f.worker.Repo.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.status(t, id) != smtpclient.Unknown {
		t.Fatal("armed failed commit resent")
	}
}

func TestCircuitStillRequiresSafeFailover(t *testing.T) {
	f := deliveryFixtureRetry(t, testsmtp.Options{DataCode: 451})
	f.worker.Repo.FailoverEnabled = true
	pid := tripCircuit(t, f)
	var id string
	if err := f.db.QueryRow(`SELECT id FROM messages WHERE smtp_account_id=$1 ORDER BY created_at DESC LIMIT 1`, f.accountID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	b := testsmtp.Start(t, testsmtp.Options{})
	f.addProvider(t, b, 20, 1)
	dueFailover(t, f, id)
	f.worker.Sender = smtpclient.Client{Domain: "relaytale.test", RootCAs: b.Roots}
	runOne(t, f.worker)
	if f.status(t, id) != smtpclient.Accepted || circuit(t, f, pid) != "OPEN" {
		t.Fatal("safe failover or isolation lost")
	}
}
