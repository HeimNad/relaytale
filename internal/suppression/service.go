// Package suppression implements global address policy, independent of SMTP evidence.
package suppression

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

var ErrActive = errors.New("address already has an active suppression")
var ErrStale = errors.New("suppression is absent or already released")
var ErrBlocked = errors.New("recipient is suppressed")

type Service struct{ DB *sql.DB }
type AddRequest struct {
	Email     string     `json:"email"`
	Category  string     `json:"category"`
	Actor     string     `json:"actor"`
	Reason    string     `json:"reason"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}
type Entry struct {
	Actor      string     `json:"actor"`
	Note       string     `json:"note"`
	ID         string     `json:"id"`
	Email      string     `json:"email"`
	Category   string     `json:"category"`
	Source     string     `json:"source"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	ReleasedAt *time.Time `json:"released_at"`
	Active     bool       `json:"active"`
}

// Lock serializes policy mutations against admission. Shared readers can run in
// parallel. Writers never lock messages; claim uses SKIP LOCKED, and the
// other admission paths lock their message before acquiring the policy lock.
// Keep this transaction-only lock out of all network and filesystem operations.
func Lock(ctx context.Context, tx *sql.Tx, write bool) error {
	q := `SELECT pg_advisory_xact_lock_shared(734219501)`
	if write {
		q = `SELECT pg_advisory_xact_lock(734219501)`
	}
	_, err := tx.ExecContext(ctx, q)
	return err
}
func validateEmail(s string) error {
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || len(s) > 320 || strings.ContainsAny(s, "\r\n\x00") || !utf8.ValidString(s) {
		return errors.New("a single bare mailbox address is required")
	}
	return nil
}
func validateAudit(actor, reason string) error {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" || len(actor) > 128 || len(reason) > 2048 || !utf8.ValidString(actor+reason) || strings.ContainsRune(actor+reason, 0) {
		return errors.New("actor (1..128 bytes) and reason (1..2048 bytes) required")
	}
	return nil
}
func begin(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		tx.Rollback()
		return nil, err
	}
	if err = Lock(ctx, tx, true); err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}
