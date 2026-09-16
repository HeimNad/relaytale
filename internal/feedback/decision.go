package feedback

import (
	"net/mail"
	"strings"
)

// Assessment is a read-only explanation, not permission to mutate delivery.
// No caller-supplied trust flag can turn a mailbox export into verified feedback.
type Assessment struct {
	Classification       string   `json:"classification"`
	CandidateMatches     bool     `json:"candidate_matches"`
	Action               string   `json:"action"`
	AutomaticSuppression bool     `json:"automatic_suppression"`
	Reasons              []string `json:"reasons"`
}

type AssociationEvidence struct {
	RecordState           string
	Parsed                bool
	RelationMatches       bool
	ExpectedDigestMatches bool
	Address               string
	CurrentStatus         string
	AttemptStatus         string
	FinalAccepted         bool
	LatestAttempt         bool
	PredatesAttempt       bool
	Selected              Recipient
	SameAddressRows       int
}

// Assess keeps reported claims separate from both provenance and SMTP facts.
// A future authenticated adapter must use a separate verified-evidence type and
// transaction, rather than promoting this result or accepting a "trusted" flag.
func Assess(e AssociationEvidence) Assessment {
	d := Assessment{Classification: classify(e.Selected), Action: "QUARANTINE", Reasons: []string{"SOURCE_UNVERIFIED"}}
	add := func(reason string) { d.Reasons = append(d.Reasons, reason) }
	if !e.ExpectedDigestMatches {
		add("EVIDENCE_DIGEST_MISMATCH")
	}
	switch e.RecordState {
	case "CONFLICT":
		add("SOURCE_EVENT_CONFLICT")
	case "DISMISSED":
		add("FEEDBACK_DISMISSED")
	}
	if !e.Parsed {
		add("UNSUPPORTED_REPORT")
	}
	if !e.RelationMatches {
		add("RELATION_MISMATCH")
	}
	final, ok := reportMailbox(e.Selected.FinalRecipient)
	if !ok {
		add("UNSUPPORTED_RECIPIENT_ADDRESS")
	} else if !sameMailbox(final, e.Address) {
		add("RECIPIENT_ADDRESS_MISMATCH")
	}
	rewritten := false
	if original := e.Selected.OriginalRecipient; original != "" {
		address, valid := reportMailbox(original)
		if !valid || !sameMailbox(address, final) {
			rewritten = true
			add("RECIPIENT_REWRITTEN")
		}
	}
	if e.SameAddressRows != 1 {
		add("AMBIGUOUS_RECIPIENT_BLOCKS")
	}
	if e.PredatesAttempt {
		add("REPORT_PREDATES_ATTEMPT")
	}
	d.CandidateMatches = e.ExpectedDigestMatches && e.Parsed && e.RelationMatches && ok && sameMailbox(final, e.Address) && e.SameAddressRows == 1 && !rewritten && !e.PredatesAttempt
	if e.RelationMatches && !e.LatestAttempt {
		add("NEWER_RECIPIENT_ATTEMPT")
	}
	if e.CurrentStatus == "DELIVERY_UNKNOWN" || e.AttemptStatus == "DELIVERY_UNKNOWN" {
		add("DELIVERY_UNKNOWN_PRESERVED")
	}
	if e.RelationMatches && (e.CurrentStatus != "SMTP_ACCEPTED" || e.AttemptStatus != "SMTP_ACCEPTED" || !e.FinalAccepted) {
		add("NO_STABLE_SMTP_ACCEPTANCE")
	}
	switch d.Classification {
	case "ADDRESS_INVALID_REPORTED":
		add("ADDRESS_FAILURE_REQUIRES_VERIFIED_SOURCE")
	case "TEMPORARY_FAILURE_REPORTED":
		add("TEMPORARY_FAILURE_NOT_ADDRESS_SUPPRESSION")
	case "PERMANENT_FAILURE_REPORTED":
		add("PERMANENT_FAILURE_NOT_PROOF_OF_INVALID_ADDRESS")
	case "DELIVERY_REPORTED", "RELAY_REPORTED", "EXPANSION_REPORTED":
		add("REPORT_DOES_NOT_PROVE_INBOX_DELIVERY")
	default:
		add("UNCLASSIFIED_REPORT")
	}
	return d
}
func classify(r Recipient) string {
	if !statusPattern.MatchString(r.Status) {
		return "UNCLASSIFIED"
	}
	switch r.Action {
	case "failed":
		if r.Status == "5.1.1" {
			return "ADDRESS_INVALID_REPORTED"
		}
		if r.Status[0] == '5' {
			return "PERMANENT_FAILURE_REPORTED"
		}
	case "delayed":
		if r.Status[0] == '4' {
			return "TEMPORARY_FAILURE_REPORTED"
		}
	case "delivered":
		if r.Status[0] == '2' {
			return "DELIVERY_REPORTED"
		}
	case "relayed":
		if r.Status[0] == '2' {
			return "RELAY_REPORTED"
		}
	case "expanded":
		if r.Status[0] == '2' {
			return "EXPANSION_REPORTED"
		}
	}
	return "UNCLASSIFIED"
}
func reportMailbox(s string) (string, bool) {
	kind, address, ok := strings.Cut(s, ";")
	address = strings.TrimSpace(address)
	if !ok || !strings.EqualFold(strings.TrimSpace(kind), "rfc822") {
		return "", false
	}
	a, err := mail.ParseAddress(address)
	if err != nil || a.Address != address || len(address) > 320 || strings.ContainsAny(address, "\r\n\x00") {
		return "", false
	}
	for _, c := range address {
		if c > 127 {
			return "", false
		}
	}
	return address, true
}

// Preserve local-part case and aliases. Only domain case is insignificant here.
func sameMailbox(a, b string) bool {
	ai, bi := strings.LastIndexByte(a, '@'), strings.LastIndexByte(b, '@')
	return ai > 0 && bi > 0 && a[:ai] == b[:bi] && strings.EqualFold(a[ai+1:], b[bi+1:])
}
