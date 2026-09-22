package integration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"relaytale/internal/auth"
	"relaytale/internal/message"
	"relaytale/internal/testsmtp"
)

type captureAPISubmission struct{ sub message.Submission }

func (c *captureAPISubmission) Enqueue(_ context.Context, s message.Submission) error {
	c.sub = s
	return nil
}
func prepareAPI(t *testing.T, f *deliveryFixture, addresses ...string) message.Submission {
	t.Helper()
	capture := &captureAPISubmission{}
	r := f.receiver()
	r.Repo = capture
	_, e := r.Receive(context.Background(), message.Envelope{Account: auth.Account{ID: f.accountID, AllowedFrom: []string{"sender@" + f.domain}}, From: "sender@" + f.domain, Recipients: addresses}, strings.NewReader(f.raw()))
	if e != nil {
		t.Fatal(e)
	}
	return capture.sub
}
func testAPIKey(t *testing.T, f *deliveryFixture) string {
	t.Helper()
	id := uuid.NewString()
	mustExec(t, f, `INSERT INTO api_keys(id,name,key_hash,prefix) VALUES($1,'test',$2,'test')`, id, uuid.NewString())
	return id
}
func TestAPIIdempotencyConcurrentAndDelivery(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	identity := testAPIKey(t, f)
	repo := message.Postgres{DB: f.db}
	ctx := context.Background()
	submissions := make([]message.Submission, 8)
	for i := range submissions {
		submissions[i] = prepareAPI(t, f, "one@"+f.domain)
	}
	start := make(chan struct{})
	results := make(chan message.EnqueueResult, 8)
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for _, sub := range submissions {
		wg.Add(1)
		go func(s message.Submission) {
			defer wg.Done()
			<-start
			r, e := repo.EnqueueIdempotent(ctx, s, identity, "same-key")
			results <- r
			errs <- e
		}(sub)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	id := ""
	fresh := 0
	for r := range results {
		if id != "" && id != r.MessageID {
			t.Fatal("different message IDs")
		}
		id = r.MessageID
		if !r.Replayed {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatal(fresh)
	}
	assertCount(t, f, 1, `SELECT count(*) FROM messages WHERE api_key_id=$1`, identity)
	assertCount(t, f, 1, `SELECT count(*) FROM recipients WHERE message_id=$1`, id)
	assertCount(t, f, 3, `SELECT count(*) FROM events WHERE message_id=$1 AND source='api'`, id)
	assertCount(t, f, 1, `SELECT count(*) FROM messages WHERE id=$1 AND source_type='api' AND smtp_account_id IS NULL`, id)
	runOne(t, f.worker)
	if f.status(t, id) != "SMTP_ACCEPTED" {
		t.Fatal(f.status(t, id))
	}
	if got := <-f.fake.Payloads; string(got) != f.raw() {
		t.Fatal("MIME changed")
	}
	replay, e := repo.EnqueueIdempotent(ctx, submissions[0], identity, "same-key")
	if e != nil || !replay.Replayed || replay.MessageID != id {
		t.Fatal(replay, e)
	}
	assertCount(t, f, 1, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1`, id)
	changed := submissions[0]
	changed.Envelope.Recipients = []string{"other@" + f.domain}
	if _, e = repo.EnqueueIdempotent(ctx, changed, identity, "same-key"); !errors.Is(e, message.ErrIdempotencyConflict) {
		t.Fatal(e)
	}
	changed = submissions[0]
	changed.Archive.SHA256 = strings.Repeat("0", 64)
	if _, e = repo.EnqueueIdempotent(ctx, changed, identity, "same-key"); !errors.Is(e, message.ErrIdempotencyConflict) {
		t.Fatal(e)
	}
	// The namespace is the stable API key record, never an SMTP account or global key.
	other := testAPIKey(t, f)
	sub := prepareAPI(t, f, "one@"+f.domain)
	accepted, e := repo.EnqueueIdempotent(ctx, sub, other, "same-key")
	if e != nil || accepted.Replayed || accepted.MessageID == id {
		t.Fatal(accepted, e)
	}
}
func TestAPIIdempotencyRollbackAndSuppression(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(map[bool]string{false: "event failure", true: "commit failure"}[deferred], func(t *testing.T) {
			f := delivery(t, testsmtp.Options{}, 1)
			identity := testAPIKey(t, f)
			repo := message.Postgres{DB: f.db}
			ctx := context.Background()
			address := "blocked@" + f.domain
			suppress(t, f, address, nil)
			sub := prepareAPI(t, f, address)
			name := "fail_api_" + strings.ReplaceAll(uuid.NewString(), "-", "")
			mustExec(t, f, `CREATE FUNCTION `+name+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.message_id='`+sub.ID+`'::uuid THEN RAISE EXCEPTION 'injected API event failure'; END IF; RETURN NEW; END $$`)
			kind := "CREATE TRIGGER "
			timing := "BEFORE INSERT ON events "
			if deferred {
				kind = "CREATE CONSTRAINT TRIGGER "
				timing = "AFTER INSERT ON events DEFERRABLE INITIALLY DEFERRED "
			}
			mustExec(t, f, kind+name+" "+timing+`FOR EACH ROW EXECUTE FUNCTION `+name+`()`)
			t.Cleanup(func() {
				f.db.Exec(`DROP TRIGGER IF EXISTS ` + name + ` ON events`)
				f.db.Exec(`DROP FUNCTION IF EXISTS ` + name + `()`)
			})
			if _, e := repo.EnqueueIdempotent(ctx, sub, identity, "retry-after-failure"); e == nil {
				t.Fatal("failed transaction acknowledged")
			}
			assertCount(t, f, 0, `SELECT count(*) FROM api_submission_ledger WHERE api_key_id=$1`, identity)
			assertCount(t, f, 0, `SELECT count(*) FROM messages WHERE id=$1`, sub.ID)
			assertCount(t, f, 0, `SELECT count(*) FROM recipients WHERE message_id=$1`, sub.ID)
			assertCount(t, f, 0, `SELECT count(*) FROM events WHERE message_id=$1`, sub.ID)
			mustExec(t, f, `DROP TRIGGER `+name+` ON events`)
			result, e := repo.EnqueueIdempotent(ctx, sub, identity, "retry-after-failure")
			if e != nil || result.Replayed {
				t.Fatal(result, e)
			}
			if f.status(t, sub.ID) != "SUPPRESSED" {
				t.Fatal("API bypassed suppression")
			}
			assertCount(t, f, 1, `SELECT count(*) FROM api_submission_ledger WHERE api_key_id=$1`, identity)
		})
	}
}

