package integration

import (
	"context"
	"errors"
	"sync"
	"testing"

	"mailgateway/internal/queue"
	"mailgateway/internal/smtpclient"
	"mailgateway/internal/testsmtp"
)

func dueFailover(t *testing.T, f *deliveryFixture, id string) {
	t.Helper()
	if _, err := f.db.Exec(`UPDATE recipients SET retry_at=now()-interval '1 second' WHERE message_id=$1 AND retry_at IS NOT NULL`, id); err != nil {
		t.Fatal(err)
	}
}

func TestSafeFailover(t *testing.T) {
	for _, failure := range []string{"data451", "connect", "tls"} {
		t.Run(failure, func(t *testing.T) {
			f := deliveryFixtureRetry(t, testsmtp.Options{DataCode: 451})
			f.worker.Repo.FailoverEnabled = true
			id := f.seed(t)
			if failure == "connect" {
				// Port zero cannot be stored; a freshly closed local listener's port gives refusal.
				f.fake.CloseListener()
			}
			if failure == "tls" {
				f.worker.Sender = smtpclient.Client{Domain: "gateway.test"}
			}
			runOne(t, f.worker)
			backup := testsmtp.Start(t, testsmtp.Options{})
			bid := f.addProvider(t, backup, 20, 1)
			dueFailover(t, f, id)
			fresh := f.worker
			fresh.Repo = queue.Repository{DB: f.db, RetryEnabled: true, FailoverEnabled: true}
			fresh.Sender = smtpclient.Client{Domain: "gateway.test", RootCAs: backup.Roots}
			runOne(t, fresh)
			if f.status(t, id) != smtpclient.Accepted {
				t.Fatal("backup did not accept")
			}
			select {
			case got := <-backup.Payloads:
				if string(got) != f.raw() {
					t.Fatal("MIME changed")
				}
			default:
				t.Fatal("missing backup payload")
			}
			var route string
			var n int
			if err := f.db.QueryRow(`SELECT route_provider_id FROM messages WHERE id=$1`, id).Scan(&route); err != nil || route != bid {
				t.Fatal(route, err)
			}
			if err := f.db.QueryRow(`SELECT count(*) FROM events WHERE message_id=$1 AND event_type='PROVIDER_FAILOVER'`, id).Scan(&n); err != nil || n != 2 {
				t.Fatal(n, err)
			}
		})
	}
}

func TestFailoverDoesNotBypassUnsafeEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    testsmtp.Options
	}{
		{"auth", testsmtp.Options{RejectAuth: true}}, {"unknown", testsmtp.Options{DropFinal: true}},
		{"final451", testsmtp.Options{FinalCode: 451}}, {"rcpt450", testsmtp.Options{RejectRecipients: map[string]int{"two@example.test": 450}}},
		{"mixedData", testsmtp.Options{DataCode: 451, RejectRecipients: map[string]int{"two@example.test": 450}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := deliveryFixtureRetry(t, tc.o)
			f.worker.Repo.FailoverEnabled = true
			id := f.seed(t)
			runOne(t, f.worker)
			backup := testsmtp.Start(t, testsmtp.Options{})
			bid := f.addProvider(t, backup, 1, 1)
			dueFailover(t, f, id)
			if err := f.worker.Repo.Schedule(context.Background()); err != nil {
				t.Fatal(err)
			}
			j, err := f.worker.Repo.Claim(context.Background())
			if tc.name == "auth" || tc.name == "unknown" {
				if !errors.Is(err, queue.ErrNoJob) {
					t.Fatal("unsafe claim", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if j.Provider.ID == bid {
					t.Fatal("unsafe switch")
				}
			}
			if backup.Connections.Load() != 0 {
				t.Fatal("backup contacted")
			}
		})
	}
}

func TestFailoverEligibilityAndOptIn(t *testing.T) {
	for _, kind := range []string{"disabled", "retryDisabled", "domain", "headerDomain", "quota", "providerDisabled", "capacity", "budget"} {
		t.Run(kind, func(t *testing.T) {
			f := deliveryFixtureRetry(t, testsmtp.Options{DataCode: 451})
			f.worker.Repo.FailoverEnabled = true
			id := f.seed(t)
			runOne(t, f.worker)
			b := testsmtp.Start(t, testsmtp.Options{})
			bid := f.addProvider(t, b, 1, 1)
			switch kind {
			case "disabled":
				f.worker.Repo.FailoverEnabled = false
			case "retryDisabled":
				f.worker.Repo.RetryEnabled = false
			case "domain":
				mustExec(t, f, `UPDATE providers SET from_domains=ARRAY['other.test'] WHERE id=$1`, bid)
			case "headerDomain":
				mustExec(t, f, `UPDATE messages SET header_from='other@other.test' WHERE id=$1`, id)
			case "quota":
				mustExec(t, f, `UPDATE providers SET hourly_limit=1 WHERE id=$1`, bid)
				busy := f.seed(t)
				mustExec(t, f, `UPDATE messages SET route_provider_id=$2 WHERE id=$1`, busy, bid)
				if _, err := f.worker.Repo.Claim(context.Background()); err != nil {
					t.Fatal(err)
				}
			case "providerDisabled":
				mustExec(t, f, `UPDATE providers SET enabled=false WHERE id=$1`, bid)
			case "budget":
				mustExec(t, f, `UPDATE recipients SET attempt_count=7 WHERE message_id=$1`, id)
			case "capacity":
				busy := f.seed(t)
				mustExec(t, f, `UPDATE messages SET route_provider_id=$2 WHERE id=$1`, busy, bid)
				if _, err := f.worker.Repo.Claim(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			dueFailover(t, f, id)
			if err := f.worker.Repo.Schedule(context.Background()); err != nil {
				t.Fatal(err)
			}
			j, err := f.worker.Repo.Claim(context.Background())
			if err != nil && !errors.Is(err, queue.ErrNoJob) {
				t.Fatal(err)
			}
			if err == nil && j.Provider.ID == bid {
				t.Fatal("ineligible backup selected")
			}
		})
	}
}
func mustExec(t *testing.T, f *deliveryFixture, q string, args ...any) {
	t.Helper()
	if _, err := f.db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func TestFailoverPreservesAcceptedRecipients(t *testing.T) {
	f := deliveryFixtureRetry(t, testsmtp.Options{RejectRecipients: map[string]int{"two@example.test": 450}})
	f.worker.Repo.FailoverEnabled = true
	id := f.seed(t)
	runOne(t, f.worker)
	<-f.fake.Payloads
	<-f.fake.Envelopes
	f.fake.CloseListener()
	dueFailover(t, f, id)
	runOne(t, f.worker) // Only the rejected recipient hits connect failure.
	b := testsmtp.Start(t, testsmtp.Options{})
	f.addProvider(t, b, 20, 1)
	dueFailover(t, f, id)
	f.worker.Sender = smtpclient.Client{Domain: "gateway.test", RootCAs: b.Roots}
	runOne(t, f.worker)
	select {
	case rc := <-b.Envelopes:
		if len(rc) != 1 || rc[0] != "two@example.test" {
			t.Fatal("duplicate accepted recipient", rc)
		}
	default:
		t.Fatal("no backup envelope")
	}
	if f.status(t, id) != smtpclient.Accepted {
		t.Fatal("aggregate lost")
	}
}

func TestFailoverClaimRollbackAndConcurrency(t *testing.T) {
	f := deliveryFixtureRetry(t, testsmtp.Options{DataCode: 451})
	f.worker.Repo.FailoverEnabled = true
	id := f.seed(t)
	runOne(t, f.worker)
	b := testsmtp.Start(t, testsmtp.Options{})
	bid := f.addProvider(t, b, 20, 1)
	dueFailover(t, f, id)
	if err := f.worker.Repo.Schedule(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustExec(t, f, `CREATE FUNCTION fail_switch_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event_type='PROVIDER_FAILOVER' THEN RAISE EXCEPTION 'injected'; END IF; RETURN NEW; END; $$; CREATE TRIGGER fail_switch_event BEFORE INSERT ON events FOR EACH ROW EXECUTE FUNCTION fail_switch_event();`)
	_, err := f.worker.Repo.Claim(context.Background())
	if err == nil {
		t.Fatal("audit failure ignored")
	}
	mustExec(t, f, `DROP TRIGGER fail_switch_event ON events; DROP FUNCTION fail_switch_event();`)
	var n int
	var route string
	if err := f.db.QueryRow(`SELECT route_provider_id FROM messages WHERE id=$1`, id).Scan(&route); err != nil || route == bid {
		t.Fatal("route leaked", err)
	}
	if err := f.db.QueryRow(`SELECT count(*) FROM delivery_attempts WHERE message_id=$1`, id).Scan(&n); err != nil || n != 1 {
		t.Fatal("attempt leaked", n, err)
	}
	var wg sync.WaitGroup
	ch := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, err := f.worker.Repo.Claim(context.Background())
			if err == nil && j.Provider.ID != bid {
				err = errors.New("wrong route")
			}
			ch <- err
		}()
	}
	wg.Wait()
	close(ch)
	success := 0
	for err := range ch {
		if err == nil {
			success++
		} else if !errors.Is(err, queue.ErrNoJob) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatal("duplicate claim", success)
	}
}

func TestFailoverNoRevisitAndProviderBudget(t *testing.T) {
	f := deliveryFixtureRetry(t, testsmtp.Options{DataCode: 451})
	f.worker.Repo.FailoverEnabled = true
	id := f.seed(t)
	runOne(t, f.worker)
	for i := 0; i < 2; i++ {
		b := testsmtp.Start(t, testsmtp.Options{DataCode: 451})
		bid := f.addProvider(t, b, 20+i, 1)
		f.worker.Sender = smtpclient.Client{Domain: "gateway.test", RootCAs: b.Roots}
		dueFailover(t, f, id)
		runOne(t, f.worker)
		var route string
		if err := f.db.QueryRow(`SELECT route_provider_id FROM messages WHERE id=$1`, id).Scan(&route); err != nil || route != bid {
			t.Fatal("revisited failed provider", err)
		}
	}
	b := testsmtp.Start(t, testsmtp.Options{})
	bid := f.addProvider(t, b, 1, 1)
	dueFailover(t, f, id)
	if err := f.worker.Repo.Schedule(context.Background()); err != nil {
		t.Fatal(err)
	}
	j, err := f.worker.Repo.Claim(context.Background())
	if err != nil || j.Provider.ID == bid {
		t.Fatal("provider budget exceeded", err)
	}
}

func TestFailoverRecoveryDoesNotReuseOldDecision(t *testing.T) {
	f := deliveryFixtureRetry(t, testsmtp.Options{DataCode: 451})
	f.worker.Repo.FailoverEnabled = true
	id := f.seed(t)
	runOne(t, f.worker)
	b := testsmtp.Start(t, testsmtp.Options{})
	bid := f.addProvider(t, b, 20, 1)
	dueFailover(t, f, id)
	if err := f.worker.Repo.Schedule(context.Background()); err != nil {
		t.Fatal(err)
	}
	j, err := f.worker.Repo.Claim(context.Background())
	if err != nil || j.Provider.ID != bid {
		t.Fatal(err)
	}
	f.expire(t, id)
	if err := f.worker.Repo.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	third := testsmtp.Start(t, testsmtp.Options{})
	f.addProvider(t, third, 1, 1)
	j, err = f.worker.Repo.Claim(context.Background())
	if err != nil || j.Provider.ID != bid {
		t.Fatal("stale failover decision reused", err)
	}
	if err := f.worker.Repo.ArmData(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	f.expire(t, id)
	if err := f.worker.Repo.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.worker.Repo.Claim(context.Background()); !errors.Is(err, queue.ErrNoJob) {
		t.Fatal("armed failover recovery resent", err)
	}
}

func TestFailoverLostCompletionRemainsUnknown(t *testing.T) {
	f := deliveryFixtureRetry(t, testsmtp.Options{DataCode: 451})
	f.worker.Repo.FailoverEnabled = true
	id := f.seed(t)
	runOne(t, f.worker)
	b := testsmtp.Start(t, testsmtp.Options{})
	f.addProvider(t, b, 20, 1)
	f.worker.Sender = smtpclient.Client{Domain: "gateway.test", RootCAs: b.Roots}
	dueFailover(t, f, id)
	mustExec(t, f, `CREATE FUNCTION fail_backup_completion() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event_type='SMTP_ACCEPTED' THEN RAISE EXCEPTION 'injected'; END IF; RETURN NEW; END; $$; CREATE TRIGGER fail_backup_completion BEFORE INSERT ON events FOR EACH ROW EXECUTE FUNCTION fail_backup_completion();`)
	worked, err := f.worker.RunOne(context.Background())
	mustExec(t, f, `DROP TRIGGER fail_backup_completion ON events; DROP FUNCTION fail_backup_completion();`)
	if !worked || err == nil {
		t.Fatal("commit failure ignored")
	}
	select {
	case <-b.Payloads:
	default:
		t.Fatal("backup never accepted")
	}
	f.expire(t, id)
	if err := f.worker.Repo.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.status(t, id) != smtpclient.Unknown {
		t.Fatal("lost acceptance did not become UNKNOWN")
	}
	third := testsmtp.Start(t, testsmtp.Options{})
	f.addProvider(t, third, 1, 1)
	if worked, err := f.worker.RunOne(context.Background()); worked || err != nil {
		t.Fatal("unsafe third attempt", err)
	}
	if third.Connections.Load() != 0 || b.Connections.Load() != 1 {
		t.Fatal("duplicate transmission")
	}
}
