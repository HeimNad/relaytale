package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"relaytale/internal/delivery"
	"relaytale/internal/smtpclient"
	"relaytale/internal/suppression"
)

func (r Repository) claimRecipient(alias string) string {
	return fmt.Sprintf(`%s.status='QUEUED' AND (NOT %s.retry_automatic OR (%t AND %s.attempt_count<7 AND %s.retry_started_at>clock_timestamp()-interval '24 hours'))`, alias, alias, r.RetryEnabled, alias, alias) + ` AND NOT ` + suppression.ActiveSQL(alias+".address")
}
func (r Repository) decideRecipient(ctx context.Context, tx *sql.Tx, j Job, out smtpclient.Result, rc smtpclient.RecipientResult) error {
	var count int
	var started time.Time
	if err := tx.QueryRowContext(ctx, `SELECT attempt_count,retry_started_at FROM recipients WHERE id=$1`, rc.ID).Scan(&count, &started); err != nil {
		return err
	}
	evidence := delivery.Evidence{Stage: rc.Stage, ErrorClass: out.ErrorClass, Status: rc.Status, Code: rc.Code, FinalCode: out.Code, FinalResponse: !out.FinalResponseAt.IsZero(), BodyStarted: !out.DataStartedAt.IsZero()}
	budget := delivery.Budget{Attempts: count, StartedAt: started, Now: out.FinishedAt, Seed: j.AttemptID + rc.ID}
	d := delivery.Decide(evidence, budget)
	var retryAt any
	if d.Action == delivery.Retry && r.RetryEnabled {
		retryAt = out.FinishedAt.Add(d.RetryAfter)
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recipients SET decision=$2,retry_at=$3,retry_automatic=false,completed_at=CASE WHEN $4 IN ('MANUAL_INTERVENTION','DELIVERY_UNKNOWN') THEN NULL ELSE completed_at END,status=CASE WHEN $4='MANUAL_INTERVENTION' THEN 'TEMP_FAILED' WHEN $4='DELIVERY_UNKNOWN' THEN 'DELIVERY_UNKNOWN' ELSE status END WHERE id=$1`, rc.ID, string(raw), retryAt, d.Action); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE attempt_recipients SET decision=$3,protocol_stage=$4 WHERE attempt_id=$1 AND recipient_id=$2`, j.AttemptID, rc.ID, string(raw), rc.Stage); err != nil {
		return err
	}
	return event(ctx, tx, j, "DELIVERY_DECIDED", rc.ID, out.FinishedAt, map[string]any{"budget": budget, "evidence": evidence, "decision": d, "retry_at": retryAt, "automatic_retry_enabled": r.RetryEnabled})
}

// refresh derives the message projection from ALL recipients, never only the latest attempt.
func refresh(ctx context.Context, tx *sql.Tx, id string) error {
	_, err := tx.ExecContext(ctx, `WITH counts AS (
 SELECT count(*) n,count(*) FILTER(WHERE status='DELIVERY_UNKNOWN') unknown,
 count(*) FILTER(WHERE status='QUEUED') queued,count(*) FILTER(WHERE status='SENDING') sending,
 count(*) FILTER(WHERE status IN ('SMTP_ACCEPTED','DELIVERED')) accepted,
 count(*) FILTER(WHERE status='SUPPRESSED') suppressed,
 count(*) FILTER(WHERE status='DELIVERED') delivered,count(*) FILTER(WHERE status='TEMP_FAILED') temporary
 FROM recipients WHERE message_id=$1), projection AS (
 SELECT CASE WHEN unknown>0 THEN 'DELIVERY_UNKNOWN' WHEN sending>0 THEN 'SENDING' WHEN queued>0 THEN 'QUEUED'
 WHEN suppressed=n AND n>0 THEN 'SUPPRESSED' WHEN delivered=n AND n>0 THEN 'DELIVERED' WHEN accepted=n AND n>0 THEN 'SMTP_ACCEPTED'
 WHEN accepted>0 THEN 'PARTIAL_ACCEPTED' WHEN temporary>0 THEN 'TEMP_FAILED' ELSE 'PERM_FAILED' END status FROM counts)
 UPDATE messages m SET status=p.status,next_attempt_at=CASE WHEN p.status='QUEUED' THEN now() ELSE (SELECT min(retry_at) FROM recipients WHERE message_id=$1) END,
 completed_at=CASE WHEN p.status IN ('SMTP_ACCEPTED','PERM_FAILED','DELIVERED','SUPPRESSED') THEN coalesce(m.completed_at,now()) ELSE NULL END
 FROM projection p WHERE m.id=$1`, id)
	return err
}

