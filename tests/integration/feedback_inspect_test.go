package integration

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"relaytale/internal/feedback"
	"relaytale/internal/smtpclient"
	"relaytale/internal/testsmtp"
)

func TestFeedbackInspectionCandidateAndMismatch(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	ctx := context.Background()
	address := "one@" + f.domain
	mid := suppressionSeed(t, f, address)
	runOne(t, f.worker)
	s := feedback.Service{DB: f.db}
	req := feedback.InspectRequest{MessageID: mid, ProviderID: providerID(t, f)}
	if err := f.db.QueryRow(`SELECT r.id,a.id FROM recipients r JOIN delivery_attempts a ON a.message_id=r.message_id WHERE r.message_id=$1`, mid).Scan(&req.RecipientID, &req.AttemptID); err != nil {
		t.Fatal(err)
	}
	imported, err := s.Import(ctx, feedback.ImportRequest{Source: uuid.NewString(), Actor: "test", Reason: "forged source"}, strings.NewReader(feedbackRaw(address)))
	if err != nil {
		t.Fatal(err)
	}
	req.FeedbackID = imported.Entry.ID
	req.ExpectedSHA256 = imported.Entry.SHA256
	result, err := s.Inspect(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Assessment.CandidateMatches || result.Assessment.AutomaticSuppression || result.Assessment.Action != "QUARANTINE" || result.CurrentStatus != smtpclient.Accepted || result.SnapshotAt.IsZero() {
		t.Fatal(result)
	}
	if slices.Contains(result.Assessment.Reasons, "NO_STABLE_SMTP_ACCEPTANCE") {
		t.Fatal(result)
	}
	for _, field := range []string{"provider", "message", "recipient", "attempt", "digest"} {
		t.Run(field, func(t *testing.T) {
			other := req
			reason := "RELATION_MISMATCH"
			switch field {
			case "provider":
				other.ProviderID = uuid.NewString()
			case "message":
				other.MessageID = uuid.NewString()
			case "recipient":
				other.RecipientID = uuid.NewString()
			case "attempt":
				other.AttemptID = uuid.NewString()
			case "digest":
				other.ExpectedSHA256 = strings.Repeat("0", 64)
				reason = "EVIDENCE_DIGEST_MISMATCH"
			}
			r, e := s.Inspect(ctx, other)
			if e != nil || r.Assessment.CandidateMatches || !slices.Contains(r.Assessment.Reasons, reason) {
				t.Fatal(r, e)
			}
		})
	}
	req.ReportIndex = 1
	if _, err = s.Inspect(ctx, req); err == nil {
		t.Fatal("missing report index accepted")
	}
	req.ReportIndex = 0
	// A newer historical attempt makes the previous candidate stale. Do not
	// rewrite its accepted evidence or schedule an artificial worker retry.
	mustExec(t, f, `INSERT INTO delivery_attempts(id,message_id,provider_id,attempt_number,result,finished_at) VALUES($1,$2,$3,2,'TEMP_FAILED',now())`, uuid.NewString(), mid, req.ProviderID)
	mustExec(t, f, `INSERT INTO attempt_recipients(attempt_id,recipient_id,status) SELECT id,$2,'TEMP_FAILED' FROM delivery_attempts WHERE message_id=$1 AND attempt_number=2`, mid, req.RecipientID)
	result, err = s.Inspect(ctx, req)
	if err != nil || !slices.Contains(result.Assessment.Reasons, "NEWER_RECIPIENT_ATTEMPT") {
		t.Fatal(result, err)
	}
	assertCount(t, f, 1, `SELECT count(*) FROM maintenance_audit WHERE data->>'id'=$1`, req.FeedbackID)
	assertCount(t, f, 0, `SELECT count(*) FROM suppression_entries WHERE email=$1`, address)
	if f.status(t, mid) != smtpclient.Accepted {
		t.Fatal("inspection changed message")
	}
}
func TestFeedbackInspectionUnknownAndAmbiguousReports(t *testing.T) {
	f := delivery(t, testsmtp.Options{DropFinal: true}, 1)
	ctx := context.Background()
	address := "one@" + f.domain
	mid := suppressionSeed(t, f, address)
	runOne(t, f.worker)
	s := feedback.Service{DB: f.db}
	req := feedback.InspectRequest{MessageID: mid, ProviderID: providerID(t, f)}
	if err := f.db.QueryRow(`SELECT r.id,a.id FROM recipients r JOIN delivery_attempts a ON a.message_id=r.message_id WHERE r.message_id=$1`, mid).Scan(&req.RecipientID, &req.AttemptID); err != nil {
		t.Fatal(err)
	}
	raw := feedbackRaw(address)
	duplicate := strings.Replace(raw, "\r\n--dsn--", "\r\nFinal-Recipient: rfc822; "+address+"\r\nAction: delayed\r\nStatus: 4.2.2\r\n\r\n--dsn--", 1)
	for _, sample := range []struct{ name, raw, reason string }{
		{"unknown", raw, "DELIVERY_UNKNOWN_PRESERVED"},
		{"duplicate", duplicate, "AMBIGUOUS_RECIPIENT_BLOCKS"},
		{"rewritten", strings.Replace(raw, "Action: failed", "Original-Recipient: rfc822; old@example.test\r\nAction: failed", 1), "RECIPIENT_REWRITTEN"},
		{"unsupported", "not a report", "UNSUPPORTED_REPORT"},
	} {
		t.Run(sample.name, func(t *testing.T) {
			imported, err := s.Import(ctx, feedback.ImportRequest{Source: uuid.NewString(), Actor: "test", Reason: "unverified"}, strings.NewReader(sample.raw))
			if err != nil {
				t.Fatal(err)
			}
			req.FeedbackID = imported.Entry.ID
			req.ExpectedSHA256 = imported.Entry.SHA256
			result, err := s.Inspect(ctx, req)
			if err != nil || !slices.Contains(result.Assessment.Reasons, sample.reason) || result.Assessment.AutomaticSuppression {
				t.Fatal(result, err)
			}
			if (sample.name == "duplicate" || sample.name == "rewritten") && result.Assessment.CandidateMatches {
				t.Fatal("ambiguous mapping accepted")
			}
		})
	}
	if f.status(t, mid) != smtpclient.Unknown {
		t.Fatal("UNKNOWN overwritten")
	}
	assertCount(t, f, 0, `SELECT count(*) FROM suppression_entries WHERE email=$1`, address)
}
