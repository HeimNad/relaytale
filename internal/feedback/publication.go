package feedback

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
)

var ErrPublication = errors.New("feedback publication conflicts with evidence or review state")
var ErrInvalidPublication = errors.New("invalid feedback publication request")

type LinkRequest struct {
	InspectRequest
	Reason                string `json:"reason"`
	AcknowledgeUnverified bool   `json:"acknowledge_unverified"`
	// Unsupported reports require a deliberate human association, never a parser hint.
	ManualAssociation bool `json:"manual_association"`
}
type ReviewRequest struct {
	ExpectedRevision      int    `json:"expected_revision"`
	Action                string `json:"action"`
	Reason                string `json:"reason"`
	AcknowledgeUnverified bool   `json:"acknowledge_unverified"`
}
type Publication struct {
	ID          string     `json:"id"`
	RecipientID string     `json:"recipient_id"`
	AttemptID   string     `json:"attempt_id"`
	State       string     `json:"state"`
	SourceTrust string     `json:"source_trust"`
	Revision    int        `json:"revision"`
	CreatedAt   time.Time  `json:"created_at"`
	ReviewedAt  *time.Time `json:"reviewed_at,omitempty"`
}

// Projection contains no provider, source mailbox, MIME, diagnostic or actor data.
// Invalidating evidence outranks every operator conclusion.
const Projection = `SELECT l.id::text,l.recipient_id::text,l.attempt_id::text,
 CASE WHEN EXISTS(SELECT 1 FROM feedback_reports other WHERE other.source=f.source AND other.external_id=f.external_id AND other.sha256<>f.sha256) THEN 'EVIDENCE_CONFLICT'
 WHEN EXISTS(SELECT 1 FROM feedback_reviews dismissed WHERE dismissed.feedback_id=f.id) THEN 'WITHDRAWN'
 WHEN rev.action='WITHDRAW' THEN 'WITHDRAWN'
 WHEN rev.action='CONFIRM_FAILURE' THEN 'FAILURE_CONFIRMED_BY_OPERATOR' ELSE 'PENDING_REVIEW' END,
 f.trust,coalesce(rev.revision,0),l.created_at,rev.created_at
 FROM feedback_links l JOIN feedback_reports f ON f.id=l.feedback_id
 LEFT JOIN LATERAL(SELECT revision,action,created_at FROM feedback_link_reviews WHERE link_id=l.id ORDER BY revision DESC LIMIT 1) rev ON true `

func ScanPublication(row interface{ Scan(...any) error }) (Publication, error) {
	var out Publication
	err := row.Scan(&out.ID, &out.RecipientID, &out.AttemptID, &out.State, &out.SourceTrust, &out.Revision, &out.CreatedAt, &out.ReviewedAt)
	return out, err
}
func fence(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(78320421)`)
	return err
}

func (s Service) Link(ctx context.Context, v LinkRequest, actor string) (Publication, error) {
	if !v.AcknowledgeUnverified || !valid(v.Reason, 2048) || !valid(actor, 128) || v.ReportIndex < 0 || v.ReportIndex >= MaxRecipients {
		return Publication{}, ErrInvalidPublication
	}
	for _, id := range []string{v.FeedbackID, v.MessageID, v.AttemptID, v.ProviderID, v.RecipientID} {
		u, e := uuid.Parse(id)
		if e != nil || u == uuid.Nil || u.String() != id {
			return Publication{}, ErrInvalidPublication
		}
	}
	digest, e := hex.DecodeString(v.ExpectedSHA256)
	if e != nil || len(digest) != 32 || hex.EncodeToString(digest) != v.ExpectedSHA256 {
		return Publication{}, ErrInvalidPublication
	}
	tx, err := begin(ctx, s.DB)
	if err != nil {
		return Publication{}, err
	}
	defer tx.Rollback()
	if err = fence(ctx, tx); err != nil {
		return Publication{}, err
	}
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT raw FROM feedback_reports WHERE id=$1`, v.FeedbackID).Scan(&raw); err != nil {
		return Publication{}, err
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != v.ExpectedSHA256 {
		return Publication{}, ErrPublication
	}
	inspection, err := inspectSnapshot(ctx, tx, v.InspectRequest)
	if err != nil {
		return Publication{}, ErrPublication
	}
	if inspection.FeedbackState != "QUARANTINED" || inspection.CurrentStatus != "SMTP_ACCEPTED" || inspection.AttemptStatus != "SMTP_ACCEPTED" || slices.Contains(inspection.Assessment.Reasons, "NO_STABLE_SMTP_ACCEPTANCE") || slices.Contains(inspection.Assessment.Reasons, "NEWER_RECIPIENT_ATTEMPT") || slices.Contains(inspection.Assessment.Reasons, "REPORT_PREDATES_ATTEMPT") {
		return Publication{}, ErrPublication
	}
	unsupported := slices.Contains(inspection.Assessment.Reasons, "UNSUPPORTED_REPORT")
	if unsupported {
		if !v.ManualAssociation || v.ReportIndex != 0 || slices.Contains(inspection.Assessment.Reasons, "RELATION_MISMATCH") {
			return Publication{}, ErrPublication
		}
	} else if v.ManualAssociation || !inspection.Assessment.CandidateMatches {
		return Publication{}, ErrPublication
	}
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT id FROM feedback_links WHERE feedback_id=$1 AND report_index=$2`, v.FeedbackID, v.ReportIndex).Scan(&existing)
	if err == nil {
		return Publication{}, ErrPublication
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Publication{}, err
	}
	id := uuid.Must(uuid.NewV7()).String()
	if _, err = tx.ExecContext(ctx, `INSERT INTO feedback_links(id,feedback_id,report_index,message_id,attempt_id,recipient_id,actor,reason) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, id, v.FeedbackID, v.ReportIndex, v.MessageID, v.AttemptID, v.RecipientID, actor, v.Reason); err != nil {
		return Publication{}, err
	}
	if err = publicationEvent(ctx, tx, id, v.MessageID, v.AttemptID, v.RecipientID, "FEEDBACK_LINKED", actor, v.Reason); err != nil {
		return Publication{}, err
	}
	out, err := ScanPublication(tx.QueryRowContext(ctx, Projection+` WHERE l.id=$1`, id))
	if err != nil {
		return Publication{}, err
	}
	return out, tx.Commit()
}