func TestAPIIdempotencyPreservesUnknown(t *testing.T) {
	f := delivery(t, testsmtp.Options{DropFinal: true}, 1)
	identity := testAPIKey(t, f)
	repo := message.Postgres{DB: f.db}
	ctx := context.Background()
	sub := prepareAPI(t, f, "one@"+f.domain)
	first, e := repo.EnqueueIdempotent(ctx, sub, identity, "unknown-replay")
	if e != nil {
		t.Fatal(e)
	}
	runOne(t, f.worker)
	if f.status(t, first.MessageID) != "DELIVERY_UNKNOWN" {
		t.Fatal("expected unknown")
	}
	again := prepareAPI(t, f, "one@"+f.domain)
	result, e := repo.EnqueueIdempotent(ctx, again, identity, "unknown-replay")
	if e != nil || !result.Replayed || result.MessageID != first.MessageID {
		t.Fatal(result, e)
	}
	if f.status(t, first.MessageID) != "DELIVERY_UNKNOWN" {
		t.Fatal("replay changed unknown")
	}
	assertCount(t, f, 1, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1`, first.MessageID)
	assertCount(t, f, 0, `SELECT count(*) FROM messages WHERE id=$1`, again.ID)
	if _, e = f.db.Exec(`DELETE FROM api_submission_ledger WHERE api_key_id=$1`, identity); e == nil {
		t.Fatal("idempotency history removed")
	}
}
