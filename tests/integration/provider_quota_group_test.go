package integration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"relaytale/internal/message"
	"relaytale/internal/provider"
	"relaytale/internal/queue"
	"relaytale/internal/smtpclient"
	"relaytale/internal/testsmtp"
)

func quotaGroup(t *testing.T, f *deliveryFixture, hour, day int) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := provider.ManageQuotaGroup(context.Background(), f.db, true, id, 0, provider.QuotaGroupSettings{Name: id, HourlyLimit: hour, DailyLimit: day}, "test", "shared account"); err != nil {
		t.Fatal(err)
	}
	return id
}
func attachGroup(t *testing.T, f *deliveryFixture, pid, gid string) {
	t.Helper()
	var rev int64
	if err := f.db.QueryRow(`SELECT revision FROM providers WHERE id=$1`, pid).Scan(&rev); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.AttachQuotaGroup(context.Background(), f.db, pid, gid, rev, "test", "same upstream account"); err != nil {
		t.Fatal(err)
	}
}
func groupUnits(t *testing.T, f *deliveryFixture, gid string, want int) {
	t.Helper()
	assertCount(t, f, want, `SELECT daily_reserved FROM provider_quota_group_usage WHERE id=$1`, gid)
}

func TestSharedQuotaTLSPathsAndHistoricalAdmission(t *testing.T) {
	f := delivery(t, testsmtp.Options{ImplicitTLS: true}, 2)
	first := providerID(t, f)
	mustExec(t, f, `UPDATE providers SET security='implicit_tls' WHERE id=$1`, first)
	// Existing usage must be included when the group is attached later.
	id := f.seed(t)
	runOne(t, f.worker)
	if f.status(t, id) != smtpclient.Accepted {
		t.Fatal(f.status(t, id))
	}
	secondSMTP := testsmtp.Start(t, testsmtp.Options{})
	second := f.addProvider(t, secondSMTP, 20, 2)
	gid := quotaGroup(t, f, 2, 3)
	attachGroup(t, f, first, gid)
	attachGroup(t, f, second, gid)
	groupUnits(t, f, gid, 2)
	mustExec(t, f, `UPDATE providers SET enabled=false WHERE id=$1`, first)
	f.worker.Sender = smtpclient.Client{Domain: "relaytale.test", RootCAs: secondSMTP.Roots}
	next := f.seed(t)
	noWork(t, f)
	// Releasing the hour does not release the shared daily balance.
	mustExec(t, f, `UPDATE provider_quota SET reserved_at=clock_timestamp()-interval '61 minutes' WHERE provider_id=$1`, first)
	runOne(t, f.worker)
	if got := <-secondSMTP.Envelopes; len(got) != 1 || f.status(t, next) != "QUEUED" {
		t.Fatal("shared quota did not split recipients", got)
	}
	groupUnits(t, f, gid, 3)
	noWork(t, f)
	assertCount(t, f, 1, `SELECT count(*) FROM events WHERE message_id=$1 AND event_type='QUOTA_RESERVED' AND data->>'quota_group_id'=$2`, next, gid)
	mustExec(t, f, `UPDATE provider_quota SET reserved_at=clock_timestamp()-interval '25 hours' WHERE provider_id IN ($1,$2)`, first, second)
	runOne(t, f.worker)
	if f.status(t, next) != smtpclient.Accepted {
		t.Fatal("window release failed")
	}
}