func (s Service) Review(ctx context.Context, id string, v ReviewRequest, actor string) (Publication, error) {
	u, e := uuid.Parse(id)
	if e != nil || u == uuid.Nil || u.String() != id || !v.AcknowledgeUnverified || !valid(v.Reason, 2048) || !valid(actor, 128) || v.ExpectedRevision < 0 || v.ExpectedRevision > 1 || (v.Action != "CONFIRM_FAILURE" && v.Action != "WITHDRAW") {
		return Publication{}, ErrInvalidPublication
	}
	tx, err := begin(ctx, s.DB)
	if err != nil {
		return Publication{}, err
	}
	defer tx.Rollback()
	if err = fence(ctx, tx); err != nil {
		return Publication{}, err
	}
	out, err := ScanPublication(tx.QueryRowContext(ctx, Projection+` WHERE l.id=$1`, id))
	if err != nil {
		return Publication{}, err
	}
	if out.Revision != v.ExpectedRevision || out.State == "WITHDRAWN" || (v.Action == "CONFIRM_FAILURE" && out.State != "PENDING_REVIEW") || out.Revision == 1 && v.Action != "WITHDRAW" {
		return Publication{}, ErrPublication
	}
	// A withdrawal is always available, including after conflicting evidence.
	if v.Action == "CONFIRM_FAILURE" {
		var reportState string
		var reportedFailure bool
		if err = tx.QueryRowContext(ctx, `SELECT f.parse_state,coalesce(f.parsed->'recipients'->l.report_index->>'action'='failed' AND f.parsed->'recipients'->l.report_index->>'status' LIKE '5.%',false) FROM feedback_links l JOIN feedback_reports f ON f.id=l.feedback_id WHERE l.id=$1`, id).Scan(&reportState, &reportedFailure); err != nil {
			return Publication{}, err
		}
		if reportState == "PARSED" && !reportedFailure {
			return Publication{}, ErrPublication
		}
		var stable bool
		if err = tx.QueryRowContext(ctx, `SELECT r.status='SMTP_ACCEPTED' AND NOT EXISTS(SELECT 1 FROM attempt_recipients ar JOIN delivery_attempts a ON a.id=ar.attempt_id WHERE ar.recipient_id=r.id AND a.attempt_number>original.attempt_number) FROM recipients r JOIN delivery_attempts original ON original.id=$2 WHERE r.id=$1`, out.RecipientID, out.AttemptID).Scan(&stable); err != nil {
			return Publication{}, err
		}
		if !stable {
			return Publication{}, ErrPublication
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO feedback_link_reviews(link_id,revision,action,actor,reason) VALUES($1,$2,$3,$4,$5)`, id, out.Revision+1, v.Action, actor, v.Reason); err != nil {
		return Publication{}, err
	}
	var mid string
	if err = tx.QueryRowContext(ctx, `SELECT message_id FROM feedback_links WHERE id=$1`, id).Scan(&mid); err != nil {
		return Publication{}, err
	}
	if err = publicationEvent(ctx, tx, id, mid, out.AttemptID, out.RecipientID, "FEEDBACK_REVIEWED", actor, v.Reason); err != nil {
		return Publication{}, err
	}
	out, err = ScanPublication(tx.QueryRowContext(ctx, Projection+` WHERE l.id=$1`, id))
	if err != nil {
		return Publication{}, err
	}
	return out, tx.Commit()
}
func publicationEvent(ctx context.Context, tx *sql.Tx, id, mid, aid, rid, kind, actor, reason string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(id,message_id,attempt_id,recipient_id,event_type,source,data) VALUES($1,$2,$3,$4,$5,'operator',jsonb_build_object('feedback_link_id',$6::text,'source_trust','UNVERIFIED'))`, uuid.Must(uuid.NewV7()).String(), mid, aid, rid, kind, id); err != nil {
		return err
	}
	return audit(ctx, tx, kind, actor, map[string]string{"feedback_link_id": id, "message_id": mid, "reason": reason, "source_trust": "UNVERIFIED"})
}