// Schedule advances durable deadlines under the same message locks used by Claim/Finish.
// No timers hold work in process memory. Disabling retry freezes pending automatic jobs.
func (r Repository) Schedule(ctx context.Context) error {
	tx, err := r.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT m.id FROM messages m WHERE m.locked_by IS NULL AND m.archive_state='AVAILABLE'
 AND m.status IN ('QUEUED','TEMP_FAILED','PARTIAL_ACCEPTED')
 AND EXISTS(SELECT 1 FROM recipients r WHERE r.message_id=m.id AND (r.retry_at<=now() OR (r.retry_automatic AND r.status='QUEUED' AND (r.attempt_count>=7 OR r.retry_started_at<=now()-interval '24 hours'))))
 ORDER BY m.created_at,m.id LIMIT 20 FOR UPDATE OF m SKIP LOCKED`)
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
		rcRows, err := tx.QueryContext(ctx, `SELECT id,attempt_count,retry_started_at FROM recipients WHERE message_id=$1 AND (retry_at<=now() OR (retry_automatic AND status='QUEUED' AND (attempt_count>=7 OR retry_started_at<=now()-interval '24 hours'))) ORDER BY id`, id)
		if err != nil {
			return err
		}
		type candidate struct {
			id      string
			count   int
			started time.Time
		}
		var candidates []candidate
		for rcRows.Next() {
			var c candidate
			if err = rcRows.Scan(&c.id, &c.count, &c.started); err != nil {
				rcRows.Close()
				return err
			}
			candidates = append(candidates, c)
		}
		err = rcRows.Err()
		rcRows.Close()
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		for _, c := range candidates {
			if c.count >= delivery.MaxAttempts || !now.Before(c.started.Add(delivery.Window)) {
				d := delivery.Decision{Version: 1, Action: delivery.Manual, Reason: "RETRY_BUDGET_EXHAUSTED", Scope: "policy"}
				raw, _ := json.Marshal(d)
				if _, err = tx.ExecContext(ctx, `UPDATE recipients SET status='TEMP_FAILED',retry_at=NULL,retry_automatic=false,decision=$2 WHERE id=$1`, c.id, string(raw)); err != nil {
					return err
				}
				if err = event(ctx, tx, Job{ID: id}, "RETRY_EXHAUSTED", c.id, now, d); err != nil {
					return err
				}
			} else if r.RetryEnabled {
				if _, err = tx.ExecContext(ctx, `UPDATE recipients SET status='QUEUED',retry_at=NULL,retry_automatic=true WHERE id=$1`, c.id); err != nil {
					return err
				}
				if err = event(ctx, tx, Job{ID: id}, "RETRY_QUEUED", c.id, now, map[string]any{"attempts": c.count, "route_evaluated_at_claim": true, "failover_enabled": r.FailoverEnabled}); err != nil {
					return err
				}
			}
		}
		if err = refresh(ctx, tx, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func expireRecovered(ctx context.Context, tx *sql.Tx, j Job) error {
	d := delivery.Decision{Version: 1, Action: delivery.Manual, Reason: "RECOVERY_BUDGET_EXHAUSTED", Scope: "policy"}
	raw, _ := json.Marshal(d)
	rows, err := tx.QueryContext(ctx, `UPDATE recipients SET status='TEMP_FAILED',retry_at=NULL,retry_automatic=false,decision=$2 WHERE message_id=$1 AND status='QUEUED' AND (attempt_count>=7 OR retry_started_at<=now()-interval '24 hours') RETURNING id`, j.ID, string(raw))
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
		if err = event(ctx, tx, j, "RETRY_EXHAUSTED", id, time.Now().UTC(), d); err != nil {
			return err
		}
	}
	return refresh(ctx, tx, j.ID)
}
