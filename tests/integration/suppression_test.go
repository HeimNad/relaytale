package integration

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"relaytale/internal/auth"
	"relaytale/internal/message"
	"relaytale/internal/provider"
	"relaytale/internal/queue"
	"relaytale/internal/smtpclient"
	"relaytale/internal/suppression"
	"relaytale/internal/testsmtp"
)

func suppressionSeed(t *testing.T, f *deliveryFixture, addresses ...string) string {
	t.Helper()
	id, err := f.receiver().Receive(context.Background(), message.Envelope{Account: auth.Account{ID: f.accountID, AllowedFrom: []string{"sender@" + f.domain}}, From: "sender@" + f.domain, Recipients: addresses}, strings.NewReader(f.raw()))
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func suppress(t *testing.T, f *deliveryFixture, address string, expiry *time.Time) string {
	t.Helper()
	s := suppression.Service{DB: f.db}
	id, err := s.Add(context.Background(), suppression.AddRequest{Email: address, Actor: "integration", Reason: "controlled test", Category: "manual", ExpiresAt: expiry})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		err := s.Release(context.Background(), id, "integration", "cleanup")
		if err != nil && !errors.Is(err, suppression.ErrStale) {
			t.Error(err)
		}
	})
	return id
}
func recipientStatus(t *testing.T, f *deliveryFixture, mid, address string) string {
	t.Helper()
	var state string
	if err := f.db.QueryRow(`SELECT status FROM recipients WHERE message_id=$1 AND address=$2`, mid, address).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}
func assertCount(t *testing.T, f *deliveryFixture, want int, q string, args ...any) {
	t.Helper()
	var n int
	if err := f.db.QueryRow(q, args...).Scan(&n); err != nil || n != want {
		t.Fatalf("count: want %d got %d: %v", want, n, err)
	}
}

