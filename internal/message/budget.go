package message

import (
	"context"
	"database/sql"
	"errors"
	"time"
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
var ErrStreamDenied = errors.New("message stream not enabled")
var ErrStreamBudget = errors.New("message stream budget exhausted")

var ErrSubmissionDisabled = errors.New("website submission disabled")

// reserveSubmission serializes a website's admission across all processes. The
// NO KEY UPDATE lock is compatible with FK KEY SHARE locks already taken by the
// idempotency ledger, avoiding a concurrent lock-upgrade deadlock. Always read
// usage in a separate statement after acquiring the lock (READ COMMITTED).
func reserveSubmission(ctx context.Context, tx *sql.Tx, identity, id string, units int, stream string, expiresAt time.Time) error {
	stream, err := CanonicalStream(stream)
	if err != nil {
		return err
	}
	var b SubmissionBudget
	var enabled bool
	if err := tx.QueryRowContext(ctx, `SELECT submission_minute_limit,submission_hour_limit,submission_day_limit,enabled FROM api_keys WHERE id=$1 FOR NO KEY UPDATE`, identity).Scan(&b.Minute, &b.Hour, &b.Day, &enabled); err != nil {
		return err
	}
	if !expiresAt.IsZero() {
		var expired, tooFar bool
		if err = tx.QueryRowContext(ctx, `SELECT $1::timestamptz<=clock_timestamp(),$1::timestamptz>clock_timestamp()+interval '720 hours'`, expiresAt).Scan(&expired, &tooFar); err != nil {
			return err
		}
		if expired {
			return ErrExpiredSubmission
		}
		if tooFar {
			return ErrInvalidExpiry
		}
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

	var sb SubmissionBudget
	var allowed bool
	if err = tx.QueryRowContext(ctx, `SELECT enabled,minute_limit,hour_limit,day_limit FROM api_message_streams WHERE api_key_id=$1 AND name=$2`, identity, stream).Scan(&allowed, &sb.Minute, &sb.Hour, &sb.Day); err == sql.ErrNoRows {
		return ErrStreamDenied
	} else if err != nil {
		return err
	}
	if !allowed {
		return ErrStreamDenied
	}
	for _, limit := range []int{sb.Minute, sb.Hour, sb.Day} {
		if limit > 0 && units > limit {
			return ErrSubmissionBatchTooLarge
		}
	}
	var streamUsed [3]int64
	if err = tx.QueryRowContext(ctx, `SELECT coalesce(sum(units) FILTER(WHERE reserved_at>clock_timestamp()-interval '1 minute'),0),coalesce(sum(units) FILTER(WHERE reserved_at>clock_timestamp()-interval '1 hour'),0),coalesce(sum(units),0) FROM api_submission_quota WHERE api_key_id=$1 AND message_stream=$2 AND reserved_at>clock_timestamp()-interval '24 hours'`, identity, stream).Scan(&streamUsed[0], &streamUsed[1], &streamUsed[2]); err != nil {
		return err
	}
	for i, limit := range []int{sb.Minute, sb.Hour, sb.Day} {
		if limit > 0 && streamUsed[i]+int64(units) > int64(limit) {
			return ErrStreamBudget
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO api_submission_quota(message_id,api_key_id,units,message_stream) VALUES($1,$2,$3,$4)`, id, identity, units, stream)
	return err
}
