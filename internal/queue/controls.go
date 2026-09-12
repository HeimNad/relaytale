package queue

import (
	"context"
	"database/sql"
	"time"

	"relaytale/internal/delivery"
	"relaytale/internal/smtpclient"
)

// Quotas are rolling admission budgets measured in recipient attempts, including
// uncertain outcomes. Never refund a committed reservation after network I/O.
const quotaAvailable = `LEAST(
 coalesce(p.hourly_limit::bigint-(SELECT coalesce(sum(q.units),0) FROM provider_quota q WHERE q.provider_id=p.id AND q.reserved_at>clock_timestamp()-interval '1 hour'),2147483647),
 coalesce(p.daily_limit::bigint-(SELECT coalesce(sum(q.units),0) FROM provider_quota q WHERE q.provider_id=p.id AND q.reserved_at>clock_timestamp()-interval '24 hours'),2147483647))`

const circuitAvailable = `NOT p.health_check_enabled OR NOT EXISTS(SELECT 1 FROM provider_health h WHERE h.provider_id=p.id
 AND h.circuit_state<>'CLOSED' AND (h.circuit_state='OPEN' AND h.open_until<=clock_timestamp() AND h.probe_attempt_id IS NULL) IS NOT TRUE)`

func (r Repository) controlRequirements() string {
	result := ` AND (` + quotaAvailable + `)>0`
	if r.HealthEnabled {
		result += ` AND (` + circuitAvailable + `)`
	}
	return result
}

// Called with the provider row locked, using a fresh statement snapshot after
// acquiring that lock. Selection snapshots alone cannot enforce shared limits.
func (r Repository) admission(ctx context.Context, tx *sql.Tx, pid string) (int, error) {
	var remaining int
	err := tx.QueryRowContext(ctx, `SELECT `+quotaAvailable+` FROM providers p WHERE p.id=$1`+r.controlRequirements(), pid).Scan(&remaining)
	if err == sql.ErrNoRows {
		return 0, ErrNoJob
	}
	return remaining, err
}

