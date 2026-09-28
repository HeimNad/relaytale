package webhook

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/google/uuid"
)

// Replay only requeues the identical notification. It never touches mail state.
func Replay(ctx context.Context, db *sql.DB, sequence int64, generation int, actor, reason string) error {
	if sequence < 1 || generation < 0 || strings.TrimSpace(actor) == "" || len(actor) > 128 || strings.TrimSpace(reason) == "" || len(reason) > 1000 {
		return errors.New("invalid webhook replay arguments")
	}
	tx, e := begin(ctx, db)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	result, e := tx.ExecContext(ctx, `UPDATE webhook_outbox SET state='PENDING',attempts=0,generation=generation+1,next_at=clock_timestamp(),deadline=clock_timestamp()+interval '24 hours' WHERE sequence=$1 AND generation=$2 AND state='DEAD'`, sequence, generation)
	if e != nil {
		return e
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return errors.New("webhook is not dead or replay generation changed")
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO webhook_delivery_log(sequence,generation,attempt,outcome,actor,reason) VALUES($1,$2,0,'REPLAY_REQUESTED',$3,$4)`, sequence, generation+1, actor, reason); e != nil {
		return e
	}
	return tx.Commit()
}
func Pause(ctx context.Context, db *sql.DB, id, actor, reason string) error {
	if !canonicalID(id) || strings.TrimSpace(actor) == "" || len(actor) > 128 || strings.TrimSpace(reason) == "" || len(reason) > 1000 {
		return errors.New("invalid webhook pause arguments")
	}
	tx, e := begin(ctx, db)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	result, e := tx.ExecContext(ctx, `UPDATE webhook_endpoints SET enabled=false WHERE id=$1`, id)
	if e != nil {
		return e
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return errors.New("webhook endpoint not found")
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO maintenance_audit(id,action,actor,data) VALUES($1,'WEBHOOK_PAUSED',$2,jsonb_build_object('endpoint_id',$3::text,'reason',$4::text))`, uuid.NewString(), actor, id, reason); e != nil {
		return e
	}
	return tx.Commit()
}
