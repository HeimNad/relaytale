package feedback

import (
	"slices"
	"testing"
)

func matchingEvidence() AssociationEvidence {
	return AssociationEvidence{RecordState: "QUARANTINED", Parsed: true, RelationMatches: true, ExpectedDigestMatches: true, Address: "User@example.test", CurrentStatus: "SMTP_ACCEPTED", AttemptStatus: "SMTP_ACCEPTED", FinalAccepted: true, LatestAttempt: true, Selected: Recipient{FinalRecipient: "rfc822; User@EXAMPLE.test", Action: "failed", Status: "5.1.1"}, SameAddressRows: 1}
}
func TestFeedbackAssessmentNeverGrantsTrust(t *testing.T) {
	e := matchingEvidence()
	d := Assess(e)
	if !d.CandidateMatches || d.AutomaticSuppression || d.Action != "QUARANTINE" || d.Classification != "ADDRESS_INVALID_REPORTED" || !slices.Contains(d.Reasons, "SOURCE_UNVERIFIED") {
		t.Fatal(d)
	}
	cases := []struct {
		name, reason string
		change       func(*AssociationEvidence)
	}{
		{"wrong-digest", "EVIDENCE_DIGEST_MISMATCH", func(e *AssociationEvidence) { e.ExpectedDigestMatches = false }},
		{"wrong-membership", "RELATION_MISMATCH", func(e *AssociationEvidence) { e.RelationMatches = false }},
		{"conflict", "SOURCE_EVENT_CONFLICT", func(e *AssociationEvidence) { e.RecordState = "CONFLICT" }},
		{"dismissed", "FEEDBACK_DISMISSED", func(e *AssociationEvidence) { e.RecordState = "DISMISSED" }},
		{"rewritten", "RECIPIENT_REWRITTEN", func(e *AssociationEvidence) { e.Selected.OriginalRecipient = "rfc822; other@example.test" }},
		{"duplicate-recipient", "AMBIGUOUS_RECIPIENT_BLOCKS", func(e *AssociationEvidence) { e.SameAddressRows = 2 }},
		{"stale-attempt", "NEWER_RECIPIENT_ATTEMPT", func(e *AssociationEvidence) { e.LatestAttempt = false }},
		{"unknown", "DELIVERY_UNKNOWN_PRESERVED", func(e *AssociationEvidence) {
			e.CurrentStatus = "DELIVERY_UNKNOWN"
			e.AttemptStatus = "DELIVERY_UNKNOWN"
		}},
		{"no-final-response", "NO_STABLE_SMTP_ACCEPTANCE", func(e *AssociationEvidence) { e.FinalAccepted = false }},
		{"old-report", "REPORT_PREDATES_ATTEMPT", func(e *AssociationEvidence) { e.PredatesAttempt = true }},
		{"foreign-address", "UNSUPPORTED_RECIPIENT_ADDRESS", func(e *AssociationEvidence) { e.Selected.FinalRecipient = "x400; opaque" }},
		{"local-part-case", "RECIPIENT_ADDRESS_MISMATCH", func(e *AssociationEvidence) { e.Selected.FinalRecipient = "rfc822; user@example.test" }},
		{"alias", "RECIPIENT_ADDRESS_MISMATCH", func(e *AssociationEvidence) { e.Selected.FinalRecipient = "rfc822; User+tag@example.test" }},
	}
	for _, v := range cases {
		t.Run(v.name, func(t *testing.T) {
			e := matchingEvidence()
			v.change(&e)
			d := Assess(e)
			if !slices.Contains(d.Reasons, v.reason) || d.AutomaticSuppression || d.Action != "QUARANTINE" {
				t.Fatal(d)
			}
		})
	}
}
func TestFeedbackClassification(t *testing.T) {
	for _, v := range []struct{ action, code, want string }{
		{"failed", "5.1.1", "ADDRESS_INVALID_REPORTED"}, {"failed", "5.7.1", "PERMANENT_FAILURE_REPORTED"},
		{"failed", "5.2.2", "PERMANENT_FAILURE_REPORTED"}, {"failed", "5.1.3", "PERMANENT_FAILURE_REPORTED"},
		{"delayed", "4.1.1", "TEMPORARY_FAILURE_REPORTED"}, {"delivered", "2.0.0", "DELIVERY_REPORTED"},
		{"relayed", "2.0.0", "RELAY_REPORTED"}, {"expanded", "2.0.0", "EXPANSION_REPORTED"},
		{"failed", "4.1.1", "UNCLASSIFIED"}, {"made-up", "5.1.1", "UNCLASSIFIED"},
	} {
		r := Recipient{Action: v.action, Status: v.code, Diagnostic: "550 unknown user"}
		if got := classify(r); got != v.want {
			t.Fatal(v, got)
		}
	}
}
