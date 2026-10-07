package encryption

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"relaytale/internal/database"
)

type RotationReport struct {
	Active   string         `json:"active_version"`
	Versions map[string]int `json:"source_versions"`
	Checked  int            `json:"checked"`
	Changed  int            `json:"changed"`
	Applied  bool           `json:"applied"`
}

// RotateProviders requires an acquired offline ownership context. Every credential,
// including disabled and already-current providers, is authenticated before commit.
func RotateProviders(ctx context.Context, db *sql.DB, box *Box, actor, reason string, apply bool) (RotationReport, error) {
	report := RotationReport{Active: box.ActiveVersion(), Versions: map[string]int{}, Applied: apply}
	fail := errors.New("provider key rotation failed; no successful commit confirmed")
	if report.Active == "legacy" || !utf8.ValidString(actor) || !utf8.ValidString(reason) || strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" || len(actor) > 128 || len(reason) > 2048 || strings.ContainsRune(actor, 0) || strings.ContainsRune(reason, 0) {
		return report, errors.New("rotation requires a versioned keyring and valid actor/reason")
	}
	if err := database.CheckNoRuntime(ctx, db); err != nil {
		return report, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return report, fail
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return report, fail
	}
	rows, err := tx.QueryContext(ctx, `SELECT id::text,password_ciphertext,nonce FROM providers ORDER BY id LIMIT 10001 FOR UPDATE`)
	if err != nil {
		return report, fail
	}
	type record struct {
		id                string
		ciphertext, nonce []byte
	}
	records := []record{}
	for rows.Next() {
		var r record
		if err = rows.Scan(&r.id, &r.ciphertext, &r.nonce); err != nil {
			rows.Close()
			return report, fail
		}
		records = append(records, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(records) > 10000 {
		return report, fail
	}
	for _, r := range records {
		version, err := Version(r.nonce)
		if err != nil {
			return report, fail
		}
		plain, err := box.Open(r.id, r.ciphertext, r.nonce)
		if err != nil {
			return report, fail
		}
		report.Checked++
		report.Versions[version]++
		if version == report.Active {
			continue
		}
		report.Changed++
		if apply {
			encrypted, nonce, err := box.Seal(r.id, plain)
			if err != nil {
				return report, fail
			}
			if _, err = tx.ExecContext(ctx, `UPDATE providers SET password_ciphertext=$2,nonce=$3 WHERE id=$1`, r.id, encrypted, nonce); err != nil {
				return report, fail
			}
		}
	}
	if !apply {
		return report, nil
	}
	payload, err := json.Marshal(struct {
		Reason string         `json:"reason"`
		Report RotationReport `json:"report"`
	}{reason, report})
	if err != nil {
		return report, fail
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO maintenance_audit(id,action,actor,data) VALUES($1,'PROVIDER_KEYS_ROTATED',$2,$3)`, uuid.NewString(), actor, string(payload)); err != nil {
		return report, fail
	}
	if err = tx.Commit(); err != nil {
		return report, fail
	}
	return report, nil
}