func TestSharedQuotaConcurrentProvidersFreshSnapshot(t *testing.T) {
	a := delivery(t, testsmtp.Options{}, 2)
	b := delivery(t, testsmtp.Options{}, 2)
	gid := quotaGroup(t, a, 0, 3)
	attachGroup(t, a, providerID(t, a), gid)
	attachGroup(t, b, providerID(t, b), gid)
	a.seed(t)
	b.seed(t)
	gate, err := a.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback()
	if _, err = gate.Exec(`SELECT id FROM provider_quota_groups WHERE id=$1 FOR NO KEY UPDATE`, gid); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, f := range []*deliveryFixture{a, b} {
		wg.Add(1)
		go func(f *deliveryFixture) { defer wg.Done(); _, err := f.worker.Repo.Claim(ctx); errs <- err }(f)
	}
	// Hold both claims past route selection, so both initially see three units.
	deadline := time.Now().Add(3 * time.Second)
	waiting := 0
	for time.Now().Before(deadline) {
		if err = a.db.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT g.id FROM provider_quota_groups%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if waiting != 2 {
		t.Fatal("claims did not reach shared lock", waiting)
	}
	if err = gate.Commit(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	groupUnits(t, a, gid, 3)
	// Crashing after a committed reservation does not refund either provider.
	mustExec(t, a, `UPDATE messages SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE smtp_account_id IN ($1,$2) AND status='SENDING'`, a.accountID, b.accountID)
	if err = a.worker.Repo.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	groupUnits(t, a, gid, 3)
	noWork(t, a)
}

func TestSharedQuotaRetryFailoverAndUnknown(t *testing.T) {
	f := deliveryFixtureRetry(t, testsmtp.Options{DataCode: 451})
	f.worker.Repo.FailoverEnabled = true
	first := providerID(t, f)
	backup := testsmtp.Start(t, testsmtp.Options{})
	second := f.addProvider(t, backup, 20, 1)
	gid := quotaGroup(t, f, 0, 2)
	attachGroup(t, f, first, gid)
	attachGroup(t, f, second, gid)
	id := f.seed(t)
	runOne(t, f.worker)
	dueFailover(t, f, id)
	noWork(t, f) // safe failure cannot bypass the account budget through another port
	groupUnits(t, f, gid, 2)
	f.worker.Repo.FailoverEnabled = false
	noWork(t, f) // same-provider retry is also blocked
	f.worker.Repo.FailoverEnabled = true
	if _, err := provider.ManageQuotaGroup(context.Background(), f.db, false, gid, 1, provider.QuotaGroupSettings{Name: gid, DailyLimit: 4}, "test", "increase shared limit"); err != nil {
		t.Fatal(err)
	}
	f.worker.Sender = smtpclient.Client{Domain: "relaytale.test", RootCAs: backup.Roots}
	runOne(t, f.worker)
	groupUnits(t, f, gid, 4)
	if f.status(t, id) != smtpclient.Accepted {
		t.Fatal("safe failover failed")
	}
	u := delivery(t, testsmtp.Options{DropFinal: true}, 1)
	ug := quotaGroup(t, u, 0, 2)
	attachGroup(t, u, providerID(t, u), ug)
	uid := u.seed(t)
	runOne(t, u.worker)
	if u.status(t, uid) != smtpclient.Unknown {
		t.Fatal("unknown overwritten")
	}
	u.seed(t)
	noWork(t, u)
	groupUnits(t, u, ug, 2)
}

func TestSharedQuotaManagementFencesAndRollback(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h := management(t, f)
	gid := uuid.NewString()
	settings := provider.QuotaGroupSettings{Name: gid, HourlyLimit: 5, DailyLimit: 10}
	create := map[string]any{"id": gid, "settings": settings, "reason": "account budget"}
	path := "/provider-quota-groups"
	for _, role := range []string{"v", "o"} {
		requireStatus(t, adminRequest(h, "POST", path, role, create), 403)
	}
	requireStatus(t, adminRequest(h, "POST", path, "a", create), 201)
	requireStatus(t, adminRequest(h, "GET", path+"/"+gid, "v", nil), 200)
	update := map[string]any{"expected_revision": 1, "settings": settings, "reason": "limit update"}
	requireStatus(t, adminRequest(h, "PUT", path+"/"+gid, "a", update), 200)
	requireStatus(t, adminRequest(h, "PUT", path+"/"+gid, "a", update), 409)
	pid := providerID(t, f)
	var rev int64
	if err := f.db.QueryRow(`SELECT revision FROM providers WHERE id=$1`, pid).Scan(&rev); err != nil {
		t.Fatal(err)
	}
	attach := map[string]any{"group_id": gid, "expected_revision": rev, "reason": "same account"}
	requireStatus(t, adminRequest(h, "POST", "/providers/"+pid+"/quota-group", "o", attach), 403)
	requireStatus(t, adminRequest(h, "POST", "/providers/"+pid+"/quota-group", "a", attach), 200)
	requireStatus(t, adminRequest(h, "POST", "/providers/"+pid+"/quota-group", "a", attach), 409)
	if _, err := f.db.Exec(`UPDATE providers SET quota_group_id=NULL WHERE id=$1`, pid); err == nil {
		t.Fatal("membership removed")
	}
	other := quotaGroup(t, f, 0, 1)
	if _, err := f.db.Exec(`UPDATE providers SET quota_group_id=$2 WHERE id=$1`, pid, other); err == nil {
		t.Fatal("membership reassigned")
	}
	read := adminRequest(h, "GET", "/providers/"+pid, "v", nil)
	requireStatus(t, read, 200)
	if !strings.Contains(read.Body.String(), gid) {
		t.Fatal("membership missing from provider read")
	}
	// Claim and group audit failures must never leave partial writes.
	id := f.seed(t)
	name := "fail_group_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	mustExec(t, f, `CREATE FUNCTION `+name+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected'; END $$`)
	mustExec(t, f, `CREATE CONSTRAINT TRIGGER `+name+` AFTER INSERT ON events DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.event_type='QUOTA_RESERVED') EXECUTE FUNCTION `+name+`() `)
	_, err := f.worker.Repo.Claim(context.Background())
	mustExec(t, f, `DROP TRIGGER `+name+` ON events`)
	if err == nil {
		t.Fatal("failed reservation acknowledged")
	}
	groupUnits(t, f, gid, 0)
	if f.status(t, id) != "QUEUED" {
		t.Fatal("claim leaked")
	}
	mustExec(t, f, `CREATE TRIGGER `+name+` BEFORE INSERT ON maintenance_audit FOR EACH ROW EXECUTE FUNCTION `+name+`() `)
	_, err = provider.ManageQuotaGroup(context.Background(), f.db, false, gid, 2, provider.QuotaGroupSettings{Name: gid, DailyLimit: 1}, "test", "audit rollback")
	mustExec(t, f, `DROP TRIGGER `+name+` ON maintenance_audit; DROP FUNCTION `+name+`() `)
	if err == nil {
		t.Fatal("failed audit acknowledged")
	}
	assertCount(t, f, 10, `SELECT daily_limit FROM provider_quota_groups WHERE id=$1`, gid)
	runOne(t, f.worker)
	groupUnits(t, f, gid, 2)
	if _, err := provider.AttachQuotaGroup(context.Background(), f.db, pid, other, rev+1, "test", "move"); !errors.Is(err, provider.ErrConflict) {
		t.Fatal(err)
	}
	if _, err := f.worker.Repo.Claim(context.Background()); !errors.Is(err, queue.ErrNoJob) {
		t.Fatal(err)
	}
}

func TestSharedQuotaWebsiteBudgetRemainsIndependent(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	gid := quotaGroup(t, f, 0, 1)
	attachGroup(t, f, providerID(t, f), gid)
	_, key, receiver := sendFixture(t, f)
	key.Budget = message.SubmissionBudget{Day: 2}
	h := budgetHandler(t, key, receiver)
	requireStatus(t, sendRequest(h, key, "one", f.raw(), nil), 202)
	requireStatus(t, sendRequest(h, key, "two", f.raw(), nil), 202)
	requireStatus(t, sendRequest(h, key, "three", f.raw(), nil), 429)
	runOne(t, f.worker)
	noWork(t, f)
	groupUnits(t, f, gid, 1)
	assertCount(t, f, 2, `SELECT sum(units) FROM api_submission_quota WHERE api_key_id=$1`, key.ID)
	assertCount(t, f, 1, `SELECT count(*) FROM messages WHERE api_key_id=$1 AND status='QUEUED'`, key.ID)
	requireStatus(t, sendRequest(h, key, "one", f.raw(), nil), 202)
	groupUnits(t, f, gid, 1)
}
