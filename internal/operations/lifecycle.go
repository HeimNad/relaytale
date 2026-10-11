package operations

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

type LifecycleOptions struct {
	EMLDays int
	Limit   int
	After   string
	AsOf    time.Time
}

type LifecycleItem struct {
	MessageID      string   `json:"message_id"`
	Status         string   `json:"status"`
	ArchiveState   string   `json:"archive_state"`
	EMLDisposition string   `json:"eml_disposition"`
	Reasons        []string `json:"retain_reasons"`
}

type LifecycleReport struct {
	ReadOnly            bool            `json:"read_only"`
	DeletionAuthorized  bool            `json:"deletion_authorized"`
	AsOf                time.Time       `json:"as_of"`
	EMLDays             int             `json:"eml_days"`
	Items               []LifecycleItem `json:"items"`
	HasMore             bool            `json:"has_more"`
	NextAfter           string          `json:"next_after,omitempty"`
	UnreviewedFeedback  bool            `json:"unreviewed_feedback"`
	MetadataDisposition string          `json:"metadata_disposition"`
}

// LifecyclePreflight returns a single read-only snapshot. A candidate is an
// operator review hint, never a reusable authorization or a physical file check.
func LifecyclePreflight(ctx context.Context, db *sql.DB, p LifecycleOptions) (LifecycleReport, error) {
	empty := LifecycleReport{}
	if p.EMLDays < 0 || p.EMLDays > 36500 || p.Limit < 1 || p.Limit > 1000 {
		return empty, errors.New("invalid lifecycle bounds")
	}
	if p.After == "" {
		p.After = uuid.Nil.String()
	}
	if _, err := uuid.Parse(p.After); err != nil {
		return empty, errors.New("invalid lifecycle cursor")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	var now time.Time
	if err = tx.QueryRowContext(ctx, "SELECT transaction_timestamp()").Scan(&now); err != nil {
		return empty, err
	}
	if p.AsOf.IsZero() {
		p.AsOf = now
	}
	p.AsOf = p.AsOf.UTC()
	if p.AsOf.After(now) || p.AsOf.Year() < 1 || p.AsOf.Year() > 9999 {
		return empty, errors.New("invalid lifecycle as-of")
	}
	report := LifecycleReport{ReadOnly: true, AsOf: p.AsOf, EMLDays: p.EMLDays, Items: []LifecycleItem{}, MetadataDisposition: "RETAIN_ALL"}
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM feedback_reports f WHERE NOT EXISTS(SELECT 1 FROM feedback_reviews r WHERE r.feedback_id=f.id))`).Scan(&report.UnreviewedFeedback); err != nil {
		return empty, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.id,m.status,m.archive_state,m.completed_at,
 m.locked_by IS NOT NULL,
 EXISTS(SELECT 1 FROM delivery_attempts a WHERE a.message_id=m.id AND a.result='IN_PROGRESS'),
 EXISTS(SELECT 1 FROM recipients r WHERE r.message_id=m.id AND (r.status IN ('QUEUED','SENDING','TEMP_FAILED','DELIVERY_UNKNOWN') OR (r.status='EXPIRED' AND r.decision->>'may_have_delivered'='true'))),
 EXISTS(SELECT 1 FROM api_submission_ledger l WHERE l.message_id=m.id) OR m.idempotency_key IS NOT NULL,
 EXISTS(SELECT 1 FROM feedback_links l WHERE l.message_id=m.id),
 EXISTS(SELECT 1 FROM webhook_outbox w WHERE w.message_id=m.id AND w.state IN ('PENDING','IN_FLIGHT','DEAD'))
 FROM messages m WHERE m.id>$1 ORDER BY m.id LIMIT $2`, p.After, p.Limit+1)
	if err != nil {
		return empty, err
	}
	defer rows.Close()
	for rows.Next() {
		if len(report.Items) == p.Limit {
			report.HasMore = true
			break
		}
		item := LifecycleItem{Reasons: []string{}, EMLDisposition: "RETAIN"}
		var completed sql.NullTime
		var locked, attempt, recipients, idempotency, publication, webhook bool
		if err = rows.Scan(&item.MessageID, &item.Status, &item.ArchiveState, &completed, &locked, &attempt, &recipients, &idempotency, &publication, &webhook); err != nil {
			return empty, err
		}
		hold := func(yes bool, reason string) {
			if yes {
				item.Reasons = append(item.Reasons, reason)
			}
		}
		hold(p.EMLDays == 0, "RETENTION_DISABLED")
		terminalStatus := false
		switch item.Status {
		case "SMTP_ACCEPTED", "PERM_FAILED", "DELIVERED", "BOUNCED", "SUPPRESSED", "CANCELLED", "EXPIRED", "SANDBOX_STORED":
			terminalStatus = true
		}
		hold(!terminalStatus, "NON_TERMINAL_MESSAGE")
		hold(locked, "MESSAGE_LEASE")
		hold(attempt, "ATTEMPT_IN_PROGRESS")
		hold(recipients, "RECIPIENT_UNRESOLVED")
		hold(!completed.Valid, "COMPLETION_UNRECORDED")
		hold(completed.Valid && !completed.Time.Before(p.AsOf.AddDate(0, 0, -p.EMLDays)), "RETENTION_WINDOW")
		hold(idempotency, "IDEMPOTENCY_REFERENCE")
		hold(webhook, "WEBHOOK_REQUIRES_REVIEW")
		hold(publication, "MANUAL_FEEDBACK_HISTORY")
		// Quarantined reports have no authoritative Gateway UUID association yet.
		// Until scoped holds exist, unreviewed feedback conservatively holds all EML.
		hold(report.UnreviewedFeedback, "UNSCOPED_FEEDBACK_REVIEW")
		hold(item.ArchiveState != "AVAILABLE", "ARCHIVE_NOT_AVAILABLE")
		if len(item.Reasons) == 0 {
			item.EMLDisposition = "REVIEW_CANDIDATE"
		}
		report.Items = append(report.Items, item)
		report.NextAfter = item.MessageID
	}
	if err = rows.Err(); err != nil {
		return empty, err
	}
	if err = rows.Close(); err != nil {
		return empty, err
	}
	if err = tx.Commit(); err != nil {
		return empty, err
	}
	return report, nil
}
