package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
)

var ErrTemplateCatalog = errors.New("invalid template publication, identity or catalog limits")

// VerifyPublishedTemplates streams the catalog during offline recovery; no
// source is returned, and only one bounded definition is parsed at a time.
func VerifyPublishedTemplates(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) error {
	rows, err := q.QueryContext(ctx, `SELECT v.api_key_id,v.template_id,v.version,v.language,v.definition,v.definition_sha256,k.prefix,a.action,a.data->>'api_key_id',a.data->>'id',a.data->>'version',a.data->>'definition_sha256',(NOT (a.data ? 'draft_revision') OR EXISTS(SELECT 1 FROM api_template_drafts d JOIN maintenance_audit da ON da.id=d.audit_id WHERE da.action='TEMPLATE_DRAFT_SAVED' AND da.data->>'api_key_id'=d.api_key_id::text AND da.data->>'id'=d.template_id AND da.data->>'revision'=d.revision::text AND da.data->>'version'=d.version AND da.data->>'definition_sha256'=d.definition_sha256 AND d.api_key_id=v.api_key_id AND d.template_id=v.template_id AND d.revision::text=a.data->>'draft_revision' AND d.version=v.version AND d.definition_sha256=v.definition_sha256)),EXISTS(SELECT 1 FROM api_template_retirements r WHERE r.api_key_id=v.api_key_id AND r.template_id=v.template_id AND r.version=v.version)
 FROM api_template_versions v JOIN api_keys k ON k.id=v.api_key_id JOIN maintenance_audit a ON a.id=v.audit_id ORDER BY v.api_key_id,v.template_id,v.version`)
	if err != nil {
		return err
	}
	defer rows.Close()
	last := ""
	count, size := 0, 0
	activeCount, activeSize := 0, 0
	for rows.Next() {
		var identity, id, version, language, definition, hash, prefix, action string
		var provenanceValid, retired bool
		var auditIdentity, auditID, auditVersion, auditHash sql.NullString
		if err = rows.Scan(&identity, &id, &version, &language, &definition, &hash, &prefix, &action, &auditIdentity, &auditID, &auditVersion, &auditHash, &provenanceValid, &retired); err != nil {
			return err
		}
		if identity != last {
			last = identity
			count = 0
			size = 0
			activeCount, activeSize = 0, 0
		}
		count++
		if !retired {
			activeCount++
			activeSize += len(definition)
		}
		if activeCount > 8 || activeSize > 65536 {
			return ErrTemplateCatalog
		}
		size += len(definition)
		if !provenanceValid || count > 64 || size > 524288 || prefix != "rts_" || action != "TEMPLATE_PUBLISHED" || !auditIdentity.Valid || auditIdentity.String != identity || !auditID.Valid || auditID.String != id || !auditVersion.Valid || auditVersion.String != version || !auditHash.Valid || auditHash.String != hash {
			return ErrTemplateCatalog
		}
		digest := sha256.Sum256([]byte(definition))
		t, err := VersionTemplateJSON([]byte(definition))
		if err != nil || t.ID != id || t.Version != version || t.Language != language || hex.EncodeToString(digest[:]) != hash {
			return ErrTemplateCatalog
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := verifyTemplateRetirements(ctx, q); err != nil {
		return err
	}
	return VerifyTemplateDrafts(ctx, q)
}

type PublishedTemplate struct {
	ID          string     `json:"id"`
	Version     string     `json:"version"`
	Language    string     `json:"language"`
	Bytes       int        `json:"bytes"`
	PublishedAt time.Time  `json:"published_at"`
	AuditID     string     `json:"audit_id"`
	RetiredAt   *time.Time `json:"retired_at,omitempty"`
}

func canonicalTemplate(t SendTemplate) ([]byte, error) {
	t.Variables = append([]string(nil), t.Variables...)
	sort.Strings(t.Variables)
	raw, err := json.Marshal(t)
	if err != nil || len(raw) > 32<<10 {
		return nil, ErrTemplateCatalog
	}
	return raw, nil
}

// PublishTemplate locks the identity before checking the bounded catalog and
// commits immutable content with its audit row. Success requires commit.
func PublishTemplate(ctx context.Context, db *sql.DB, identity string, raw []byte, actor, reason string) (bool, error) {
	id, err := uuid.Parse(identity)
	if err != nil || id == uuid.Nil || id.String() != identity || !safeJSONHeader(actor, 128) || actor == "" || !safeJSONHeader(reason, 256) || reason == "" {
		return false, ErrTemplateCatalog
	}
	t, err := VersionTemplateJSON(raw)
	if err != nil {
		return false, ErrTemplateCatalog
	}
	// Publication requires the reviewed artifact to already carry its version.
	var supplied SendTemplate
	if json.Unmarshal(raw, &supplied) != nil || supplied.Version == "" {
		return false, ErrTemplateCatalog
	}
	definition, err := canonicalTemplate(t)
	if err != nil {
		return false, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SET LOCAL synchronous_commit=on"); err != nil {
		return false, err
	}
	replayed, err := publishTemplateTx(ctx, tx, identity, t, definition, actor, reason, nil)
	if err != nil {
		return false, err
	}
	return replayed, tx.Commit()
}

// Caller holds a transaction; publication and optional draft provenance commit together.
func publishTemplateTx(ctx context.Context, tx *sql.Tx, identity string, t SendTemplate, definition []byte, actor, reason string, provenance map[string]any) (bool, error) {
	var err error
	var enabled bool
	if err = tx.QueryRowContext(ctx, "SELECT enabled FROM api_keys WHERE id=$1 AND prefix='rts_' FOR UPDATE", identity).Scan(&enabled); err != nil || !enabled {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		return false, ErrTemplateCatalog
	}
	var stored string
	var auditValid bool
	err = tx.QueryRowContext(ctx, `SELECT v.definition,coalesce(a.action='TEMPLATE_PUBLISHED' AND a.data->>'api_key_id'=v.api_key_id::text AND a.data->>'id'=v.template_id AND a.data->>'version'=v.version AND a.data->>'definition_sha256'=v.definition_sha256 AND (NOT (a.data ? 'draft_revision') OR EXISTS(SELECT 1 FROM api_template_drafts d JOIN maintenance_audit da ON da.id=d.audit_id WHERE da.action='TEMPLATE_DRAFT_SAVED' AND da.data->>'api_key_id'=d.api_key_id::text AND da.data->>'id'=d.template_id AND da.data->>'revision'=d.revision::text AND da.data->>'version'=d.version AND da.data->>'definition_sha256'=d.definition_sha256 AND d.api_key_id=v.api_key_id AND d.template_id=v.template_id AND d.revision::text=a.data->>'draft_revision' AND d.version=v.version AND d.definition_sha256=v.definition_sha256)),false) FROM api_template_versions v JOIN maintenance_audit a ON a.id=v.audit_id WHERE v.api_key_id=$1 AND v.template_id=$2 AND v.version=$3`, identity, t.ID, t.Version).Scan(&stored, &auditValid)
	if err == nil {
		if !auditValid || stored != string(definition) {
			return false, ErrTemplateCatalog
		}
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	var count, size int
	if err = tx.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(octet_length(definition)),0) FROM api_template_versions WHERE api_key_id=$1", identity).Scan(&count, &size); err != nil {
		return false, err
	}
	if count >= 64 || size+len(definition) > 524288 {
		return false, ErrTemplateCatalog
	}
	var invalidRetirement bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM api_template_retirements r JOIN maintenance_audit a ON a.id=r.audit_id WHERE r.api_key_id=$1 AND NOT coalesce(a.action='TEMPLATE_RETIRED' AND a.data->>'api_key_id'=r.api_key_id::text AND a.data->>'id'=r.template_id AND a.data->>'version'=r.version,false))`, identity).Scan(&invalidRetirement); err != nil {
		return false, err
	}
	if invalidRetirement {
		return false, ErrTemplateCatalog
	}
	var activeCount, activeSize int
	if err = tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(octet_length(v.definition)),0) FROM api_template_versions v WHERE v.api_key_id=$1 AND NOT EXISTS(SELECT 1 FROM api_template_retirements r WHERE r.api_key_id=v.api_key_id AND r.template_id=v.template_id AND r.version=v.version)`, identity).Scan(&activeCount, &activeSize); err != nil {
		return false, err
	}
	if activeCount >= 8 || activeSize+len(definition) > 65536 {
		return false, ErrTemplateCatalog
	}
	digest := sha256.Sum256(definition)
	hash := hex.EncodeToString(digest[:])
	auditID := uuid.NewString()
	metadata := map[string]any{"api_key_id": identity, "id": t.ID, "version": t.Version, "language": t.Language, "definition_sha256": hash, "reason": reason}
	for k, v := range provenance {
		metadata[k] = v
	}
	data, _ := json.Marshal(metadata)
	if _, err = tx.ExecContext(ctx, "INSERT INTO maintenance_audit(id,action,actor,data) VALUES($1,'TEMPLATE_PUBLISHED',$2,$3)", auditID, actor, string(data)); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO api_template_versions(api_key_id,template_id,version,language,definition,definition_sha256,audit_id) VALUES($1,$2,$3,$4,$5,$6,$7)", identity, t.ID, t.Version, t.Language, string(definition), hash, auditID); err != nil {
		return false, err
	}
	return false, nil
}

