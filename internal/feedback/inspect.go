package feedback

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

type InspectRequest struct {
	FeedbackID     string `json:"feedback_id"`
	ExpectedSHA256 string `json:"expected_sha256"`
	MessageID      string `json:"message_id"`
	ProviderID     string `json:"provider_id"`
	AttemptID      string `json:"attempt_id"`
	RecipientID    string `json:"recipient_id"`
	ReportIndex    int    `json:"report_index"`
}
type Inspection struct {
	Request       InspectRequest `json:"request"`
	SnapshotAt    time.Time      `json:"snapshot_at"`
	FeedbackState string         `json:"feedback_state"`
	CurrentStatus string         `json:"current_status,omitempty"`
	AttemptStatus string         `json:"attempt_status,omitempty"`
	Selected      *Recipient     `json:"selected_report,omitempty"`
	Assessment    Assessment     `json:"assessment"`
}

// Inspect uses one read-only repeatable snapshot, with no locks on workers and
// no state writes. Output is advisory and must never be used as a write ticket.
func (s Service) Inspect(ctx context.Context, v InspectRequest) (Inspection, error) {
	for _, id := range []string{v.FeedbackID, v.MessageID, v.ProviderID, v.AttemptID, v.RecipientID} {
		if _, err := uuid.Parse(id); err != nil {
			return Inspection{}, errors.New("feedback, message, provider, attempt and recipient UUIDs required")
		}
	}
	if len(v.ExpectedSHA256) != 64 || v.ReportIndex < 0 || v.ReportIndex >= MaxRecipients {
		return Inspection{}, errors.New("expected SHA-256 and report index 0..99 required")
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return Inspection{}, err
	}
	defer tx.Rollback()
	out, err := inspectSnapshot(ctx, tx, v)
	if err != nil {
		return Inspection{}, err
	}
	if err = tx.Commit(); err != nil {
		return Inspection{}, err
	}
	return out, nil
}

// inspectSnapshot is shared by explicit inspection and candidate discovery so
// every candidate uses the same rules and database snapshot.
func inspectSnapshot(ctx context.Context, tx *sql.Tx, v InspectRequest) (Inspection, error) {
	var err error
	var out Inspection
	out.Request = v
	if err = tx.QueryRowContext(ctx, `SELECT transaction_timestamp()`).Scan(&out.SnapshotAt); err != nil {
		return Inspection{}, err
	}
	f, err := scan(tx.QueryRowContext(ctx, `SELECT `+columns+`,f.parsed FROM feedback_reports f WHERE f.id=$1`, v.FeedbackID), true)
	if err != nil {
		return Inspection{}, err
	}
	out.FeedbackState = f.State
	e := AssociationEvidence{RecordState: f.State, Parsed: f.ParseState == "PARSED", ExpectedDigestMatches: f.SHA256 == v.ExpectedSHA256}
	if e.Parsed {
		var report Report
		if err = json.Unmarshal(f.Report, &report); err != nil {
			return Inspection{}, errors.New("stored report cannot be decoded")
		}
		if v.ReportIndex >= len(report.Recipients) {
			return Inspection{}, errors.New("report index out of range")
		}
		e.Selected = report.Recipients[v.ReportIndex]
		out.Selected = &e.Selected
		address, valid := reportMailbox(e.Selected.FinalRecipient)
		if valid {
			for _, r := range report.Recipients {
				other, ok := reportMailbox(r.FinalRecipient)
				if ok && sameMailbox(address, other) {
					e.SameAddressRows++
				}
			}
		}
	}
	// Check the membership row as well as both message IDs: independent foreign
	// keys alone do not prove that a recipient belongs to an attempt's message.
	err = tx.QueryRowContext(ctx, `SELECT r.address,r.status,ar.status,
 a.final_response_at IS NOT NULL AND a.finished_at IS NOT NULL AND coalesce(a.smtp_code BETWEEN 200 AND 299,false),
 NOT EXISTS(SELECT 1 FROM attempt_recipients newer JOIN delivery_attempts na ON na.id=newer.attempt_id WHERE newer.recipient_id=r.id AND na.attempt_number>a.attempt_number),
 $5::timestamptz<a.started_at
 FROM delivery_attempts a JOIN attempt_recipients ar ON ar.attempt_id=a.id JOIN recipients r ON r.id=ar.recipient_id
 WHERE a.id=$1 AND a.message_id=$2 AND r.message_id=$2 AND a.provider_id=$3 AND r.id=$4`, v.AttemptID, v.MessageID, v.ProviderID, v.RecipientID, f.CreatedAt).Scan(&e.Address, &e.CurrentStatus, &e.AttemptStatus, &e.FinalAccepted, &e.LatestAttempt, &e.PredatesAttempt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Inspection{}, err
	}
	e.RelationMatches = err == nil
	out.CurrentStatus = e.CurrentStatus
	out.AttemptStatus = e.AttemptStatus
	out.Assessment = Assess(e)
	return out, nil
}
