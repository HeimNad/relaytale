package feedback

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

type Service struct{ DB *sql.DB }
type ImportRequest struct{ Source, ExternalID, Actor, Reason string }
type Entry struct {
	ID         string          `json:"id"`
	Source     string          `json:"source"`
	ExternalID string          `json:"external_id"`
	SHA256     string          `json:"sha256"`
	ParseState string          `json:"parse_state"`
	Trust      string          `json:"trust"`
	State      string          `json:"state"`
	CreatedAt  time.Time       `json:"created_at"`
	Report     json.RawMessage `json:"report,omitempty"`
}
type ImportResult struct {
	Entry     Entry `json:"entry"`
	Duplicate bool  `json:"duplicate"`
}

var ErrReviewed = errors.New("feedback already reviewed")

func valid(s string, max int) bool {
	return strings.TrimSpace(s) != "" && len(s) <= max && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n")
}
func audit(ctx context.Context, tx *sql.Tx, action, actor string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO maintenance_audit(id,action,actor,data) VALUES($1,$2,$3,$4)`, uuid.NewString(), action, actor, string(b))
	return err
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
	return tx, nil
}

// Import never accepts caller-supplied trust or a delivery-state mutation.
// Unsupported MIME is retained for manual inspection; oversized input is rejected.
func (s Service) Import(ctx context.Context, v ImportRequest, r io.Reader) (ImportResult, error) {
	if !valid(v.Source, 128) || !valid(v.Actor, 128) || !valid(v.Reason, 2048) || (v.ExternalID != "" && !valid(v.ExternalID, 256)) {
		return ImportResult{}, errors.New("source, actor, reason and bounded external ID required")
	}
	raw, err := Read(r)
	if err != nil {
		return ImportResult{}, err
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	if v.ExternalID == "" {
		v.ExternalID = "sha256:" + digest
	}
	parsed, parseErr := Parse(raw)
	state := "PARSED"
	payload := []byte(`{}`)
	if parseErr != nil {
		state = "UNSUPPORTED"
	} else {
		payload, err = json.Marshal(parsed)
		if err != nil {
			return ImportResult{}, err
		}
	}
	tx, err := begin(ctx, s.DB)
	if err != nil {
		return ImportResult{}, err
	}
	defer tx.Rollback()
	id := uuid.Must(uuid.NewV7()).String()
	res, err := tx.ExecContext(ctx, `INSERT INTO feedback_reports(id,source,external_id,sha256,raw,parsed,parse_state,actor,reason) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(source,external_id,sha256) DO NOTHING`, id, v.Source, v.ExternalID, digest, raw, string(payload), state, v.Actor, v.Reason)
	if err != nil {
		return ImportResult{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ImportResult{}, err
	}
	if n == 0 {
		if err = tx.QueryRowContext(ctx, `SELECT id FROM feedback_reports WHERE source=$1 AND external_id=$2 AND sha256=$3`, v.Source, v.ExternalID, digest).Scan(&id); err != nil {
			return ImportResult{}, err
		}
	} else if err = audit(ctx, tx, "FEEDBACK_QUARANTINED", v.Actor, map[string]string{"id": id, "sha256": digest, "source": v.Source, "reason": v.Reason, "trust": "UNVERIFIED"}); err != nil {
		return ImportResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return ImportResult{}, err
	}
	e, err := s.Get(ctx, id)
	return ImportResult{Entry: e, Duplicate: n == 0}, err
}

const columns = `f.id,f.source,f.external_id,f.sha256,f.parse_state,f.trust,
 CASE WHEN EXISTS(SELECT 1 FROM feedback_reports other WHERE other.source=f.source AND other.external_id=f.external_id AND other.sha256<>f.sha256) THEN 'CONFLICT'
 WHEN EXISTS(SELECT 1 FROM feedback_reviews rev WHERE rev.feedback_id=f.id) THEN 'DISMISSED' ELSE 'QUARANTINED' END,f.created_at`

func scan(row interface{ Scan(...any) error }, detail bool) (Entry, error) {
	var e Entry
	args := []any{&e.ID, &e.Source, &e.ExternalID, &e.SHA256, &e.ParseState, &e.Trust, &e.State, &e.CreatedAt}
	if detail {
		args = append(args, &e.Report)
	}
	err := row.Scan(args...)
	return e, err
}
func (s Service) Get(ctx context.Context, id string) (Entry, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Entry{}, errors.New("valid feedback UUID required")
	}
	return scan(s.DB.QueryRowContext(ctx, `SELECT `+columns+`,f.parsed FROM feedback_reports f WHERE f.id=$1`, id), true)
}
func (s Service) List(ctx context.Context, after string, limit int) ([]Entry, error) {
	if after != "" {
		if _, err := uuid.Parse(after); err != nil {
			return nil, errors.New("invalid feedback cursor")
		}
	}
	if limit < 1 || limit > 200 {
		return nil, errors.New("limit must be 1..200")
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+columns+` FROM feedback_reports f WHERE ($1='' OR f.id>NULLIF($1,'')::uuid) ORDER BY f.id LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		e, err := scan(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Dismiss records a review only. Conflicting evidence remains visibly conflicting,
// even when a variant was dismissed earlier. No UNKNOWN or SMTP fact is changed.
func (s Service) Dismiss(ctx context.Context, id, digest, actor, reason string) error {
	if _, err := uuid.Parse(id); err != nil {
		return errors.New("valid feedback UUID required")
	}
	if !valid(actor, 128) || !valid(reason, 2048) {
		return errors.New("actor and reason required")
	}
	tx, err := begin(ctx, s.DB)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var found string
	if err = tx.QueryRowContext(ctx, `SELECT sha256 FROM feedback_reports WHERE id=$1`, id).Scan(&found); err != nil {
		return err
	}
	if found != digest {
		return errors.New("feedback digest does not match reviewed evidence")
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO feedback_reviews(feedback_id,action,actor,reason) VALUES($1,'DISMISS',$2,$3) ON CONFLICT DO NOTHING`, id, actor, reason)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrReviewed
	}
	if err = audit(ctx, tx, "FEEDBACK_DISMISSED", actor, map[string]string{"id": id, "sha256": digest, "reason": reason}); err != nil {
		return err
	}
	return tx.Commit()
}