func TestSuppressionIngressMixedAndReleaseDoesNotReplay(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	one, two := "One@"+f.domain, "two@"+f.domain
	sid := suppress(t, f, strings.ToUpper(one), nil)
	id := suppressionSeed(t, f, one, two)
	if recipientStatus(t, f, id, one) != "SUPPRESSED" {
		t.Fatal("ingress did not suppress case variant")
	}
	runOne(t, f.worker)
	if got := <-f.fake.Envelopes; len(got) != 1 || got[0] != two {
		t.Fatal("blocked recipient sent", got)
	}
	if got := <-f.fake.Payloads; string(got) != f.raw() {
		t.Fatal("MIME changed")
	}
	if f.status(t, id) != "PARTIAL_ACCEPTED" {
		t.Fatal("mixed projection")
	}
	assertCount(t, f, 1, `SELECT sum(units) FROM provider_quota WHERE provider_id=$1`, providerID(t, f))
	if err := (suppression.Service{DB: f.db}).Release(context.Background(), sid, "operator", "verified release"); err != nil {
		t.Fatal(err)
	}
	noWork(t, f)
	if recipientStatus(t, f, id, one) != "SUPPRESSED" {
		t.Fatal("release rewrote history")
	}
	newID := suppressionSeed(t, f, one)
	runOne(t, f.worker)
	if f.status(t, newID) != smtpclient.Accepted {
		t.Fatal("release did not permit future message")
	}
}
func TestSuppressionAllAndExpiry(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	one := "one@" + f.domain
	expiry := time.Now().Add(time.Hour)
	sid := suppress(t, f, one, &expiry)
	id := suppressionSeed(t, f, one)
	if f.status(t, id) != "SUPPRESSED" {
		t.Fatal("all-suppressed projection")
	}
	noWork(t, f)
	assertCount(t, f, 0, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1`, id)
	assertCount(t, f, 1, `SELECT count(*) FROM events WHERE message_id=$1 AND event_type='MESSAGE_SUPPRESSED'`, id)
	mustExec(t, f, `UPDATE suppression_entries SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, sid)
	next := suppressionSeed(t, f, one)
	runOne(t, f.worker)
	if f.status(t, next) != smtpclient.Accepted || f.status(t, id) != "SUPPRESSED" {
		t.Fatal("expiry replayed history or blocked new mail")
	}
	suppress(t, f, one, nil)
	// Old-ID release must not release the replacement.
	if err := (suppression.Service{DB: f.db}).Release(context.Background(), sid, "op", "stale"); !errors.Is(err, suppression.ErrStale) {
		t.Fatal("stale release", err)
	}
	third := suppressionSeed(t, f, one)
	if f.status(t, third) != "SUPPRESSED" {
		t.Fatal("replacement was released")
	}
}
func TestSuppressionPendingRetriesAndNoProvider(t *testing.T) {
	f := delivery(t, testsmtp.Options{DataCode: 451}, 1)
	f.worker.Repo.RetryEnabled = true
	one, two := "one@"+f.domain, "two@"+f.domain
	id := suppressionSeed(t, f, one, two)
	runOne(t, f.worker)
	suppress(t, f, one, nil)
	suppress(t, f, two, nil)
	mustExec(t, f, `UPDATE providers SET enabled=false WHERE id=$1`, providerID(t, f))
	noWork(t, f)
	if f.status(t, id) != "SUPPRESSED" {
		t.Fatal("paused retries not suppressed without provider")
	}
	assertCount(t, f, 0, `SELECT count(*) FROM recipients WHERE message_id=$1 AND (retry_at IS NOT NULL OR retry_automatic)`, id)
	assertCount(t, f, 1, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1`, id)
}
func TestSuppressionDoesNotFoldPlusOrDots(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	suppress(t, f, "one@"+f.domain, nil)
	id := suppressionSeed(t, f, "one+tag@"+f.domain, "o.ne@"+f.domain)
	runOne(t, f.worker)
	if f.status(t, id) != smtpclient.Accepted {
		t.Fatal("alias heuristics used")
	}
}

type suppressionSender struct {
	base queue.Sender
	hook func(context.Context, func(context.Context) error) error
}

func (s suppressionSender) Send(ctx context.Context, p provider.Provider, password, from string, rc []smtpclient.Recipient, raw io.ReadSeeker, gate func(context.Context) error, events ...func(context.Context, smtpclient.Event) error) smtpclient.Result {
	return s.base.Send(ctx, p, password, from, rc, raw, func(c context.Context) error { return s.hook(c, gate) }, events...)
}
func TestSuppressionMidConversationResumesOnlyUnblocked(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	one, two := "one@"+f.domain, "two@"+f.domain
	id := suppressionSeed(t, f, one, two)
	base := f.worker.Sender
	f.worker.Sender = suppressionSender{base, func(ctx context.Context, gate func(context.Context) error) error {
		sid := suppress(t, f, one, nil)
		err := gate(ctx)
		if !errors.Is(err, smtpclient.ErrSuppressed) {
			t.Fatal("gate did not block", err)
		}
		// Release before Finish must not undo the committed gate snapshot.
		if e := (suppression.Service{DB: f.db}).Release(ctx, sid, "op", "release after gate"); e != nil {
			t.Fatal(e)
		}
		return err
	}}
	runOne(t, f.worker)
	select {
	case <-f.fake.Payloads:
		t.Fatal("DATA sent after block")
	default:
	}
	if recipientStatus(t, f, id, one) != "SUPPRESSED" || recipientStatus(t, f, id, two) != "QUEUED" {
		t.Fatal("bad gate projection")
	}
	assertCount(t, f, 1, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1 AND suppression_blocked_at IS NOT NULL AND data_armed_at IS NULL AND health_outcome='IGNORED'`, id)
	assertCount(t, f, 1, `SELECT count(*) FROM attempt_recipients ar JOIN recipients r ON r.id=ar.recipient_id WHERE r.message_id=$1 AND r.address=$2 AND ar.decision->>'action'='RESUME_SAME_PROVIDER' AND ar.decision->>'failover_allowed'='false'`, id, two)
	f.worker.Sender = base
	runOne(t, f.worker)
	if got := <-f.fake.Envelopes; len(got) != 1 || got[0] != two {
		t.Fatal("resumed envelope", got)
	}
	if f.status(t, id) != "PARTIAL_ACCEPTED" {
		t.Fatal("completion projection")
	}
	assertCount(t, f, 3, `SELECT sum(units) FROM provider_quota WHERE provider_id=$1`, providerID(t, f))
}
func TestSuppressionAfterDataPermissionCannotRetract(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	one := "one@" + f.domain
	id := suppressionSeed(t, f, one)
	f.worker.Sender = suppressionSender{f.worker.Sender, func(ctx context.Context, gate func(context.Context) error) error {
		if err := gate(ctx); err != nil {
			return err
		}
		suppress(t, f, one, nil)
		return nil
	}}
	runOne(t, f.worker)
	if f.status(t, id) != smtpclient.Accepted {
		t.Fatal("authorized attempt retracted")
	}
	if next := suppressionSeed(t, f, one); f.status(t, next) != "SUPPRESSED" {
		t.Fatal("future mail not blocked")
	}
}
func TestSuppressionGateCrashRecoveryAndBudget(t *testing.T) {
	for _, exhausted := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume", true: "budget"}[exhausted], func(t *testing.T) {
			f := delivery(t, testsmtp.Options{}, 1)
			one, two := "one@"+f.domain, "two@"+f.domain
			id := suppressionSeed(t, f, one, two)
			j, err := f.worker.Repo.Claim(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			suppress(t, f, one, nil)
			if err = f.worker.Repo.ArmData(context.Background(), j); !errors.Is(err, smtpclient.ErrSuppressed) {
				t.Fatal(err)
			}
			if exhausted {
				mustExec(t, f, `UPDATE recipients SET attempt_count=7 WHERE message_id=$1`, id)
			}
			f.expire(t, id)
			if err = f.worker.Repo.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			if recipientStatus(t, f, id, one) != "SUPPRESSED" {
				t.Fatal("recovery erased suppression")
			}
			if exhausted {
				noWork(t, f)
			} else {
				runOne(t, f.worker)
				if got := <-f.fake.Envelopes; len(got) != 1 || got[0] != two {
					t.Fatal(got)
				}
			}
		})
	}
}
func TestSuppressionUnknownPreservedAndManualRetryBlocked(t *testing.T) {
	f := delivery(t, testsmtp.Options{DropFinal: true}, 1)
	one := "one@" + f.domain
	id := suppressionSeed(t, f, one)
	runOne(t, f.worker)
	sid := suppress(t, f, one, nil)
	noWork(t, f)
	if f.status(t, id) != smtpclient.Unknown {
		t.Fatal("UNKNOWN was hidden")
	}
	var rid, aid string
	if err := f.db.QueryRow(`SELECT r.id,a.id FROM recipients r JOIN delivery_attempts a ON a.message_id=r.message_id WHERE r.message_id=$1`, id).Scan(&rid, &aid); err != nil {
		t.Fatal(err)
	}
	v := queue.Resolution{RecipientID: rid, ExpectedAttempt: aid, Action: "retry", Actor: "op", Reason: "checked", AcknowledgeDuplicate: true}
	if _, err := f.worker.Repo.ResolveUnknown(context.Background(), v); !errors.Is(err, suppression.ErrBlocked) {
		t.Fatal("suppressed manual retry", err)
	}
	if err := (suppression.Service{DB: f.db}).Release(context.Background(), sid, "op", "release"); err != nil {
		t.Fatal(err)
	}
	noWork(t, f)
	if f.status(t, id) != smtpclient.Unknown {
		t.Fatal("release revived UNKNOWN")
	}
}
func TestSuppressionConcurrentAddReleaseAndHistory(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	s := suppression.Service{DB: f.db}
	email := "one@" + f.domain
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := s.Add(context.Background(), suppression.AddRequest{Email: email, Category: "manual", Actor: "op", Reason: "parallel"})
			if err == nil {
				ids <- id
			} else {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if !errors.Is(err, suppression.ErrActive) {
			t.Fatal(err)
		}
	}
	if len(ids) != 1 {
		t.Fatal("duplicate active entries")
	}
	id := <-ids
	t.Cleanup(func() { _ = s.Release(context.Background(), id, "op", "cleanup") })
	if err := s.Release(context.Background(), id, "op", "done"); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(context.Background(), id, "op", "duplicate"); !errors.Is(err, suppression.ErrStale) {
		t.Fatal("duplicate release accepted", err)
	}
	suppress(t, f, email, nil)
	page, err := s.List(context.Background(), strings.ToUpper(email), "", 1)
	if err != nil || len(page) != 1 || page[0].Active {
		t.Fatal("history page", page, err)
	}
	next, err := s.List(context.Background(), email, page[0].ID, 1)
	if err != nil || len(next) != 1 || !next[0].Active {
		t.Fatal("next page", next, err)
	}
	assertCount(t, f, 2, `SELECT count(*) FROM maintenance_audit WHERE data->>'id'=$1`, id)
}
func TestSuppressionAuditFailureRollsBack(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	s := suppression.Service{DB: f.db}
	email := "one@" + f.domain
	mustExec(t, f, `CREATE FUNCTION fail_suppression_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action LIKE 'SUPPRESSION_%' THEN RAISE EXCEPTION 'injected'; END IF; RETURN NEW; END; $$; CREATE TRIGGER fail_suppression_audit BEFORE INSERT ON maintenance_audit FOR EACH ROW EXECUTE FUNCTION fail_suppression_audit();`)
	_, err := s.Add(context.Background(), suppression.AddRequest{Email: email, Category: "manual", Actor: "op", Reason: "test"})
	mustExec(t, f, `DROP TRIGGER fail_suppression_audit ON maintenance_audit; DROP FUNCTION fail_suppression_audit();`)
	if err == nil {
		t.Fatal("audit failure ignored")
	}
	assertCount(t, f, 0, `SELECT count(*) FROM suppression_entries WHERE email=$1`, email)
	sid := suppress(t, f, email, nil)
	mustExec(t, f, `CREATE FUNCTION fail_suppression_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected'; END; $$; CREATE TRIGGER fail_suppression_audit BEFORE INSERT ON maintenance_audit FOR EACH ROW EXECUTE FUNCTION fail_suppression_audit();`)
	err = s.Release(context.Background(), sid, "op", "test")
	mustExec(t, f, `DROP TRIGGER fail_suppression_audit ON maintenance_audit; DROP FUNCTION fail_suppression_audit();`)
	if err == nil {
		t.Fatal("release audit failure ignored")
	}
	entries, err := s.List(context.Background(), email, "", 10)
	if err != nil || len(entries) != 1 || !entries[0].Active {
		t.Fatal("release was not rolled back", err)
	}
}
func TestSuppressionGateAuditFailureNeverAuthorizesData(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	one := "one@" + f.domain
	id := suppressionSeed(t, f, one)
	j, err := f.worker.Repo.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	suppress(t, f, one, nil)
	mustExec(t, f, `CREATE FUNCTION fail_suppression_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event_type='SUPPRESSION_GATE_BLOCKED' THEN RAISE EXCEPTION 'injected'; END IF; RETURN NEW; END; $$; CREATE TRIGGER fail_suppression_event BEFORE INSERT ON events FOR EACH ROW EXECUTE FUNCTION fail_suppression_event();`)
	err = f.worker.Repo.ArmData(context.Background(), j)
	mustExec(t, f, `DROP TRIGGER fail_suppression_event ON events; DROP FUNCTION fail_suppression_event();`)
	if err == nil {
		t.Fatal("event failure ignored")
	}
	assertCount(t, f, 1, `SELECT count(*) FROM delivery_attempts WHERE id=$1 AND data_armed_at IS NULL AND suppression_blocked_at IS NULL`, j.AttemptID)
	if recipientStatus(t, f, id, one) != "SENDING" {
		t.Fatal("partial gate commit")
	}
	if err = f.worker.Repo.ArmData(context.Background(), j); !errors.Is(err, smtpclient.ErrSuppressed) {
		t.Fatal("retry gate did not block", err)
	}
}
func TestSuppressionNeverCreatedFromSMTP5xx(t *testing.T) {
	f := delivery(t, testsmtp.Options{RecipientCode: func(_ int, _ string) int { return 550 }}, 1)
	one := "one@" + f.domain
	id := suppressionSeed(t, f, one)
	runOne(t, f.worker)
	if f.status(t, id) != smtpclient.Permanent {
		t.Fatal("wrong SMTP outcome")
	}
	assertCount(t, f, 0, `SELECT count(*) FROM suppression_entries WHERE email=$1`, one)
}