func audit(ctx context.Context, tx *sql.Tx, action, actor string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO maintenance_audit(id,action,actor,data) VALUES($1,$2,$3,$4)`, uuid.NewString(), action, actor, string(raw))
	return err
}
func (s Service) Add(ctx context.Context, v AddRequest) (string, error) {
	if err := validateEmail(v.Email); err != nil {
		return "", err
	}
	if err := validateAudit(v.Actor, v.Reason); err != nil {
		return "", err
	}
	switch v.Category {
	case "manual", "hard_bounce", "complaint", "invalid", "unsubscribe":
	default:
		return "", errors.New("invalid suppression category")
	}
	tx, err := begin(ctx, s.DB)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var future bool
	if err = tx.QueryRowContext(ctx, `SELECT $1::timestamptz IS NULL OR $1>clock_timestamp()`, v.ExpiresAt).Scan(&future); err != nil {
		return "", err
	}
	if !future {
		return "", errors.New("expiry must be in the future")
	}
	var active bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM suppression_entries WHERE lower(email)=lower($1) AND released_at IS NULL AND (expires_at IS NULL OR expires_at>clock_timestamp()))`, v.Email).Scan(&active); err != nil {
		return "", err
	}
	if active {
		return "", ErrActive
	}
	// Keep expired rows as history; retire their unique slot before creating a new ID.
	if _, err = tx.ExecContext(ctx, `UPDATE suppression_entries SET released_at=clock_timestamp() WHERE lower(email)=lower($1) AND released_at IS NULL`, v.Email); err != nil {
		return "", err
	}
	id := uuid.Must(uuid.NewV7()).String()
	if _, err = tx.ExecContext(ctx, `INSERT INTO suppression_entries(id,email,reason,source,expires_at,created_by,note) VALUES($1,$2,$3,'operator',$4,$5,$6)`, id, v.Email, v.Category, v.ExpiresAt, v.Actor, v.Reason); err != nil {
		return "", err
	}
	if err = audit(ctx, tx, "SUPPRESSION_ADDED", v.Actor, map[string]any{"id": id, "request": v, "scope": "gateway_global", "normalization": "postgres_lower_entire_address"}); err != nil {
		return "", err
	}
	return id, tx.Commit()
}
func (s Service) Release(ctx context.Context, id, actor, reason string) error {
	if _, err := uuid.Parse(id); err != nil {
		return errors.New("valid suppression UUID required")
	}
	if err := validateAudit(actor, reason); err != nil {
		return err
	}
	tx, err := begin(ctx, s.DB)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE suppression_entries SET released_at=clock_timestamp() WHERE id=$1 AND released_at IS NULL`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrStale
	}
	if err = audit(ctx, tx, "SUPPRESSION_RELEASED", actor, map[string]string{"id": id, "reason": reason}); err != nil {
		return err
	}
	return tx.Commit()
}

// List uses stable UUID keyset pagination and includes expired/released history.
func (s Service) List(ctx context.Context, email, after string, limit int) ([]Entry, error) {
	if email != "" {
		if err := validateEmail(email); err != nil {
			return nil, err
		}
	}
	if after != "" {
		if _, err := uuid.Parse(after); err != nil {
			return nil, errors.New("invalid cursor")
		}
	}
	if limit < 1 || limit > 200 {
		return nil, errors.New("limit must be 1..200")
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id,email,reason,coalesce(source,''),created_by,note,created_at,expires_at,released_at,released_at IS NULL AND (expires_at IS NULL OR expires_at>clock_timestamp()) FROM suppression_entries WHERE ($1='' OR lower(email)=lower($1)) AND ($2='' OR id>NULLIF($2,'')::uuid) ORDER BY id LIMIT $3`, email, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		if err = rows.Scan(&e.ID, &e.Email, &e.Category, &e.Source, &e.Actor, &e.Note, &e.CreatedAt, &e.ExpiresAt, &e.ReleasedAt, &e.Active); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ActiveSQL must receive a trusted SQL expression, never user input. Matching
// deliberately uses the database's lower() for both index and lookup.
func ActiveSQL(address string) string {
	return `EXISTS(SELECT 1 FROM suppression_entries se WHERE lower(se.email)=lower(` + address + `) AND se.released_at IS NULL AND (se.expires_at IS NULL OR se.expires_at>clock_timestamp()))`
}

// Apply requires the message lock (or an uncommitted new message) and policy
// read lock. It preserves terminal/UNKNOWN facts and records every projection.
// Sending recipients may only be included by the unarmed DATA fence.
func Apply(ctx context.Context, tx *sql.Tx, mid, attempt string, sending bool) (int, error) {
	states := `('QUEUED','TEMP_FAILED')`
	if sending {
		states = `('SENDING')`
	}
	rows, err := tx.QueryContext(ctx, `SELECT r.id,se.id FROM recipients r JOIN suppression_entries se ON lower(se.email)=lower(r.address) AND se.released_at IS NULL AND (se.expires_at IS NULL OR se.expires_at>clock_timestamp()) WHERE r.message_id=$1 AND r.status IN `+states+` ORDER BY r.id`, mid)
	if err != nil {
		return 0, err
	}
	type hit struct{ rid, sid string }
	var hits []hit
	for rows.Next() {
		var h hit
		if err = rows.Scan(&h.rid, &h.sid); err != nil {
			rows.Close()
			return 0, err
		}
		hits = append(hits, h)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, h := range hits {
		if _, err = tx.ExecContext(ctx, `UPDATE recipients SET status='SUPPRESSED',retry_at=NULL,retry_automatic=false,completed_at=clock_timestamp(),decision=jsonb_build_object('version',1,'action','SUPPRESS','scope','policy','terminal',true,'may_have_delivered',false,'failover_allowed',false,'suppression_id',$2::text) WHERE id=$1`, h.rid, h.sid); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO events(id,message_id,recipient_id,attempt_id,event_type,source,data) VALUES($1,$2,$3,NULLIF($4,'')::uuid,'RECIPIENT_SUPPRESSED','policy',jsonb_build_object('suppression_id',$5::text,'boundary',$6::text))`, uuid.NewString(), mid, h.rid, attempt, h.sid, map[bool]string{true: "before_data", false: "pending"}[sending]); err != nil {
			return 0, err
		}
	}
	return len(hits), nil
}
