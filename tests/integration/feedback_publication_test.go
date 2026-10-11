package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"relaytale/internal/api"
	"relaytale/internal/feedback"
	"relaytale/internal/testsmtp"
	"relaytale/internal/webhook"
)

func TestManualFeedbackWebsiteLifecycle(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, receiver := sendFixture(t, f)
	mid := submitStatusFixture(t, h, key, f.raw(), nil)
	runOne(t, f.worker)
	ctx := context.Background()
	s := feedback.Service{DB: f.db}
	source := uuid.NewString()
	var beforeSuppression int
	if err := f.db.QueryRow(`SELECT count(*) FROM suppression_entries`).Scan(&beforeSuppression); err != nil {
		t.Fatal(err)
	}
	imported, err := s.Import(ctx, feedback.ImportRequest{Source: source, ExternalID: "bounce-1", Actor: "test", Reason: "controlled synthetic evidence"}, strings.NewReader(feedbackRaw("one@example.test")))
	if err != nil {
		t.Fatal(err)
	}
	req := feedback.LinkRequest{InspectRequest: feedback.InspectRequest{FeedbackID: imported.Entry.ID, ExpectedSHA256: imported.Entry.SHA256, MessageID: mid, ProviderID: providerID(t, f)}, Reason: "human compared recipient and original mail", AcknowledgeUnverified: true}
	if err = f.db.QueryRow(`SELECT r.id,a.id FROM recipients r JOIN delivery_attempts a ON a.message_id=r.message_id WHERE r.message_id=$1`, mid).Scan(&req.RecipientID, &req.AttemptID); err != nil {
		t.Fatal(err)
	}
	endpoint := uuid.NewString()
	if err = webhook.Sync(ctx, f.db, []webhook.Endpoint{{ID: endpoint, APIKeyID: key.ID, URL: "https://example.com/hook", Keys: []webhook.Key{{ID: "v1", Secret: strings.Repeat("ab", 32)}}}}); err != nil {
		t.Fatal(err)
	}
	admin := management(t, f)
	path := "/feedback/" + req.FeedbackID + "/links"
	requireStatus(t, adminRequest(admin, "POST", path, "v", req), 403)
	requireStatus(t, adminRequest(admin, "POST", path, "o", req), 403)
	bad := req
	bad.AcknowledgeUnverified = false
	requireStatus(t, adminRequest(admin, "POST", path, "a", bad), 400)
	bad = req
	bad.ExpectedSHA256 = strings.Repeat("0", 64)
	requireStatus(t, adminRequest(admin, "POST", path, "a", bad), 409)
	linked := adminRequest(admin, "POST", path, "a", req)
	requireStatus(t, linked, 201)
	var link feedback.Publication
	if err = json.Unmarshal(linked.Body.Bytes(), &link); err != nil {
		t.Fatal(err)
	}
	if link.State != "PENDING_REVIEW" || link.SourceTrust != "UNVERIFIED" || link.Revision != 0 {
		t.Fatal(link)
	}
	requireStatus(t, adminRequest(admin, "POST", path, "a", req), 409)
	w := statusRequest(h, key, mid, "/feedback?limit=1", nil)
	requireStatus(t, w, 200)
	if !strings.Contains(w.Body.String(), "PENDING_REVIEW") || strings.Contains(w.Body.String(), req.FeedbackID) || strings.Contains(w.Body.String(), "Diagnostic") || strings.Contains(w.Body.String(), req.ProviderID) {
		t.Fatal(w.Body)
	}
	foreign := key
	foreign.ID = uuid.NewString()
	foreign.Token = "rts_" + strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")
	config, _ := json.Marshal([]api.SendKey{key, foreign})
	foreignHandler, e := api.NewSendHandler(ctx, string(config), receiver)
	if e != nil {
		t.Fatal(e)
	}
	requireStatus(t, statusRequest(foreignHandler, foreign, mid, "/feedback", nil), 404)
	other := key
	other.Token = "rts_" + strings.Repeat("0", 64)
	requireStatus(t, statusRequest(h, other, mid, "/feedback", nil), 401)
	for _, q := range []string{"/feedback?limit=101", "/feedback?limit=1&limit=2", "/feedback?raw=true"} {
		requireStatus(t, statusRequest(h, key, mid, q, nil), 400)
	}
	review := feedback.ReviewRequest{Action: "CONFIRM_FAILURE", Reason: "human reviewed report and mailbox evidence", AcknowledgeUnverified: true}
	result, err := s.Review(ctx, link.ID, review, "human")
	if err != nil || result.State != "FAILURE_CONFIRMED_BY_OPERATOR" || result.Revision != 1 {
		t.Fatal(result, err)
	}
	if _, err = s.Review(ctx, link.ID, review, "human"); !errors.Is(err, feedback.ErrPublication) {
		t.Fatal(err)
	}
	requireStatus(t, statusRequest(h, key, mid, "/feedback", nil), 200)
	if f.status(t, mid) != "SMTP_ACCEPTED" {
		t.Fatal("human review rewrote SMTP fact")
	}
	assertCount(t, f, beforeSuppression, `SELECT count(*) FROM suppression_entries`)
	assertCount(t, f, 2, `SELECT count(*) FROM webhook_outbox WHERE message_id=$1 AND payload::jsonb->>'feedback_availability'='MANUAL_REVIEW'`, mid)
	// A late same-event content conflict must revoke effective conclusions and notify.
	_, err = s.Import(ctx, feedback.ImportRequest{Source: source, ExternalID: "bounce-1", Actor: "test", Reason: "conflicting evidence"}, strings.NewReader(strings.Replace(feedbackRaw("one@example.test"), "5.1.1", "5.1.2", 1)))
	if err != nil {
		t.Fatal(err)
	}
	w = statusRequest(h, key, mid, "/feedback", nil)
	requireStatus(t, w, 200)
	if !strings.Contains(w.Body.String(), "EVIDENCE_CONFLICT") {
		t.Fatal(w.Body)
	}
	assertCount(t, f, 1, `SELECT count(*) FROM events WHERE message_id=$1 AND event_type='FEEDBACK_INVALIDATED'`, mid)
	review.ExpectedRevision = 1
	review.Action = "WITHDRAW"
	if _, err = s.Review(ctx, link.ID, review, "human"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Review(ctx, link.ID, review, "human"); !errors.Is(err, feedback.ErrPublication) {
		t.Fatal(err)
	}
	assertCount(t, f, 2, `SELECT count(*) FROM feedback_link_reviews WHERE link_id=$1`, link.ID)
	if _, err = f.db.Exec(`UPDATE feedback_links SET reason='rewrite' WHERE id=$1`, link.ID); err == nil {
		t.Fatal("link history mutable")
	}
}

func TestManualFeedbackReviewConcurrentAndUnsupported(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sendFixture(t, f)
	mid := submitStatusFixture(t, h, key, f.raw(), nil)
	runOne(t, f.worker)
	ctx := context.Background()
	s := feedback.Service{DB: f.db}
	imported, err := s.Import(ctx, feedback.ImportRequest{Source: uuid.NewString(), Actor: "test", Reason: "nonstandard report"}, strings.NewReader("Subject: nonstandard\r\n\r\nnot a standard DSN"))
	if err != nil {
		t.Fatal(err)
	}
	req := feedback.LinkRequest{InspectRequest: feedback.InspectRequest{FeedbackID: imported.Entry.ID, ExpectedSHA256: imported.Entry.SHA256, MessageID: mid, ProviderID: providerID(t, f)}, Reason: "manual nonstandard association", AcknowledgeUnverified: true}
	if err = f.db.QueryRow(`SELECT r.id,a.id FROM recipients r JOIN delivery_attempts a ON a.message_id=r.message_id WHERE r.message_id=$1`, mid).Scan(&req.RecipientID, &req.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Link(ctx, req, "human"); !errors.Is(err, feedback.ErrPublication) {
		t.Fatal(err)
	}
	req.ManualAssociation = true
	link, err := s.Link(ctx, req, "human")
	if err != nil {
		t.Fatal(err)
	}
	review := feedback.ReviewRequest{Action: "CONFIRM_FAILURE", Reason: "manual evidence checked", AcknowledgeUnverified: true}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() { _, e := s.Review(ctx, link.ID, review, "human"); results <- e })
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for e := range results {
		if e == nil {
			success++
		} else if errors.Is(e, feedback.ErrPublication) {
			conflict++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal(success, conflict)
	}
	if err = s.Dismiss(ctx, imported.Entry.ID, imported.Entry.SHA256, "human", "wrong association"); err != nil {
		t.Fatal(err)
	}
	w := statusRequest(h, key, mid, "/feedback", nil)
	requireStatus(t, w, http.StatusOK)
	if !strings.Contains(w.Body.String(), "WITHDRAWN") {
		t.Fatal(w.Body)
	}
	assertCount(t, f, 1, `SELECT count(*) FROM events WHERE message_id=$1 AND event_type='FEEDBACK_INVALIDATED'`, mid)
}

func TestManualFeedbackRejectsUnknownAndRollsBackAuditFailure(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "audit rollback", true: "unknown preserved"}[unknown], func(t *testing.T) {
			f := delivery(t, testsmtp.Options{DropFinal: unknown}, 1)
			h, key, _ := sendFixture(t, f)
			mid := submitStatusFixture(t, h, key, f.raw(), nil)
			runOne(t, f.worker)
			ctx := context.Background()
			s := feedback.Service{DB: f.db}
			imported, err := s.Import(ctx, feedback.ImportRequest{Source: uuid.NewString(), Actor: "test", Reason: "fault injection"}, strings.NewReader(feedbackRaw("one@example.test")))
			if err != nil {
				t.Fatal(err)
			}
			req := feedback.LinkRequest{InspectRequest: feedback.InspectRequest{FeedbackID: imported.Entry.ID, ExpectedSHA256: imported.Entry.SHA256, MessageID: mid, ProviderID: providerID(t, f)}, Reason: "manual association", AcknowledgeUnverified: true}
			if err = f.db.QueryRow(`SELECT r.id,a.id FROM recipients r JOIN delivery_attempts a ON a.message_id=r.message_id WHERE r.message_id=$1`, mid).Scan(&req.RecipientID, &req.AttemptID); err != nil {
				t.Fatal(err)
			}
			if !unknown {
				name := "reject_manual_audit_" + strings.ReplaceAll(uuid.NewString(), "-", "")
				mustExec(t, f, `CREATE FUNCTION `+name+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='FEEDBACK_LINKED' THEN RAISE EXCEPTION 'injected audit failure'; END IF; RETURN NEW; END $$`)
				mustExec(t, f, `CREATE TRIGGER `+name+` BEFORE INSERT ON maintenance_audit FOR EACH ROW EXECUTE FUNCTION `+name+`() `)
				t.Cleanup(func() {
					f.db.Exec(`DROP TRIGGER IF EXISTS ` + name + ` ON maintenance_audit`)
					f.db.Exec(`DROP FUNCTION IF EXISTS ` + name + `()`)
				})
			}
			if _, err = s.Link(ctx, req, "human"); err == nil {
				t.Fatal("unsafe or unaudited publication accepted")
			}
			assertCount(t, f, 0, `SELECT count(*) FROM feedback_links WHERE message_id=$1`, mid)
			assertCount(t, f, 0, `SELECT count(*) FROM events WHERE message_id=$1 AND event_type='FEEDBACK_LINKED'`, mid)
			if unknown && f.status(t, mid) != "DELIVERY_UNKNOWN" {
				t.Fatal("UNKNOWN rewritten")
			}
		})
	}
}
