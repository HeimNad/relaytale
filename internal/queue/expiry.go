package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"relaytale/internal/delivery"
)

// applyExpiry runs with the message locked. It changes only recipients eligible
// for future attempts, retaining accepted, uncertain and permanent SMTP facts.
// It never expires an active SENDING recipient, even before DATA authorization.
func applyExpiry(ctx context.Context, tx *sql.Tx, j Job) error {
	d := delivery.Decision{Version: 2, Action: delivery.Expire, Reason: "MESSAGE_DEADLINE_ELAPSED", Scope: "policy", Terminal: true}
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `UPDATE recipients r SET status='EXPIRED',retry_at=NULL,retry_automatic=false,completed_at=clock_timestamp(),decision=$2::jsonb||jsonb_build_object('may_have_delivered',coalesce(r.decision->>'may_have_delivered'='true',false) OR EXISTS(SELECT 1 FROM attempt_recipients ar WHERE ar.recipient_id=r.id AND ar.status='DELIVERY_UNKNOWN')) FROM messages m WHERE m.id=$1 AND r.message_id=m.id AND m.expires_at<=clock_timestamp() AND r.status IN ('QUEUED','TEMP_FAILED') RETURNING r.id,r.decision`, j.ID, string(raw))
	if err != nil {
		return err
	}
	type change struct {
		id       string
		decision json.RawMessage
	}
	var changes []change
	for rows.Next() {
		var c change
		if err = rows.Scan(&c.id, &c.decision); err != nil {
			rows.Close()
			return err
		}
		changes = append(changes, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range changes {
		if err = event(ctx, tx, j, "RECIPIENT_EXPIRED", c.id, time.Now().UTC(), c.decision); err != nil {
			return err
		}
	}
	return nil
}

// SweepExpired has an independent commit, including when no provider is usable.
// Expiration is bounded and idempotent; it does not refund admission budgets.
func (r Repository) SweepExpired(ctx context.Context) error {
	tx, err := r.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT m.id FROM messages m WHERE m.locked_by IS NULL AND m.status IN ('QUEUED','TEMP_FAILED','PARTIAL_ACCEPTED','DELIVERY_UNKNOWN') AND m.expires_at<=clock_timestamp() AND EXISTS(SELECT 1 FROM recipients r WHERE r.message_id=m.id AND r.status IN ('QUEUED','TEMP_FAILED')) ORDER BY m.expires_at,m.id LIMIT 20 FOR UPDATE OF m SKIP LOCKED`)
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
	for _, id := range ids {
		if err = applyExpiry(ctx, tx, Job{ID: id}); err != nil {
			return err
		}
		if err = refresh(ctx, tx, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
