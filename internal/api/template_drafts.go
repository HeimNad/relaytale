package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

var ErrTemplateDraftConflict = errors.New("template draft revision changed")

type TemplateDraft struct {
	ID       string    `json:"id"`
	Revision int64     `json:"revision"`
	Version  string    `json:"version"`
	Bytes    int       `json:"bytes"`
	SavedAt  time.Time `json:"saved_at"`
	AuditID  string    `json:"audit_id"`
}

func draftIdentity(identity, actor, reason string) bool {
	id, err := uuid.Parse(identity)
	return err == nil && id != uuid.Nil && id.String() == identity && strings.TrimSpace(actor) != "" && safeJSONHeader(actor, 128) && strings.TrimSpace(reason) != "" && safeJSONHeader(reason, 256)
}
func lockDraftIdentity(ctx context.Context, tx *sql.Tx, identity string) error {
	var enabled bool
	if err := tx.QueryRowContext(ctx, "SELECT enabled FROM api_keys WHERE id=$1 AND prefix='rts_' FOR UPDATE", identity).Scan(&enabled); err != nil {
		return err
	}
	if !enabled {
		return ErrTemplateCatalog
	}
	return nil
}

// SaveTemplateDraft appends an immutable revision using an optimistic expected
// revision. An exact retry of the immediately preceding successful edit is safe.
func SaveTemplateDraft(ctx context.Context, db *sql.DB, identity string, expected int64, raw []byte, actor, reason string) (int64, bool, error) {
	if !draftIdentity(identity, actor, reason) || expected < 0 || expected > 64 {
		return 0, false, ErrTemplateCatalog
	}
	t, err := VersionTemplateJSON(raw)
	if err != nil {
		return 0, false, ErrTemplateCatalog
	}
	definition, err := canonicalTemplate(t)
	if err != nil {
		return 0, false, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SET LOCAL synchronous_commit=on"); err != nil {
		return 0, false, err
	}
	if err = lockDraftIdentity(ctx, tx, identity); err != nil {
		return 0, false, err
	}
	if err = verifyDraftIdentity(ctx, tx, identity); err != nil {
		return 0, false, err
	}
	var current int64
	var stored string
	err = tx.QueryRowContext(ctx, "SELECT revision,definition FROM api_template_drafts WHERE api_key_id=$1 AND template_id=$2 ORDER BY revision DESC LIMIT 1", identity, t.ID).Scan(&current, &stored)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	if stored == string(definition) && (expected == current || expected+1 == current) {
		return current, true, tx.Commit()
	}
	if expected != current {
		return 0, false, ErrTemplateDraftConflict
	}
	var count, size, latestCount, latestSize int
	if err = tx.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(octet_length(definition)),0) FROM api_template_drafts WHERE api_key_id=$1", identity).Scan(&count, &size); err != nil {
		return 0, false, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(octet_length(definition)),0) FROM (SELECT DISTINCT ON(template_id) template_id,definition FROM api_template_drafts WHERE api_key_id=$1 ORDER BY template_id,revision DESC) d`, identity).Scan(&latestCount, &latestSize); err != nil {
		return 0, false, err
	}
	if count >= 64 || size+len(definition) > 524288 || (current == 0 && latestCount >= 8) || latestSize-len(stored)+len(definition) > 65536 {
		return 0, false, ErrTemplateCatalog
	}
	revision := current + 1
	digest := sha256.Sum256(definition)
	hash := hex.EncodeToString(digest[:])
	audit := uuid.NewString()
	data, _ := json.Marshal(map[string]any{"api_key_id": identity, "id": t.ID, "revision": revision, "version": t.Version, "definition_sha256": hash, "reason": reason})
	if _, err = tx.ExecContext(ctx, "INSERT INTO maintenance_audit(id,action,actor,data) VALUES($1,'TEMPLATE_DRAFT_SAVED',$2,$3)", audit, actor, string(data)); err != nil {
		return 0, false, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO api_template_drafts(api_key_id,template_id,revision,version,definition,definition_sha256,audit_id) VALUES($1,$2,$3,$4,$5,$6,$7)", identity, t.ID, revision, t.Version, string(definition), hash, audit); err != nil {
		return 0, false, err
	}
	if err = tx.Commit(); err != nil {
		return 0, false, err
	}
	return revision, false, nil
}

// GetTemplateDraft exports an explicitly pinned revision for local review. Unlike
// listings this includes source, so only trusted operational callers may use it.
func GetTemplateDraft(ctx context.Context, db *sql.DB, identity, name string, revision int64) (SendTemplate, error) {
	if !draftIdentity(identity, "reader", "review") || !templateName(name) || revision < 1 || revision > 64 {
		return SendTemplate{}, ErrTemplateCatalog
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return SendTemplate{}, err
	}
	defer tx.Rollback()
	if err = verifyDraftIdentity(ctx, tx, identity); err != nil {
		return SendTemplate{}, err
	}
	var raw string
	if err = tx.QueryRowContext(ctx, "SELECT definition FROM api_template_drafts WHERE api_key_id=$1 AND template_id=$2 AND revision=$3", identity, name, revision).Scan(&raw); err != nil {
		return SendTemplate{}, err
	}
	t, err := VersionTemplateJSON([]byte(raw))
	if err != nil {
		return t, err
	}
	return t, tx.Commit()
}

// Publication accepts the latest exact reviewed revision and its content version,
// then reuses immutable catalog publication under the same identity lock.
func PublishTemplateDraft(ctx context.Context, db *sql.DB, identity, name string, revision int64, version, actor, reason string) (bool, error) {
	if !draftIdentity(identity, actor, reason) || !templateName(name) || revision < 1 || revision > 64 || len(version) != 64 {
		return false, ErrTemplateCatalog
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SET LOCAL synchronous_commit=on"); err != nil {
		return false, err
	}
	if err = lockDraftIdentity(ctx, tx, identity); err != nil {
		return false, err
	}
	if err = verifyDraftIdentity(ctx, tx, identity); err != nil {
		return false, err
	}
	var latest int64
	var raw string
	if err = tx.QueryRowContext(ctx, "SELECT revision,definition FROM api_template_drafts WHERE api_key_id=$1 AND template_id=$2 ORDER BY revision DESC LIMIT 1", identity, name).Scan(&latest, &raw); err != nil {
		return false, err
	}
	if latest != revision {
		return false, ErrTemplateDraftConflict
	}
	t, err := VersionTemplateJSON([]byte(raw))
	if err != nil || t.Version != version {
		return false, ErrTemplateCatalog
	}
	replayed, err := publishTemplateTx(ctx, tx, identity, t, []byte(raw), actor, reason, map[string]any{"draft_revision": revision})
	if err != nil {
		return false, err
	}
	return replayed, tx.Commit()
}

func VerifyTemplateDrafts(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) error {
	return verifyDrafts(ctx, q, "")
}
func verifyDraftIdentity(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, identity string) error {
	return verifyDrafts(ctx, q, identity)
}
func verifyDrafts(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, identity string) error {
	rows, err := q.QueryContext(ctx, `SELECT d.api_key_id,d.template_id,d.revision,d.version,d.definition,d.definition_sha256,k.prefix,coalesce(a.action='TEMPLATE_DRAFT_SAVED' AND a.data->>'api_key_id'=d.api_key_id::text AND a.data->>'id'=d.template_id AND a.data->>'revision'=d.revision::text AND a.data->>'version'=d.version AND a.data->>'definition_sha256'=d.definition_sha256,false) FROM api_template_drafts d JOIN api_keys k ON k.id=d.api_key_id JOIN maintenance_audit a ON a.id=d.audit_id WHERE ($1='' OR d.api_key_id::text=$1) ORDER BY d.api_key_id,d.template_id,d.revision`, identity)
	if err != nil {
		return err
	}
	defer rows.Close()
	lastIdentity, lastName := "", ""
	count, size, latestSize, ids := 0, 0, 0, 0
	var previous int64
	previousSize := 0
	for rows.Next() {
		var id, name, version, definition, hash, prefix string
		var revision int64
		var valid bool
		if err = rows.Scan(&id, &name, &revision, &version, &definition, &hash, &prefix, &valid); err != nil {
			return err
		}
		if id != lastIdentity {
			if latestSize+previousSize > 65536 {
				return ErrTemplateCatalog
			}
			lastIdentity = id
			lastName = ""
			count, size, latestSize, ids = 0, 0, 0, 0
			previous, previousSize = 0, 0
		}
		if name != lastName {
			latestSize += previousSize
			lastName = name
			ids++
			previous, previousSize = 0, 0
		}
		count++
		size += len(definition)
		if revision != previous+1 || count > 64 || size > 524288 || ids > 8 || !valid || prefix != "rts_" {
			return ErrTemplateCatalog
		}
		// Only the final revision of each draft contributes to current-head capacity.
		previous, previousSize = revision, len(definition)
		digest := sha256.Sum256([]byte(definition))
		t, e := VersionTemplateJSON([]byte(definition))
		if e != nil || t.ID != name || t.Version != version || hex.EncodeToString(digest[:]) != hash {
			return ErrTemplateCatalog
		}
	}
	if latestSize+previousSize > 65536 {
		return ErrTemplateCatalog
	}
	return rows.Err()
}

// Lists disclose metadata only; source export requires an explicit revision.
func ListTemplateDrafts(ctx context.Context, db *sql.DB, identity string) ([]TemplateDraft, error) {
	if !draftIdentity(identity, "reader", "listing") {
		return nil, ErrTemplateCatalog
	}
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT ON(template_id) template_id,revision,version,octet_length(definition),saved_at,audit_id FROM api_template_drafts WHERE api_key_id=$1 ORDER BY template_id,revision DESC`, identity)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TemplateDraft{}
	for rows.Next() {
		var d TemplateDraft
		if err = rows.Scan(&d.ID, &d.Revision, &d.Version, &d.Bytes, &d.SavedAt, &d.AuditID); err != nil {
			return nil, err
		}
		out = append(out, d)
		if len(out) > 8 {
			return nil, ErrTemplateCatalog
		}
	}
	return out, rows.Err()
}
