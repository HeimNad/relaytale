package integration

import (
	"context"
	"github.com/google/uuid"
	"relaytale/internal/feedback"
	"strings"
	"testing"
)

func TestExplainStoredNeverRewritesEvidence(t *testing.T) {
	db := testDB(t)
	s := feedback.Service{DB: db}
	ctx := context.Background()
	raw := strings.Replace(feedbackRaw("target@example.test"), "Status: 5.1.1", "Status: 554", 1)
	r, e := s.Import(ctx, feedback.ImportRequest{Source: uuid.NewString(), Actor: "test", Reason: "nonstandard fixture"}, strings.NewReader(raw))
	if e != nil {
		t.Fatal(e)
	}
	if r.Entry.ParseState != "UNSUPPORTED" {
		t.Fatal(r)
	}
	var before, after int
	if e = db.QueryRow(`SELECT count(*) FROM maintenance_audit`).Scan(&before); e != nil {
		t.Fatal(e)
	}
	x, e := s.ExplainStored(ctx, r.Entry.ID, r.Entry.SHA256)
	if e != nil || x.StrictlyParsed || x.AutomaticActionAllowed || x.Blocks[1].Hints.SMTPCode != 554 {
		t.Fatal(x, e)
	}
	if _, e = s.ExplainStored(ctx, r.Entry.ID, strings.Repeat("0", 64)); e == nil {
		t.Fatal("wrong digest accepted")
	}
	current, e := s.Get(ctx, r.Entry.ID)
	if e != nil || current.ParseState != "UNSUPPORTED" || current.Trust != "UNVERIFIED" || current.State != "QUARANTINED" || string(current.Report) != "{}" {
		t.Fatal(current, e)
	}
	if e = db.QueryRow(`SELECT count(*) FROM maintenance_audit`).Scan(&after); e != nil || before != after {
		t.Fatal("read added audit writes", before, after, e)
	}
	var stored []byte
	if e = db.QueryRow(`SELECT raw FROM feedback_reports WHERE id=$1`, r.Entry.ID).Scan(&stored); e != nil || string(stored) != raw {
		t.Fatal("raw changed", e)
	}
}
