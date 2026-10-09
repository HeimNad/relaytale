package message

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrInvalidSchedule = errors.New("schedule must be future, within 30 days and before expiry")
var ErrScheduleCapacity = errors.New("future scheduled queue capacity exhausted")

// The caller holds the website identity lock through archive publication and
// queue commit. Replays return before this check, even after the scheduled time.
func admitSchedule(ctx context.Context, tx *sql.Tx, identity string, at, expiry time.Time, size int64) error {
	if at.IsZero() {
		return nil
	}
	if !expiry.IsZero() && !expiry.After(at) {
		return ErrInvalidSchedule
	}
	var valid bool
	if err := tx.QueryRowContext(ctx, `SELECT $1::timestamptz>clock_timestamp() AND $1::timestamptz<=clock_timestamp()+interval '720 hours'`, at).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return ErrInvalidSchedule
	}
	var count, bytes int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(eml_size),0) FROM messages WHERE api_key_id=$1 AND environment='production' AND schedule_at>clock_timestamp() AND status IN ('QUEUED','TEMP_FAILED','PARTIAL_ACCEPTED','SENDING','DELIVERY_UNKNOWN')`, identity).Scan(&count, &bytes); err != nil {
		return err
	}
	if count >= 100 || bytes+size > 128<<20 {
		return ErrScheduleCapacity
	}
	return nil
}
