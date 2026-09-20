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

func TestFeedbackCandidatesPreserveUncertainty(t *testing.T) {
	for _, mode := range []string{"accepted", "unknown", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			f := delivery(t, testsmtp.Options{DropFinal: mode == "unknown"}, 1)
			ctx := context.Background()
			address := "one@" + f.domain
			mid := suppressionSeed(t, f, address)
			hint := "<" + uuid.NewString() + "@example.test>"
			mustExec(t, f, `UPDATE messages SET message_id=$2 WHERE id=$1`, mid, hint)
			runOne(t, f.worker)
			raw := feedbackRaw(address)
			if mode == "unsupported" {
				raw = strings.Replace(raw, "Status: 5.1.1", "Status: 554", 1)
			}
			s := feedback.Service{DB: f.db}
			entry, e := s.Import(ctx, feedback.ImportRequest{Source: uuid.NewString(), Actor: "test", Reason: "candidate discovery"}, strings.NewReader(raw))
			if e != nil {
				t.Fatal(e)
			}
			req := feedback.CandidateRequest{FeedbackID: entry.Entry.ID, ExpectedSHA256: entry.Entry.SHA256, RFCMessageID: hint, Limit: 20}
			result, e := s.Candidates(ctx, req)
			if e != nil || len(result.Candidates) != 1 || result.AutomaticActionAllowed || result.Truncated || result.MultipleMessages {
				t.Fatal(result, e)
			}
			c := result.Candidates[0]
			if c.Request.MessageID != mid || !c.SnapshotAt.Equal(result.SnapshotAt) || c.Assessment.Action != "QUARANTINE" || c.Assessment.AutomaticSuppression {
				t.Fatal(c)
			}
			if mode == "unknown" && !slices.Contains(c.Assessment.Reasons, "DELIVERY_UNKNOWN_PRESERVED") {
				t.Fatal(c)
			}
			if mode == "unsupported" && (c.Assessment.CandidateMatches || !slices.Contains(c.Assessment.Reasons, "UNSUPPORTED_REPORT")) {
				t.Fatal(c)
			}
			if mode == "accepted" && !c.Assessment.CandidateMatches {
				t.Fatal(c)
			}
			wrong := req
			wrong.ExpectedSHA256 = strings.Repeat("0", 64)
			if _, e = s.Candidates(ctx, wrong); e == nil {
				t.Fatal("wrong digest accepted")
			}
			wrong = req
			wrong.RFCMessageID = "<absent-" + uuid.NewString() + "@example.test>"
			empty, e := s.Candidates(ctx, wrong)
			if e != nil || len(empty.Candidates) != 0 || !slices.Contains(empty.Reasons, "NO_MESSAGE_MATCH") {
				t.Fatal(empty, e)
			}
			assertCount(t, f, 1, `SELECT count(*) FROM maintenance_audit WHERE data->>'id'=$1`, entry.Entry.ID)
			assertCount(t, f, 0, `SELECT count(*) FROM suppression_entries WHERE email=$1`, address)
			expected := smtpclient.Accepted
			if mode == "unknown" {
				expected = smtpclient.Unknown
			}
			if f.status(t, mid) != expected {
				t.Fatal("search changed delivery")
			}
		})
	}
}
func TestFeedbackCandidatesDuplicateIDsAndTruncation(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	ctx := context.Background()
	address := "one@" + f.domain
	mid := suppressionSeed(t, f, address)
	hint := "<" + uuid.NewString() + "@example.test>"
	mustExec(t, f, `UPDATE messages SET message_id=$2 WHERE id=$1`, mid, hint)
	runOne(t, f.worker)
	s := feedback.Service{DB: f.db}
	entry, e := s.Import(ctx, feedback.ImportRequest{Source: uuid.NewString(), Actor: "test", Reason: "duplicates"}, strings.NewReader(feedbackRaw(address)))
	if e != nil {
		t.Fatal(e)
	}
	req := feedback.CandidateRequest{FeedbackID: entry.Entry.ID, ExpectedSHA256: entry.Entry.SHA256, RFCMessageID: hint, Limit: 20}
	another := suppressionSeed(t, f, address)
	mustExec(t, f, `UPDATE messages SET message_id=$2 WHERE id=$1`, another, hint)
	r, e := s.Candidates(ctx, req)
	if e != nil || !r.MultipleMessages || len(r.Candidates) != 1 || r.Candidates[0].Assessment.CandidateMatches {
		t.Fatal(r, e)
	}
	// A second message with no attempts must still make Message-ID ambiguous.
	mustExec(t, f, `UPDATE messages SET message_id=$2 WHERE id=$1`, another, "<different@example.test>")
	var rid string
	if e = f.db.QueryRow(`SELECT id FROM recipients WHERE message_id=$1`, mid).Scan(&rid); e != nil {
		t.Fatal(e)
	}
	aid := uuid.NewString()
	mustExec(t, f, `INSERT INTO delivery_attempts(id,message_id,provider_id,attempt_number,result,finished_at) VALUES($1,$2,$3,2,'TEMP_FAILED',now())`, aid, mid, providerID(t, f))
	mustExec(t, f, `INSERT INTO attempt_recipients(attempt_id,recipient_id,status) VALUES($1,$2,'TEMP_FAILED')`, aid, rid)
	req.Limit = 1
	r, e = s.Candidates(ctx, req)
	if e != nil || !r.Truncated || len(r.Candidates) != 1 || r.Candidates[0].Assessment.CandidateMatches || !slices.Contains(r.Reasons, "CANDIDATES_TRUNCATED") {
		t.Fatal(r, e)
	}
	req.Limit = 20
	r, e = s.Candidates(ctx, req)
	if e != nil || r.Truncated || len(r.Candidates) != 2 {
		t.Fatal(r, e)
	}
	if !slices.Contains(r.Candidates[1].Assessment.Reasons, "NEWER_RECIPIENT_ATTEMPT") {
		t.Fatal(r)
	}
	req.RFCMessageID = "<different@example.test>"
	r, e = s.Candidates(ctx, req)
	if e != nil || !slices.Contains(r.Reasons, "NO_ATTEMPT_CANDIDATES") {
		t.Fatal(r, e)
	}
}
