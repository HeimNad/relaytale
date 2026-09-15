package integration

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"relaytale/internal/feedback"
	"relaytale/internal/smtpclient"
	"relaytale/internal/testsmtp"
)

func feedbackRaw(address string) string {
	return "Content-Type: multipart/report; report-type=delivery-status; boundary=dsn\r\n\r\n--dsn\r\nContent-Type: text/plain\r\n\r\nUntrusted failure\r\n--dsn\r\nContent-Type: message/delivery-status\r\n\r\nReporting-MTA: dns; forged.test\r\n\r\nFinal-Recipient: rfc822; " + address + "\r\nAction: failed\r\nStatus: 5.1.1\r\n\r\n--dsn--\r\n"
}
func TestFeedbackQuarantineNeverChangesDelivery(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "unknown"}[unknown], func(t *testing.T) {
			f := delivery(t, testsmtp.Options{DropFinal: unknown}, 1)
			address := "one@" + f.domain
			mid := suppressionSeed(t, f, address)
			runOne(t, f.worker)
			s := feedback.Service{DB: f.db}
			ctx := context.Background()
			v := feedback.ImportRequest{Source: uuid.NewString(), ExternalID: "event-1", Actor: "tester", Reason: "unverified mailbox export"}
			raw := feedbackRaw(address)
			result, err := s.Import(ctx, v, strings.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			if result.Entry.State != "QUARANTINED" || result.Entry.Trust != "UNVERIFIED" || result.Entry.ParseState != "PARSED" {
				t.Fatal(result)
			}
			var saved []byte
			if err = f.db.QueryRow(`SELECT raw FROM feedback_reports WHERE id=$1`, result.Entry.ID).Scan(&saved); err != nil || !bytes.Equal(saved, []byte(raw)) {
				t.Fatal("evidence changed", err)
			}
			if err = s.Dismiss(ctx, result.Entry.ID, "wrong", "op", "review"); err == nil {
				t.Fatal("wrong evidence digest accepted")
			}
			if err = s.Dismiss(ctx, result.Entry.ID, result.Entry.SHA256, "op", "untrusted"); err != nil {
				t.Fatal(err)
			}
			if err = s.Dismiss(ctx, result.Entry.ID, result.Entry.SHA256, "op", "repeat"); !errors.Is(err, feedback.ErrReviewed) {
				t.Fatal(err)
			}
			e, err := s.Get(ctx, result.Entry.ID)
			if err != nil || e.State != "DISMISSED" {
				t.Fatal(e, err)
			}
			expected := smtpclient.Accepted
			if unknown {
				expected = smtpclient.Unknown
			}
			if f.status(t, mid) != expected {
				t.Fatal("feedback changed SMTP evidence")
			}
			assertCount(t, f, 0, `SELECT count(*) FROM suppression_entries WHERE email=$1`, address)
			assertCount(t, f, 2, `SELECT count(*) FROM maintenance_audit WHERE data->>'id'=$1`, e.ID)
			if _, err = f.db.Exec(`UPDATE feedback_reports SET trust='VERIFIED' WHERE id=$1`, e.ID); err == nil {
				t.Fatal("trust promotion allowed")
			}
			if _, err = f.db.Exec(`DELETE FROM feedback_reviews WHERE feedback_id=$1`, e.ID); err == nil {
				t.Fatal("review deletion allowed")
			}
		})
	}
}
func TestFeedbackConcurrentDuplicateAndConflict(t *testing.T) {
	db := testDB(t)
	s := feedback.Service{DB: db}
	ctx := context.Background()
	v := feedback.ImportRequest{Source: uuid.NewString(), ExternalID: "stable", Actor: "test", Reason: "fixture"}
	var wg sync.WaitGroup
	results := make(chan feedback.ImportResult, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := s.Import(ctx, v, strings.NewReader(feedbackRaw("one@example.test")))
			results <- r
			errs <- e
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	created := 0
	id := ""
	digest := ""
	for r := range results {
		if !r.Duplicate {
			created++
		}
		if id != "" && id != r.Entry.ID {
			t.Fatal("duplicate IDs")
		}
		id = r.Entry.ID
		digest = r.Entry.SHA256
	}
	if created != 1 {
		t.Fatal("duplicate creation", created)
	}
	if err := s.Dismiss(ctx, id, digest, "op", "checked"); err != nil {
		t.Fatal(err)
	}
	r, err := s.Import(ctx, v, strings.NewReader(feedbackRaw("other@example.test")))
	if err != nil || r.Entry.State != "CONFLICT" {
		t.Fatal(r, err)
	}
	old, err := s.Get(ctx, id)
	if err != nil || old.State != "CONFLICT" {
		t.Fatal("conflict hidden by prior dismissal", old, err)
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM maintenance_audit WHERE action='FEEDBACK_QUARANTINED' AND data->>'source'=$1`, v.Source).Scan(&count); err != nil || count != 2 {
		t.Fatal("duplicate audit", count, err)
	}
	// Lists are bounded and exclude parsed content / original MIME.
	page, err := s.List(ctx, id, 1)
	if err != nil || len(page) != 1 || page[0].ID != r.Entry.ID || page[0].Report != nil {
		t.Fatal(page, err)
	}
	if _, err = s.List(ctx, "", 201); err == nil {
		t.Fatal("unbounded list")
	}
}
func TestFeedbackUnsupportedAndAuditRollback(t *testing.T) {
	db := testDB(t)
	s := feedback.Service{DB: db}
	ctx := context.Background()
	v := feedback.ImportRequest{Source: uuid.NewString(), Actor: "test", Reason: "fixture"}
	r, err := s.Import(ctx, v, strings.NewReader("not a DSN"))
	if err != nil || r.Entry.ParseState != "UNSUPPORTED" || r.Entry.State != "QUARANTINED" {
		t.Fatal(r, err)
	}
	dup, err := s.Import(ctx, v, strings.NewReader("not a DSN"))
	if err != nil || !dup.Duplicate || dup.Entry.ID != r.Entry.ID {
		t.Fatal(dup, err)
	}
	if _, err = s.Import(ctx, v, strings.NewReader(strings.Repeat("x", feedback.MaxBytes+1))); !errors.Is(err, feedback.ErrSize) {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE FUNCTION fail_feedback_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action LIKE 'FEEDBACK_%' THEN RAISE EXCEPTION 'injected'; END IF; RETURN NEW; END; $$; CREATE TRIGGER fail_feedback_audit BEFORE INSERT ON maintenance_audit FOR EACH ROW EXECUTE FUNCTION fail_feedback_audit();`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER fail_feedback_audit ON maintenance_audit; DROP FUNCTION fail_feedback_audit()`)
	})
	v.ExternalID = "rollback"
	if _, err = s.Import(ctx, v, strings.NewReader(feedbackRaw("one@example.test"))); err == nil {
		t.Fatal("audit failure ignored")
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM feedback_reports WHERE source=$1 AND external_id='rollback'`, v.Source).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial import", count, err)
	}
	if err = s.Dismiss(ctx, r.Entry.ID, r.Entry.SHA256, "op", "dismiss"); err == nil {
		t.Fatal("review audit failure ignored")
	}
	e, err := s.Get(ctx, r.Entry.ID)
	if err != nil || e.State != "QUARANTINED" {
		t.Fatal("partial review", e, err)
	}
}
