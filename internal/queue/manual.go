package queue

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"relaytale/internal/suppression"
)

type Resolution struct {
	RecipientID          string `json:"recipient_id"`
	ExpectedAttempt      string `json:"expected_attempt"`
	Action               string `json:"action"`
	Actor                string `json:"actor"`
	Reason               string `json:"reason"`
	AcknowledgeDuplicate bool   `json:"acknowledge_duplicate_risk"`
}

var ErrStaleResolution = errors.New("recipient is not an unresolved UNKNOWN from the expected latest attempt")

// ResolveUnknown records an operator assertion without rewriting SMTP evidence.
func (r Repository) ResolveUnknown(ctx context.Context, v Resolution) (string, error) {
	for _, id := range []string{v.RecipientID, v.ExpectedAttempt} {
		if _, err := uuid.Parse(id); err != nil {
			return "", errors.New("valid recipient and expected attempt UUIDs required")
		}
	}
	if strings.TrimSpace(v.Actor) == "" || strings.TrimSpace(v.Reason) == "" || len(v.Actor) > 128 || len(v.Reason) > 2048 || !utf8.ValidString(v.Actor+v.Reason) {
		return "", errors.New("actor (1..128 bytes) and reason (1..2048 bytes) required")
	}
	status := ""
	switch v.Action {
	case "retry":
		if !v.AcknowledgeDuplicate {
			return "", errors.New("manual retry requires explicit acknowledgement of duplicate delivery risk")
		}
		status = "QUEUED"
	case "mark-delivered":
		status = "DELIVERED"
	case "mark-failed":
		status = "PERM_FAILED"
	default:
		return "", errors.New("action must be retry, mark-delivered or mark-failed")
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var mid, archive string
	var active bool
	err = tx.QueryRowContext(ctx, `SELECT m.id,m.archive_state,m.locked_by IS NOT NULL FROM messages m JOIN recipients r ON r.message_id=m.id WHERE r.id=$1 FOR UPDATE OF m`, v.RecipientID).Scan(&mid, &archive, &active)
	if err != nil {
		return "", err
	}
	if active {
		return "", ErrStaleResolution
	}
	var current, attempt, attemptState, provider string
	err = tx.QueryRowContext(ctx, `SELECT r.status,a.id,ar.status,a.provider_id FROM recipients r JOIN attempt_recipients ar ON ar.recipient_id=r.id JOIN delivery_attempts a ON a.id=ar.attempt_id WHERE r.id=$1 ORDER BY a.attempt_number DESC LIMIT 1`, v.RecipientID).Scan(&current, &attempt, &attemptState, &provider)
	if err != nil {
		return "", err
	}
	if current != "DELIVERY_UNKNOWN" || attemptState != "DELIVERY_UNKNOWN" || attempt != v.ExpectedAttempt {
		return "", ErrStaleResolution
	}
	if v.Action == "retry" && archive != "AVAILABLE" {
		return "", errors.New("original archive unavailable; cannot retry")
	}
	if v.Action == "retry" {
		if err = suppression.Lock(ctx, tx, false); err != nil {
			return "", err
		}
		var blocked bool
		if err = tx.QueryRowContext(ctx, `SELECT `+suppression.ActiveSQL("r.address")+` FROM recipients r WHERE id=$1`, v.RecipientID).Scan(&blocked); err != nil {
			return "", err
		}
		if blocked {
			return "", suppression.ErrBlocked
		}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recipients SET status=$2,retry_at=NULL,retry_automatic=false,decision=jsonb_build_object('version',1,'action','MANUAL_RESOLUTION','resolution',$3::jsonb),completed_at=CASE WHEN $2='QUEUED' THEN NULL ELSE now() END WHERE id=$1`, v.RecipientID, status, string(raw)); err != nil {
		return "", err
	}
	if v.Action == "retry" {
		if _, err = tx.ExecContext(ctx, `UPDATE messages SET route_provider_id=$2 WHERE id=$1`, mid, provider); err != nil {
			return "", err
		}
	}
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO events(id,message_id,recipient_id,attempt_id,event_type,source,data) VALUES($1,$2,$3,$4,'UNKNOWN_RESOLVED','operator',$5)`, id.String(), mid, v.RecipientID, attempt, string(raw)); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO maintenance_audit(id,action,actor,data) VALUES($1,'UNKNOWN_RESOLVED',$2,$3)`, id.String(), v.Actor, string(raw)); err != nil {
		return "", err
	}
	if err = refresh(ctx, tx, mid); err != nil {
		return "", err
	}
	return id.String(), tx.Commit()
}
