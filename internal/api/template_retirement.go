package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
)

// Retirement is irreversible history. Existing process snapshots and queued EML
// remain unchanged; a restart is required to stop admitting the retired version.
func RetireTemplate(ctx context.Context, db *sql.DB, identity, name, version, actor, reason string) (bool, error) {
	id, err := uuid.Parse(identity)
	if err != nil || id == uuid.Nil || id.String() != identity || !templateName(name) || len(version) != 64 || strings.Trim(version, "0123456789abcdef") != "" || strings.TrimSpace(actor) == "" || !safeJSONHeader(actor, 128) || strings.TrimSpace(reason) == "" || !safeJSONHeader(reason, 256) {
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
	var prefix string
	if err = tx.QueryRowContext(ctx, "SELECT prefix FROM api_keys WHERE id=$1 FOR UPDATE", identity).Scan(&prefix); err != nil || prefix != "rts_" {
		return false, ErrTemplateCatalog
	}
	// Verify the targeted publication and any existing retirement audit.
	var valid bool
	if err = tx.QueryRowContext(ctx, `SELECT coalesce(a.action='TEMPLATE_PUBLISHED' AND a.data->>'api_key_id'=v.api_key_id::text AND a.data->>'id'=v.template_id AND a.data->>'version'=v.version AND a.data->>'definition_sha256'=v.definition_sha256 AND (r.audit_id IS NULL OR (ra.action='TEMPLATE_RETIRED' AND ra.data->>'api_key_id'=v.api_key_id::text AND ra.data->>'id'=v.template_id AND ra.data->>'version'=v.version)),false) FROM api_template_versions v JOIN maintenance_audit a ON a.id=v.audit_id LEFT JOIN api_template_retirements r USING(api_key_id,template_id,version) LEFT JOIN maintenance_audit ra ON ra.id=r.audit_id WHERE v.api_key_id=$1 AND v.template_id=$2 AND v.version=$3`, identity, name, version).Scan(&valid); err != nil || !valid {
		return false, ErrTemplateCatalog
	}
	var exists, retired bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM api_template_versions WHERE api_key_id=$1 AND template_id=$2 AND version=$3),EXISTS(SELECT 1 FROM api_template_retirements WHERE api_key_id=$1 AND template_id=$2 AND version=$3)`, identity, name, version).Scan(&exists, &retired); err != nil {
		return false, err
	}
	if !exists {
		return false, ErrTemplateCatalog
	}
	if retired {
		return true, tx.Commit()
	}
	audit := uuid.NewString()
	data, _ := json.Marshal(map[string]string{"api_key_id": identity, "id": name, "version": version, "reason": reason})
	if _, err = tx.ExecContext(ctx, "INSERT INTO maintenance_audit(id,action,actor,data) VALUES($1,'TEMPLATE_RETIRED',$2,$3)", audit, actor, string(data)); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO api_template_retirements(api_key_id,template_id,version,audit_id) VALUES($1,$2,$3,$4)", identity, name, version, audit); err != nil {
		return false, err
	}
	return false, tx.Commit()
}

func verifyTemplateRetirements(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) error {
	rows, err := q.QueryContext(ctx, `SELECT coalesce(a.action='TEMPLATE_RETIRED' AND a.data->>'api_key_id'=r.api_key_id::text AND a.data->>'id'=r.template_id AND a.data->>'version'=r.version,false) FROM api_template_retirements r JOIN maintenance_audit a ON a.id=r.audit_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var valid bool
		if err = rows.Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return ErrTemplateCatalog
		}
	}
	return rows.Err()
}