func (r Repository) reserveControls(ctx context.Context, tx *sql.Tx, j Job) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO provider_quota(attempt_id,provider_id,units) VALUES($1,$2,$3)`, j.AttemptID, j.Provider.ID, len(j.Recipients)); err != nil {
		return err
	}
	if err := event(ctx, tx, j, "QUOTA_RESERVED", "", time.Now().UTC(), map[string]any{"provider_id": j.Provider.ID, "units": len(j.Recipients), "unit": "recipient_attempt"}); err != nil {
		return err
	}
	if !r.HealthEnabled {
		return nil
	}
	var changed string
	err := tx.QueryRowContext(ctx, `UPDATE provider_health h SET circuit_state='HALF_OPEN',probe_attempt_id=$2,status='DEGRADED',updated_at=clock_timestamp()
 FROM providers p WHERE h.provider_id=$1 AND p.id=h.provider_id AND p.health_check_enabled AND h.circuit_state='OPEN' AND h.open_until<=clock_timestamp() AND h.probe_attempt_id IS NULL RETURNING h.provider_id`, j.Provider.ID, j.AttemptID).Scan(&changed)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	return event(ctx, tx, j, "PROVIDER_CIRCUIT_HALF_OPEN", "", time.Now().UTC(), map[string]any{"provider_id": j.Provider.ID, "probe_attempt_id": j.AttemptID})
}

func (r Repository) finishHealth(ctx context.Context, tx *sql.Tx, j Job, out smtpclient.Result) error {
	sample := delivery.ProviderOutcome(delivery.Evidence{Stage: out.Stage, ErrorClass: out.ErrorClass, Status: out.Status, Code: out.Code, FinalCode: out.Code, FinalResponse: !out.FinalResponseAt.IsZero(), BodyStarted: !out.DataStartedAt.IsZero()})
	// All claim and finish paths take message -> provider locks in that order.
	var enabled bool
	if err := tx.QueryRowContext(ctx, `SELECT health_check_enabled FROM providers WHERE id=$1 FOR UPDATE`, j.Provider.ID).Scan(&enabled); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE delivery_attempts SET health_outcome=$2,health_observed_at=clock_timestamp() WHERE id=$1`, j.AttemptID, sample); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO provider_health(provider_id,status,samples_since) VALUES($1,'HEALTHY','epoch') ON CONFLICT(provider_id) DO NOTHING`, j.Provider.ID); err != nil {
		return err
	}
	var state, probe string
	if err := tx.QueryRowContext(ctx, `SELECT circuit_state,coalesce(probe_attempt_id::text,'') FROM provider_health WHERE provider_id=$1 FOR UPDATE`, j.Provider.ID).Scan(&state, &probe); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE provider_health SET last_success_at=CASE WHEN $2='SUCCESS' THEN clock_timestamp() ELSE last_success_at END,
 last_failure_at=CASE WHEN $2='FAILURE' THEN clock_timestamp() ELSE last_failure_at END,
 last_error=CASE WHEN $2='FAILURE' THEN $3 WHEN $2='SUCCESS' THEN NULL ELSE last_error END,updated_at=clock_timestamp() WHERE provider_id=$1`, j.Provider.ID, sample, out.ErrorClass); err != nil {
		return err
	}
	var total, failed int
	if err := tx.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER(WHERE a.health_outcome='FAILURE') FROM delivery_attempts a JOIN provider_health h ON h.provider_id=a.provider_id
 WHERE a.provider_id=$1 AND a.started_at>=h.samples_since AND a.health_observed_at>clock_timestamp()-interval '10 minutes' AND a.health_outcome IN ('SUCCESS','FAILURE')`, j.Provider.ID).Scan(&total, &failed); err != nil {
		return err
	}
	next := state
	if state == "HALF_OPEN" && probe == j.AttemptID {
		if sample == "SUCCESS" {
			next = "CLOSED"
		} else {
			next = "OPEN"
		}
	} else if state == "CLOSED" && r.HealthEnabled && enabled && total >= 5 && failed*2 >= total {
		next = "OPEN"
	}
	if state == "CLOSED" && next == state {
		if _, err := tx.ExecContext(ctx, `UPDATE provider_health SET status=CASE WHEN $2>0 THEN 'DEGRADED' ELSE 'HEALTHY' END WHERE provider_id=$1`, j.Provider.ID, failed); err != nil {
			return err
		}
	}
	if next != state {
		if _, err := tx.ExecContext(ctx, `UPDATE provider_health SET circuit_state=$2,probe_attempt_id=NULL,
 open_until=CASE WHEN $2='OPEN' THEN clock_timestamp()+interval '1 minute' ELSE NULL END,
 samples_since=CASE WHEN $2='CLOSED' THEN clock_timestamp() ELSE samples_since END,
 status=CASE WHEN $2='OPEN' THEN 'UNHEALTHY' ELSE 'HEALTHY' END,updated_at=clock_timestamp() WHERE provider_id=$1`, j.Provider.ID, next); err != nil {
			return err
		}
		return event(ctx, tx, j, "PROVIDER_CIRCUIT_"+next, "", time.Now().UTC(), map[string]any{"provider_id": j.Provider.ID, "previous_state": state, "outcome": sample, "window_seconds": 600, "minimum_samples": 5, "failure_ratio": 0.5, "cooldown_seconds": 60})
	}
	return nil
}

// Recovery must not reuse an abandoned half-open probe. It does not invent a
// provider failure sample from a worker crash, and never refunds its quota.
func recoverProbe(ctx context.Context, tx *sql.Tx, j Job) error {
	var pid string
	err := tx.QueryRowContext(ctx, `UPDATE provider_health SET circuit_state='OPEN',probe_attempt_id=NULL,open_until=clock_timestamp()+interval '1 minute',status='UNHEALTHY',updated_at=clock_timestamp()
 WHERE probe_attempt_id=$1 RETURNING provider_id`, j.AttemptID).Scan(&pid)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	return event(ctx, tx, j, "PROVIDER_CIRCUIT_OPEN", "", time.Now().UTC(), map[string]any{"provider_id": pid, "reason": "PROBE_LEASE_EXPIRED", "cooldown_seconds": 60})
}
