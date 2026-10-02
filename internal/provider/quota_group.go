package provider

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// QuotaGroupSettings never contains credentials; membership is assigned separately.
type QuotaGroupSettings struct {
	Name        string `json:"name"`
	HourlyLimit int    `json:"hourly_limit"`
	DailyLimit  int    `json:"daily_limit"`
}

func validQuotaID(id string) bool {
	u, e := uuid.Parse(id)
	return e == nil && u != uuid.Nil && u.String() == id
}
func validQuotaAudit(actor, reason string) bool {
	return strings.TrimSpace(actor) != "" && len(actor) <= 128 && strings.TrimSpace(reason) != "" && len(reason) <= 2048 && utf8.ValidString(actor+reason) && !strings.ContainsRune(actor+reason, 0)
}

// ManageQuotaGroup uses revision fencing and one transaction for policy and audit.
func ManageQuotaGroup(ctx context.Context, db *sql.DB, create bool, id string, expected int64, s QuotaGroupSettings, actor, reason string) (int64, error) {
	if !validQuotaID(id) || !validQuotaAudit(actor, reason) || strings.TrimSpace(s.Name) == "" || len(s.Name) > 128 || !utf8.ValidString(s.Name) || strings.ContainsAny(s.Name, "\r\n\x00") || s.HourlyLimit < 0 || s.DailyLimit < 0 || s.HourlyLimit > 2147483647 || s.DailyLimit > 2147483647 || (!create && expected < 1) || (create && expected != 0) {
		return 0, ErrInvalid
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return 0, err
	}
	var rev int64
	action := "PROVIDER_QUOTA_GROUP_CREATED"
	if create {
		err = tx.QueryRowContext(ctx, `INSERT INTO provider_quota_groups(id,name,hourly_limit,daily_limit) VALUES($1,$2,$3,$4) RETURNING revision`, id, s.Name, s.HourlyLimit, s.DailyLimit).Scan(&rev)
	} else {
		action = "PROVIDER_QUOTA_GROUP_CONFIGURED"
		if err = tx.QueryRowContext(ctx, `SELECT revision FROM provider_quota_groups WHERE id=$1 FOR NO KEY UPDATE`, id).Scan(&rev); err != nil {
			return 0, err
		}
		if rev != expected {
			return 0, ErrConflict
		}
		err = tx.QueryRowContext(ctx, `UPDATE provider_quota_groups SET name=$2,hourly_limit=$3,daily_limit=$4 WHERE id=$1 RETURNING revision`, id, s.Name, s.HourlyLimit, s.DailyLimit).Scan(&rev)
	}
	if err != nil {
		return 0, err
	}
	if err = quotaAudit(ctx, tx, action, actor, map[string]any{"group_id": id, "revision": rev, "expected_revision": expected, "settings": s, "reason": reason}); err != nil {
		return 0, err
	}
	return rev, tx.Commit()
}

// AttachQuotaGroup follows the worker's provider -> group lock order. Once
// attached, all historical reservations count, including pre-attachment attempts.
func AttachQuotaGroup(ctx context.Context, db *sql.DB, pid, gid string, expected int64, actor, reason string) (int64, error) {
	if !validQuotaID(pid) || !validQuotaID(gid) || expected < 1 || !validQuotaAudit(actor, reason) {
		return 0, ErrInvalid
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return 0, err
	}
	var rev int64
	var current string
	if err = tx.QueryRowContext(ctx, `SELECT revision,coalesce(quota_group_id::text,'') FROM providers WHERE id=$1 FOR UPDATE`, pid).Scan(&rev, &current); err != nil {
		return 0, err
	}
	if rev != expected || current != "" {
		return 0, ErrConflict
	}
	var locked string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM provider_quota_groups WHERE id=$1 FOR NO KEY UPDATE`, gid).Scan(&locked); err != nil {
		return 0, err
	}
	if err = tx.QueryRowContext(ctx, `UPDATE providers SET quota_group_id=$2 WHERE id=$1 RETURNING revision`, pid, gid).Scan(&rev); err != nil {
		return 0, err
	}
	if err = quotaAudit(ctx, tx, "PROVIDER_QUOTA_GROUP_ATTACHED", actor, map[string]any{"provider_id": pid, "group_id": gid, "revision": rev, "expected_revision": expected, "reason": reason}); err != nil {
		return 0, err
	}
	return rev, tx.Commit()
}
func quotaAudit(ctx context.Context, tx *sql.Tx, action, actor string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO maintenance_audit(id,action,actor,data) VALUES($1,$2,$3,$4)`, uuid.NewString(), action, actor, string(raw))
	return err
}
