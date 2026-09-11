package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// A message has one route. Only switch when every unfinished recipient can
// safely move together. RCPT-specific retries and manual holds retain the route.
// Read the latest immutable attempt decision, not a stale recipient projection
// left behind by lease recovery or manual intervention.
const safeFailover = `m.route_provider_id IS NOT NULL
 AND (SELECT count(DISTINCT a.provider_id) FROM delivery_attempts a WHERE a.message_id=m.id)<3
 AND NOT EXISTS(SELECT 1 FROM recipients rc WHERE rc.message_id=m.id
 AND rc.status NOT IN ('SMTP_ACCEPTED','DELIVERED','PERM_FAILED','BOUNCED','SUPPRESSED','CANCELLED')
 AND (rc.status='QUEUED' AND rc.retry_automatic AND rc.attempt_count<7
 AND rc.retry_started_at>clock_timestamp()-interval '24 hours'
 AND EXISTS(SELECT 1 FROM attempt_recipients ar JOIN delivery_attempts a ON a.id=ar.attempt_id
 WHERE ar.recipient_id=rc.id AND a.provider_id=m.route_provider_id AND a.result='TEMP_FAILED'
 AND ar.decision->>'version'='2'
 AND ar.decision->>'action'='RETRY_SAME_PROVIDER'
 AND ar.decision->>'failover_allowed'='true' AND ar.decision->>'may_have_delivered'='false'
 AND a.attempt_number=(SELECT max(last.attempt_number) FROM delivery_attempts last
 JOIN attempt_recipients last_rc ON last_rc.attempt_id=last.id WHERE last_rc.recipient_id=rc.id))) IS NOT TRUE)`

func (r Repository) eligibleProvider() string {
	route := `(m.route_provider_id IS NULL OR p.id=m.route_provider_id)`
	if r.RetryEnabled && r.FailoverEnabled {
		route = `(m.route_provider_id IS NULL OR p.id=m.route_provider_id OR (` + safeFailover + `
 AND NOT EXISTS(SELECT 1 FROM delivery_attempts visited WHERE visited.message_id=m.id AND visited.provider_id=p.id)))`
	}
	return providerRequirements + r.controlRequirements() + ` AND ` + route
}

// The selected route and its audit evidence commit together with the new claim.
// An unavailable backup falls back to a bounded retry on the pinned provider.
func recordFailover(ctx context.Context, tx *sql.Tx, j Job, previous string) error {

	rows, err := tx.QueryContext(ctx, `SELECT rc.id,ar.attempt_id,ar.decision,a.provider_id FROM recipients rc
 JOIN attempt_recipients ar ON ar.recipient_id=rc.id JOIN delivery_attempts a ON a.id=ar.attempt_id
 WHERE rc.message_id=$1 AND rc.status='SENDING' AND a.provider_id<>$3 AND a.attempt_number=(
 SELECT max(old.attempt_number) FROM delivery_attempts old JOIN attempt_recipients old_rc ON old_rc.attempt_id=old.id
 WHERE old_rc.recipient_id=rc.id AND old.id<>$2)`, j.ID, j.AttemptID, j.Provider.ID)
	if err != nil {
		return err
	}
	type evidence struct {
		id, attempt, provider string
		decision              []byte
	}
	var all []evidence
	for rows.Next() {
		var e evidence
		if err = rows.Scan(&e.id, &e.attempt, &e.decision, &e.provider); err != nil {
			rows.Close()
			return err
		}
		all = append(all, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if previous != "" && previous != j.Provider.ID && len(all) != len(j.Recipients) {
		return errors.New("incomplete failover evidence")
	}
	for _, e := range all {
		if err = event(ctx, tx, j, "PROVIDER_FAILOVER", e.id, time.Now().UTC(), map[string]any{
			"from_provider_id": e.provider, "to_provider_id": j.Provider.ID, "previous_attempt_id": e.attempt,
			"reason": "SAFE_PRE_DATA_FAILURE", "previous_decision": json.RawMessage(e.decision), "max_distinct_providers": 3,
		}); err != nil {
			return err
		}
	}
	return nil
}