func TestSuppressionMixedRCPTFactsSurvivePolicyAbort(t *testing.T) {
	f := delivery(t, testsmtp.Options{RecipientCode: func(_ int, address string) int {
		if strings.HasPrefix(address, "bad@") {
			return 550
		}
		if strings.HasPrefix(address, "later@") {
			return 450
		}
		return 0
	}}, 1)
	one, bad, later := "one@"+f.domain, "bad@"+f.domain, "later@"+f.domain
	id := suppressionSeed(t, f, one, bad, later)
	f.worker.Sender = suppressionSender{f.worker.Sender, func(ctx context.Context, gate func(context.Context) error) error {
		suppress(t, f, one, nil)
		return gate(ctx)
	}}
	runOne(t, f.worker)
	if recipientStatus(t, f, id, one) != "SUPPRESSED" || recipientStatus(t, f, id, bad) != smtpclient.Permanent || recipientStatus(t, f, id, later) != smtpclient.Temporary {
		t.Fatal("RCPT facts overwritten by policy abort")
	}
	assertCount(t, f, 2, `SELECT count(*) FROM attempt_recipients ar JOIN recipients r ON r.id=ar.recipient_id WHERE r.message_id=$1 AND ar.protocol_stage='RCPT_REJECTED' AND ar.smtp_code IN (450,550)`, id)
	noWork(t, f) // default retry disabled; 450 must not be resumed as an unsent accepted RCPT
}
func TestSuppressionFinishHonorsBudgetAndCommitFailure(t *testing.T) {
	for _, mode := range []string{"budget", "commit"} {
		t.Run(mode, func(t *testing.T) {
			f := delivery(t, testsmtp.Options{}, 1)
			one, two := "one@"+f.domain, "two@"+f.domain
			id := suppressionSeed(t, f, one, two)
			f.worker.Sender = suppressionSender{f.worker.Sender, func(ctx context.Context, gate func(context.Context) error) error {
				suppress(t, f, one, nil)
				if mode == "budget" {
					mustExec(t, f, `UPDATE recipients SET attempt_count=7 WHERE message_id=$1`, id)
				}
				return gate(ctx)
			}}
			if mode == "commit" {
				mustExec(t, f, `CREATE FUNCTION fail_suppression_finish() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event_type='ATTEMPT_FAILED' THEN RAISE EXCEPTION 'injected'; END IF; RETURN NEW; END; $$; CREATE TRIGGER fail_suppression_finish BEFORE INSERT ON events FOR EACH ROW EXECUTE FUNCTION fail_suppression_finish();`)
				worked, err := f.worker.RunOne(context.Background())
				mustExec(t, f, `DROP TRIGGER fail_suppression_finish ON events; DROP FUNCTION fail_suppression_finish();`)
				if !worked || err == nil {
					t.Fatal("finish failure ignored")
				}
				if f.status(t, id) != "SENDING" || recipientStatus(t, f, id, one) != "SUPPRESSED" {
					t.Fatal("gate evidence lost")
				}
				f.expire(t, id)
				if err = f.worker.Repo.Recover(context.Background()); err != nil {
					t.Fatal(err)
				}
				f.worker.Sender = smtpclient.Client{Domain: "relaytale.test", RootCAs: f.fake.Roots}
				runOne(t, f.worker)
			} else {
				runOne(t, f.worker)
				noWork(t, f)
				if recipientStatus(t, f, id, two) != "TEMP_FAILED" {
					t.Fatal("budget bypassed")
				}
			}
		})
	}
}
func TestSuppressionAdmissionWaitsForPolicyTransaction(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	one := "one@" + f.domain
	id := suppressionSeed(t, f, one)
	j, err := f.worker.Repo.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = suppression.Lock(context.Background(), tx, true); err != nil {
		t.Fatal(err)
	}
	// A held policy mutation must prevent DATA permission, even if the address
	// had no suppression row in the reader's earlier snapshot.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err = f.worker.Repo.ArmData(ctx, j); err == nil {
		t.Fatal("DATA permission bypassed policy lock")
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	suppress(t, f, one, nil)
	if err = f.worker.Repo.ArmData(context.Background(), j); !errors.Is(err, smtpclient.ErrSuppressed) {
		t.Fatal("fresh snapshot not used", err)
	}
	assertCount(t, f, 1, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1 AND data_armed_at IS NULL`, id)
}
func TestSuppressionIngressAuditFailureDoesNotAcknowledge(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	one := "one@" + f.domain
	suppress(t, f, one, nil)
	mustExec(t, f, `CREATE FUNCTION fail_suppression_ingress() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event_type='RECIPIENT_SUPPRESSED' THEN RAISE EXCEPTION 'injected'; END IF; RETURN NEW; END; $$; CREATE TRIGGER fail_suppression_ingress BEFORE INSERT ON events FOR EACH ROW EXECUTE FUNCTION fail_suppression_ingress();`)
	_, err := f.receiver().Receive(context.Background(), message.Envelope{Account: auth.Account{ID: f.accountID, AllowedFrom: []string{"sender@" + f.domain}}, From: "sender@" + f.domain, Recipients: []string{one}}, strings.NewReader(f.raw()))
	mustExec(t, f, `DROP TRIGGER fail_suppression_ingress ON events; DROP FUNCTION fail_suppression_ingress();`)
	if err == nil {
		t.Fatal("failed ingress was acknowledged")
	}
	assertCount(t, f, 0, `SELECT count(*) FROM messages WHERE smtp_account_id=$1`, f.accountID)
}
func TestUntrustedDuplicateDSNIsOnlyMailContent(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	victim := "victim@" + f.domain
	receiver := "feedback@" + f.domain
	original := suppressionSeed(t, f, victim)
	runOne(t, f.worker)
	payload := "From: sender@" + f.domain + "\r\nTo: " + receiver + "\r\nMessage-ID: <forged-duplicate@invalid.test>\r\nSubject: failure notice\r\nContent-Type: multipart/report; report-type=delivery-status; boundary=dsn\r\n\r\n--dsn\r\nContent-Type: text/plain\r\n\r\nDelivery failed\r\n--dsn\r\nContent-Type: message/delivery-status\r\n\r\nReporting-MTA: dns; untrusted.invalid\r\nOriginal-Envelope-Id: " + original + "\r\n\r\nFinal-Recipient: rfc822; " + victim + "\r\nAction: failed\r\nStatus: 5.1.1\r\n\r\n--dsn--\r\n"
	for i := 0; i < 2; i++ {
		_, err := f.receiver().Receive(context.Background(), message.Envelope{Account: auth.Account{ID: f.accountID, AllowedFrom: []string{"sender@" + f.domain}}, From: "sender@" + f.domain, Recipients: []string{receiver}}, strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
	}
	assertCount(t, f, 0, `SELECT count(*) FROM suppression_entries WHERE email=$1`, victim)
	if f.status(t, original) != smtpclient.Accepted {
		t.Fatal("untrusted feedback changed original delivery")
	}
}
