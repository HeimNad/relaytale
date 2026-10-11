package message

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

var ErrControlNotFound = errors.New("message not found")
var ErrNotCancellable = errors.New("message is active, terminal or has delivery evidence")

// cancelPendingTx shares the message row lock with Claim/ArmData. Active leases
// are deliberately refused, even before DATA: success always means no delivery
// attempt can remain in flight. Prior SMTP facts and admission charges survive.
func cancelPendingTx(ctx context.Context, tx *sql.Tx, identity, id, replacement string) (bool, error) {
	var cancelled bool
	err := tx.QueryRowContext(ctx, `SELECT cancelled_at IS NOT NULL FROM messages WHERE id=$1 AND source_type='api' AND api_key_id=$2 AND environment='production' FOR UPDATE`, id, identity).Scan(&cancelled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrControlNotFound
	}
	if err != nil {
		return false, err
	}
	if cancelled {
		if replacement != "" {
			return false, ErrNotCancellable
		}
		return true, nil
	}
	var unsafe bool
	err = tx.QueryRowContext(ctx, `SELECT locked_by IS NOT NULL OR status NOT IN ('QUEUED','TEMP_FAILED') OR EXISTS(SELECT 1 FROM delivery_attempts WHERE message_id=$1 AND (result='IN_PROGRESS' OR data_armed_at IS NOT NULL OR data_started_at IS NOT NULL OR final_response_at IS NOT NULL)) OR EXISTS(SELECT 1 FROM attempt_recipients ar JOIN delivery_attempts a ON a.id=ar.attempt_id WHERE a.message_id=$1 AND ar.status IN ('SMTP_ACCEPTED','DELIVERY_UNKNOWN')) OR EXISTS(SELECT 1 FROM recipients WHERE message_id=$1 AND status NOT IN ('QUEUED','TEMP_FAILED','PERM_FAILED','SUPPRESSED','EXPIRED')) FROM messages WHERE id=$1`, id).Scan(&unsafe)
	if err != nil {
		return false, err
	}
	if unsafe {
		return false, ErrNotCancellable
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recipients SET status='CANCELLED',retry_at=NULL,retry_automatic=false,completed_at=clock_timestamp(),decision='{"version":2,"action":"CANCEL","reason":"WEBSITE_CANCELLED","scope":"policy","terminal":true,"may_have_delivered":false}'::jsonb WHERE message_id=$1 AND status IN ('QUEUED','TEMP_FAILED')`, id); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE messages SET status='CANCELLED',cancelled_at=clock_timestamp(),completed_at=clock_timestamp(),next_attempt_at=NULL WHERE id=$1`, id); err != nil {
		return false, err
	}
	data, _ := json.Marshal(map[string]string{"replacement_message_id": replacement})
	_, err = tx.ExecContext(ctx, `INSERT INTO events(id,message_id,event_type,event_time,source,data) VALUES($1,$2,'MESSAGE_CANCELLED',clock_timestamp(),'api',$3)`, uuid.Must(uuid.NewV7()).String(), id, string(data))
	return false, err
}

func (p Postgres) CancelPending(ctx context.Context, identity, id string) (bool, error) {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return false, err
	}
	var enabled bool
	if err = tx.QueryRowContext(ctx, `SELECT enabled FROM api_keys WHERE id=$1 FOR NO KEY UPDATE`, identity).Scan(&enabled); err != nil {
		return false, err
	}
	if !enabled {
		return false, ErrSubmissionDisabled
	}
	replay, err := cancelPendingTx(ctx, tx, identity, id, "")
	if err != nil {
		return false, err
	}
	return replay, tx.Commit()
}
