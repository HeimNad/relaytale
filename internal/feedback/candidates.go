package feedback

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

type CandidateRequest struct {
	FeedbackID     string `json:"feedback_id"`
	ExpectedSHA256 string `json:"expected_sha256"`
	RFCMessageID   string `json:"rfc_message_id"`
	ReportIndex    int    `json:"report_index"`
	Limit          int    `json:"limit"`
}
type CandidateSearch struct {
	Request                CandidateRequest `json:"request"`
	SnapshotAt             time.Time        `json:"snapshot_at"`
	HintSource             string           `json:"hint_source"`
	AutomaticActionAllowed bool             `json:"automatic_action_allowed"`
	MultipleMessages       bool             `json:"multiple_messages"`
	Truncated              bool             `json:"truncated"`
	Reasons                []string         `json:"reasons"`
	Candidates             []Inspection     `json:"candidates"`
}

// Candidates treats the operator's exact Message-ID solely as a search hint.
// It never extracts identifiers from returned MIME or compatibility hints.
func (s Service) Candidates(ctx context.Context, v CandidateRequest) (CandidateSearch, error) {
	out := CandidateSearch{Request: v, HintSource: "OPERATOR_SUPPLIED_UNVERIFIED", Reasons: []string{"SOURCE_UNVERIFIED", "MESSAGE_ID_IS_NOT_PROOF"}, Candidates: []Inspection{}}
	if _, e := uuid.Parse(v.FeedbackID); e != nil {
		return out, errors.New("valid feedback UUID required")
	}
	d, e := hex.DecodeString(v.ExpectedSHA256)
	if e != nil || len(d) != 32 || hex.EncodeToString(d) != v.ExpectedSHA256 || v.RFCMessageID == "" || len(v.RFCMessageID) > 998 || strings.TrimSpace(v.RFCMessageID) != v.RFCMessageID || strings.ContainsAny(v.RFCMessageID, "\x00\r\n") || v.Limit < 1 || v.Limit > 50 || v.ReportIndex < 0 || v.ReportIndex >= MaxRecipients {
		return out, errors.New("invalid candidate search bounds or digest")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SET LOCAL statement_timeout='5s'`); e != nil {
		return out, e
	}
	if e = tx.QueryRowContext(ctx, `SELECT transaction_timestamp()`).Scan(&out.SnapshotAt); e != nil {
		return out, e
	}
	var raw, parsed []byte
	var digest, state string
	if e = tx.QueryRowContext(ctx, `SELECT raw,sha256,parse_state,parsed FROM feedback_reports WHERE id=$1`, v.FeedbackID).Scan(&raw, &digest, &state, &parsed); e != nil {
		return out, e
	}
	actual := sha256.Sum256(raw)
	if digest != v.ExpectedSHA256 || hex.EncodeToString(actual[:]) != v.ExpectedSHA256 {
		return out, errors.New("evidence digest mismatch")
	}
	if state == "PARSED" {
		var report Report
		if json.Unmarshal(parsed, &report) != nil || v.ReportIndex >= len(report.Recipients) {
			return out, errors.New("report index unavailable")
		}
	} else {
		out.Reasons = append(out.Reasons, "UNSUPPORTED_REPORT")
	}
	var count int
	if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT id FROM messages WHERE message_id=$1 LIMIT 2) m`, v.RFCMessageID).Scan(&count); e != nil {
		return out, e
	}
	out.MultipleMessages = count > 1
	if count == 0 {
		out.Reasons = append(out.Reasons, "NO_MESSAGE_MATCH")
	}
	if out.MultipleMessages {
		out.Reasons = append(out.Reasons, "AMBIGUOUS_MESSAGE_ID")
	}
	// Include all matching recipient attempts, not merely an arbitrary newest row.
	// The extra row is solely a truncation signal, never a candidate selection.
	rows, e := tx.QueryContext(ctx, `SELECT m.id,a.provider_id,a.id,r.id
 FROM messages m JOIN delivery_attempts a ON a.message_id=m.id
 JOIN attempt_recipients ar ON ar.attempt_id=a.id
 JOIN recipients r ON r.id=ar.recipient_id AND r.message_id=m.id
 WHERE m.message_id=$1 ORDER BY m.id,a.attempt_number DESC,r.id LIMIT $2`, v.RFCMessageID, v.Limit+1)
	if e != nil {
		return out, e
	}
	requests := []InspectRequest{}
	for rows.Next() {
		r := InspectRequest{FeedbackID: v.FeedbackID, ExpectedSHA256: v.ExpectedSHA256, ReportIndex: v.ReportIndex}
		if e = rows.Scan(&r.MessageID, &r.ProviderID, &r.AttemptID, &r.RecipientID); e != nil {
			rows.Close()
			return out, e
		}
		requests = append(requests, r)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	if len(requests) > v.Limit {
		out.Truncated = true
		requests = requests[:v.Limit]
		out.Reasons = append(out.Reasons, "CANDIDATES_TRUNCATED")
	}
	if count > 0 && len(requests) == 0 {
		out.Reasons = append(out.Reasons, "NO_ATTEMPT_CANDIDATES")
	}
	for _, r := range requests {
		candidate, e := inspectSnapshot(ctx, tx, r)
		if e != nil {
			return out, e
		}
		candidate.Assessment.Reasons = append(candidate.Assessment.Reasons, "OPERATOR_MESSAGE_ID_HINT")
		if out.MultipleMessages {
			candidate.Assessment.CandidateMatches = false
			candidate.Assessment.Reasons = append(candidate.Assessment.Reasons, "AMBIGUOUS_MESSAGE_ID")
		}
		if out.Truncated {
			candidate.Assessment.CandidateMatches = false
			candidate.Assessment.Reasons = append(candidate.Assessment.Reasons, "CANDIDATES_TRUNCATED")
		}
		out.Candidates = append(out.Candidates, candidate)
	}
	if e = tx.Commit(); e != nil {
		return out, e
	}
	return out, nil
}
