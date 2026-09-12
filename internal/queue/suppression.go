package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"relaytale/internal/delivery"
	"relaytale/internal/smtpclient"
	"relaytale/internal/suppression"
)

// SweepSuppressed makes pending suppression visible even when no provider can
// be claimed. Each bounded batch commits independently of Claim's no-job path.
func (r Repository) SweepSuppressed(ctx context.Context) error {
	tx, err := r.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT m.id FROM messages m WHERE m.locked_by IS NULL AND EXISTS(SELECT 1 FROM recipients rc WHERE rc.message_id=m.id AND rc.status IN ('QUEUED','TEMP_FAILED') AND `+suppression.ActiveSQL("rc.address")+`) ORDER BY m.created_at,m.id LIMIT 20 FOR UPDATE OF m SKIP LOCKED`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	if err = suppression.Lock(ctx, tx, false); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err = suppression.Apply(ctx, tx, id, "", false); err != nil {
			return err
		}
		if err = refresh(ctx, tx, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// finishSuppression retains SMTP evidence in attempt_recipients. The current
// recipient projection can instead be SUPPRESSED, or resume an unsent envelope.
// Only a durable blocked fence can authorize this continuation, never a generic
// callback/database failure. Existing RCPT rejections keep their normal policy.
func (r Repository) finishSuppression(ctx context.Context, tx *sql.Tx, j Job, out smtpclient.Result, rc smtpclient.RecipientResult) (bool, error) {
	if rc.Status == smtpclient.Accepted || rc.Status == smtpclient.Unknown {
		return false, errors.New("acceptance contradicts suppression fence")
	}
	var status string
	var attempts int
	var started time.Time
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT status,attempt_count,retry_started_at,decision FROM recipients WHERE id=$1`, rc.ID).Scan(&status, &attempts, &started, &raw); err != nil {
		return false, err
	}
	if status != "SUPPRESSED" {
		if rc.Stage != "SUPPRESSION_BLOCKED" && rc.Stage != "DATABASE_ERROR" {
			return false, nil
		}
		d := delivery.Decision{Version: 2, Action: delivery.Resume, Reason: "ENVELOPE_ABORTED_BEFORE_DATA", Scope: "policy"}
		status = "QUEUED"
		if attempts >= delivery.MaxAttempts || !out.FinishedAt.Before(started.Add(delivery.Window)) {
			status = "TEMP_FAILED"
			d.Action = delivery.Manual
			d.Reason = "RETRY_BUDGET_EXHAUSTED"
		}
		var err error
		raw, err = json.Marshal(d)
		if err != nil {
			return false, err
		}
		// retry_automatic retains the original claim's opt-in requirement.
		if _, err = tx.ExecContext(ctx, `UPDATE recipients SET status=$2,decision=$3,retry_at=NULL,completed_at=NULL WHERE id=$1`, rc.ID, status, string(raw)); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE attempt_recipients SET decision=$3,protocol_stage=$4 WHERE attempt_id=$1 AND recipient_id=$2`, j.AttemptID, rc.ID, string(raw), rc.Stage); err != nil {
		return false, err
	}
	return true, event(ctx, tx, j, "DELIVERY_DECIDED", rc.ID, out.FinishedAt, map[string]any{"decision": json.RawMessage(raw), "suppression_fence": true, "smtp_status": rc.Status, "smtp_code": rc.Code, "protocol_stage": rc.Stage})
}
