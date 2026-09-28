package webhook

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
)

type Worker struct {
	DB        *sql.DB
	Endpoints []Endpoint
	Sender    Sender
	Log       *slog.Logger
}
type claim struct {
	Sequence            int64
	Payload             string
	Token               string
	Attempt, Generation int
}

func (w Worker) claim(ctx context.Context, e Endpoint) (claim, error) {
	var c claim
	tx, err := begin(ctx, w.DB)
	if err != nil {
		return c, err
	}
	defer tx.Rollback()
	var eligible bool
	err = tx.QueryRowContext(ctx, `SELECT e.enabled AND k.enabled AND e.config_hash=$2 FROM webhook_endpoints e JOIN api_keys k ON k.id=e.api_key_id WHERE e.id=$1`, e.ID, e.Hash()).Scan(&eligible)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !eligible {
		return c, tx.Commit()
	}
	if err != nil {
		return c, err
	}
	// Bounded retirement of expired rows, including a final crashed claim.
	_, err = tx.ExecContext(ctx, `WITH candidates AS (
 SELECT sequence FROM webhook_outbox WHERE endpoint_id=$1 AND (state='PENDING' OR (state='IN_FLIGHT' AND lease_until<=clock_timestamp())) AND (deadline<=clock_timestamp() OR attempts>=8) ORDER BY sequence FOR UPDATE SKIP LOCKED LIMIT 100
 ), expired AS (
 UPDATE webhook_outbox o SET state='DEAD',lease_token=NULL,lease_until=NULL FROM candidates c WHERE o.sequence=c.sequence RETURNING o.sequence,o.generation,o.attempts)
 INSERT INTO webhook_delivery_log(sequence,generation,attempt,outcome) SELECT sequence,generation,attempts,'EXHAUSTED' FROM expired`, e.ID)
	if err != nil {
		return c, err
	}
	c.Token = uuid.NewString()
	err = tx.QueryRowContext(ctx, `WITH candidate AS (
 SELECT sequence FROM webhook_outbox WHERE endpoint_id=$1 AND attempts<8 AND deadline>clock_timestamp()
 AND ((state='PENDING' AND next_at<=clock_timestamp()) OR (state='IN_FLIGHT' AND lease_until<=clock_timestamp()))
 ORDER BY next_at,sequence FOR UPDATE SKIP LOCKED LIMIT 1)
 UPDATE webhook_outbox o SET state='IN_FLIGHT',attempts=attempts+1,lease_token=$2,lease_until=clock_timestamp()+interval '60 seconds'
 FROM candidate c WHERE o.sequence=c.sequence RETURNING o.sequence,o.payload,o.attempts,o.generation`, e.ID, c.Token).Scan(&c.Sequence, &c.Payload, &c.Attempt, &c.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		return claim{}, tx.Commit()
	}
	if err != nil {
		return c, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO webhook_delivery_log(sequence,generation,attempt,outcome) VALUES($1,$2,$3,'STARTED')`, c.Sequence, c.Generation, c.Attempt); err != nil {
		return c, err
	}
	return c, tx.Commit()
}
func (w Worker) finish(ctx context.Context, c claim, code int, sendErr error) error {
	state, outcome := "PENDING", "RETRY"
	if sendErr == nil && code >= 200 && code < 300 {
		state, outcome = "DELIVERED", "ACKNOWLEDGED"
	} else if c.Attempt >= 8 {
		state, outcome = "DEAD", "EXHAUSTED"
	}
	delay := time.Minute*time.Duration(1<<min(c.Attempt-1, 6)) + time.Duration(rand.IntN(16))*time.Second
	tx, err := begin(ctx, w.DB)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE webhook_outbox SET state=$3,lease_token=NULL,lease_until=NULL,next_at=clock_timestamp()+($4*interval '1 second') WHERE sequence=$1 AND lease_token=$2 AND state='IN_FLIGHT' AND lease_until>clock_timestamp()`, c.Sequence, c.Token, state, int(delay.Seconds()))
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return errors.New("webhook claim no longer owned")
	}
	if code < 100 || code > 599 {
		code = 0
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO webhook_delivery_log(sequence,generation,attempt,outcome,http_code) VALUES($1,$2,$3,$4,NULLIF($5,0))`, c.Sequence, c.Generation, c.Attempt, outcome, code)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// RunOne performs at most one notification for this endpoint. A crash after the
// HTTP side effect but before finish reuses the immutable event on lease recovery.
func (w Worker) RunOne(ctx context.Context, e Endpoint) (bool, error) {
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, err := w.claim(bounded, e)
	if err != nil || c.Sequence == 0 {
		return false, err
	}
	// Recheck configuration immediately before the network side effect. Revocation
	// after this point cannot retract an already-started request.
	var enabled bool
	err = w.DB.QueryRowContext(bounded, `SELECT e.enabled AND k.enabled AND e.config_hash=$2 FROM webhook_endpoints e JOIN api_keys k ON k.id=e.api_key_id WHERE e.id=$1`, e.ID, e.Hash()).Scan(&enabled)
	var code int
	if err == nil && enabled {
		sender := w.Sender
		if sender == nil {
			sender = HTTPSender{}
		}
		code, err = sender.Send(bounded, e, []byte(c.Payload))
	} else {
		err = errors.New("webhook configuration unavailable")
	}
	finishCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	return true, w.finish(finishCtx, c, code, err)
}
func (w Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		for _, e := range w.Endpoints {
			if ctx.Err() != nil {
				return
			}
			if _, err := w.RunOne(ctx, e); err != nil && w.Log != nil {
				w.Log.Warn("webhook dispatch unavailable")
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