// Metadata only: never export template source or request variables in listings.
func ListPublishedTemplates(ctx context.Context, db *sql.DB, identity string) ([]PublishedTemplate, error) {
	id, err := uuid.Parse(identity)
	if err != nil || id == uuid.Nil || id.String() != identity {
		return nil, ErrTemplateCatalog
	}
	rows, err := db.QueryContext(ctx, "SELECT v.template_id,v.version,v.language,octet_length(v.definition),v.published_at,v.audit_id,r.retired_at FROM api_template_versions v LEFT JOIN api_template_retirements r USING(api_key_id,template_id,version) WHERE v.api_key_id=$1 ORDER BY v.template_id,v.version LIMIT 65", identity)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PublishedTemplate{}
	for rows.Next() {
		var v PublishedTemplate
		if err = rows.Scan(&v.ID, &v.Version, &v.Language, &v.Bytes, &v.PublishedAt, &v.AuditID, &v.RetiredAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if len(out) > 64 {
		return nil, ErrTemplateCatalog
	}
	return out, rows.Err()
}

// A transaction snapshots the explicit directory at startup. No request-time
// compilation or mutable lookup can silently change an in-flight submission.
func loadPublishedTemplates(ctx context.Context, tx *sql.Tx, key *SendKey) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT v.template_id,v.version,v.language,v.definition,v.definition_sha256,coalesce(a.action='TEMPLATE_PUBLISHED' AND a.data->>'api_key_id'=v.api_key_id::text AND a.data->>'id'=v.template_id AND a.data->>'version'=v.version AND a.data->>'definition_sha256'=v.definition_sha256 AND (NOT (a.data ? 'draft_revision') OR EXISTS(SELECT 1 FROM api_template_drafts d JOIN maintenance_audit da ON da.id=d.audit_id WHERE da.action='TEMPLATE_DRAFT_SAVED' AND da.data->>'api_key_id'=d.api_key_id::text AND da.data->>'id'=d.template_id AND da.data->>'revision'=d.revision::text AND da.data->>'version'=d.version AND da.data->>'definition_sha256'=d.definition_sha256 AND d.api_key_id=v.api_key_id AND d.template_id=v.template_id AND d.revision::text=a.data->>'draft_revision' AND d.version=v.version AND d.definition_sha256=v.definition_sha256)),false),r.retired_at,coalesce(r.audit_id IS NULL OR (ra.action='TEMPLATE_RETIRED' AND ra.data->>'api_key_id'=v.api_key_id::text AND ra.data->>'id'=v.template_id AND ra.data->>'version'=v.version),false) FROM api_template_versions v JOIN maintenance_audit a ON a.id=v.audit_id LEFT JOIN api_template_retirements r USING(api_key_id,template_id,version) LEFT JOIN maintenance_audit ra ON ra.id=r.audit_id WHERE v.api_key_id=$1 ORDER BY v.template_id,v.version LIMIT 65`, key.ID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	count, size := 0, 0
	totalSize := 0
	for rows.Next() {
		count++
		if count > 64 {
			return 0, ErrTemplateCatalog
		}
		var id, version, language, definition, hash string
		var auditValid, retirementValid bool
		var retiredAt sql.NullTime
		if err = rows.Scan(&id, &version, &language, &definition, &hash, &auditValid, &retiredAt, &retirementValid); err != nil {
			return 0, err
		}
		digest := sha256.Sum256([]byte(definition))
		if !auditValid || !retirementValid || hex.EncodeToString(digest[:]) != hash {
			return 0, ErrTemplateCatalog
		}
		t, err := VersionTemplateJSON([]byte(definition))
		if err != nil || t.ID != id || t.Version != version || t.Language != language {
			return 0, ErrTemplateCatalog
		}
		totalSize += len(definition)
		if totalSize > 524288 {
			return 0, ErrTemplateCatalog
		}
		if retiredAt.Valid {
			for _, static := range key.Templates {
				if static.ID == id && static.Version == version {
					return 0, ErrTemplateCatalog
				}
			}
			continue
		}
		size += len(definition)
		if size > 65536 {
			return 0, ErrTemplateCatalog
		}
		duplicate := false
		for _, existing := range key.Templates {
			if existing.ID == t.ID && existing.Version == t.Version {
				duplicate = true
				break
			}
		}
		if !duplicate {
			key.Templates = append(key.Templates, t)
		}
	}
	if err = rows.Err(); err != nil {
		return 0, err
	}
	if err = prepareTemplates(key.Templates); err != nil {
		return 0, ErrTemplateCatalog
	}
	return size, nil
}
