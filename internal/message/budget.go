package message

import (
	"context"
	"database/sql"
	"errors"
)

// SubmissionBudget counts newly accepted envelope recipients, not attempts.
// Zero disables a window; existing reservations still count when it is enabled.
type SubmissionBudget struct {
	Minute int `json:"minute"`
	Hour   int `json:"hour"`
	Day    int `json:"day"`
}

func (b SubmissionBudget) Valid() bool {
	return b.Minute >= 0 && b.Hour >= 0 && b.Day >= 0 && b.Minute <= 2147483647 && b.Hour <= 2147483647 && b.Day <= 2147483647
}

var ErrSubmissionBudget = errors.New("website recipient budget exhausted")
var ErrSubmissionBatchTooLarge = errors.New("recipient batch exceeds website budget")
var ErrSubmissionDisabled = errors.New("website submission disabled")

// reserveSubmission serializes a website's admission across all processes. The
// NO KEY UPDATE lock is compatible with FK KEY SHARE locks already taken by the
// idempotency ledger, avoiding a concurrent lock-upgrade deadlock. Always read
// usage in a separate statement after acquiring the lock (READ COMMITTED).
func reserveSubmission(ctx context.Context, tx *sql.Tx, identity, id string, units int) error {
	var b SubmissionBudget
	var enabled bool
	if err := tx.QueryRowContext(ctx, `SELECT submission_minute_limit,submission_hour_limit,submission_day_limit,enabled FROM api_keys WHERE id=$1 FOR NO KEY UPDATE`, identity).Scan(&b.Minute, &b.Hour, &b.Day, &enabled); err != nil {
		return err
	}
	if !enabled {
		return ErrSubmissionDisabled
	}
	limits := []int{b.Minute, b.Hour, b.Day}
	for _, limit := range limits {
		if limit > 0 && units > limit {
			return ErrSubmissionBatchTooLarge
		}
	}
	var used [3]int64
	if err := tx.QueryRowContext(ctx, `SELECT
 coalesce(sum(units) FILTER (WHERE reserved_at>clock_timestamp()-interval '1 minute'),0),
 coalesce(sum(units) FILTER (WHERE reserved_at>clock_timestamp()-interval '1 hour'),0),
 coalesce(sum(units),0) FROM api_submission_quota
 WHERE api_key_id=$1 AND reserved_at>clock_timestamp()-interval '24 hours'`, identity).Scan(&used[0], &used[1], &used[2]); err != nil {
		return err
	}
	for i, limit := range limits {
		if limit > 0 && used[i]+int64(units) > int64(limit) {
			return ErrSubmissionBudget
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO api_submission_quota(message_id,api_key_id,units) VALUES($1,$2,$3)`, id, identity, units)
	return err
}
