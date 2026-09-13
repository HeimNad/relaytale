package provider

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"relaytale/internal/encryption"
)

var ErrConflict = errors.New("provider revision conflict")
var ErrInvalid = errors.New("invalid provider request")

// Settings intentionally has no credential fields. Password writes are separate.
type Settings struct {
	Name           string   `json:"name"`
	Host           string   `json:"host"`
	Port           int      `json:"port"`
	Security       string   `json:"security"`
	Username       string   `json:"username"`
	Priority       int      `json:"priority"`
	MaxConnections int      `json:"max_connections"`
	TimeoutSeconds int      `json:"timeout_seconds"`
	HourlyLimit    int      `json:"hourly_limit"`
	DailyLimit     int      `json:"daily_limit"`
	FromDomains    []string `json:"from_domains"`
	Enabled        bool     `json:"enabled"`
}

func (s Settings) provider() Provider {
	return Provider{Name: s.Name, Host: s.Host, Port: s.Port, Security: s.Security, Username: s.Username, Priority: s.Priority, MaxConnections: s.MaxConnections, Timeout: time.Duration(s.TimeoutSeconds) * time.Second, HourlyLimit: s.HourlyLimit, DailyLimit: s.DailyLimit}
}

// Manage serializes provider changes without locking messages (worker order is
// message -> provider). A committed update applies to future claims only.
func Manage(ctx context.Context, db *sql.DB, box *encryption.Box, action, id string, expected int64, s Settings, password, actor, reason string) (int64, error) {
	if parsed, err := uuid.Parse(id); err != nil || parsed == uuid.Nil || !strings.EqualFold(parsed.String(), id) {
		return 0, ErrInvalid
	}
	if strings.TrimSpace(actor) == "" || len(actor) > 128 || strings.TrimSpace(reason) == "" || len(reason) > 2048 || !utf8.ValidString(actor+reason) || strings.ContainsRune(actor+reason, 0) {
		return 0, ErrInvalid
	}
	if action != "create" && action != "configure" && action != "credentials" {
		return 0, ErrInvalid
	}
	if action != "create" && expected < 1 {
		return 0, ErrInvalid
	}
	if action == "create" || action == "configure" {
		if len(s.Name) > 128 || len(s.Host) > 253 || len(s.Username) > 320 || len(s.FromDomains) > 100 || s.Priority < -2147483648 || s.Priority > 2147483647 || s.TimeoutSeconds < 1 || s.TimeoutSeconds > 300 {
			return 0, ErrInvalid
		}
		for _, d := range s.FromDomains {
			if len(d) > 253 {
				return 0, ErrInvalid
			}
		}
		if err := Validate(s.provider(), "validation-only", s.FromDomains); err != nil {
			return 0, ErrInvalid
		}
	}
	var cipher, nonce []byte
	if action == "create" || action == "credentials" {
		if box == nil || password == "" || len(password) > 4096 || !utf8.ValidString(password) || strings.ContainsAny(password, "\r\n\x00") {
			return 0, ErrInvalid
		}
		var err error
		cipher, nonce, err = box.Seal(id, password)
		if err != nil {
			return 0, err
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return 0, err
	}
	var revision int64
	if action == "create" {
		err = tx.QueryRowContext(ctx, `INSERT INTO providers(id,name,enabled,host,port,security,username,password_ciphertext,nonce,priority,max_connections,timeout_seconds,from_domains,hourly_limit,daily_limit) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,NULLIF($14,0),NULLIF($15,0)) RETURNING revision`, id, s.Name, s.Enabled, s.Host, s.Port, s.Security, s.Username, cipher, nonce, s.Priority, s.MaxConnections, s.TimeoutSeconds, s.FromDomains, s.HourlyLimit, s.DailyLimit).Scan(&revision)
	} else {
		if err = tx.QueryRowContext(ctx, `SELECT revision FROM providers WHERE id=$1 FOR UPDATE`, id).Scan(&revision); err != nil {
			return 0, err
		}
		if revision != expected {
			return 0, ErrConflict
		}
		if action == "configure" {
			err = tx.QueryRowContext(ctx, `UPDATE providers SET name=$2,enabled=$3,host=$4,port=$5,security=$6,username=$7,priority=$8,max_connections=$9,timeout_seconds=$10,from_domains=$11,hourly_limit=NULLIF($12,0),daily_limit=NULLIF($13,0) WHERE id=$1 RETURNING revision`, id, s.Name, s.Enabled, s.Host, s.Port, s.Security, s.Username, s.Priority, s.MaxConnections, s.TimeoutSeconds, s.FromDomains, s.HourlyLimit, s.DailyLimit).Scan(&revision)
		} else {
			err = tx.QueryRowContext(ctx, `UPDATE providers SET password_ciphertext=$2,nonce=$3 WHERE id=$1 RETURNING revision`, id, cipher, nonce).Scan(&revision)
		}
	}
	if err != nil {
		return 0, err
	}
	// Never include password, ciphertext or nonce in audit data.
	data := map[string]any{"provider_id": id, "revision": revision, "expected_revision": expected, "reason": reason}
	if action != "credentials" {
		data["settings"] = s
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO maintenance_audit(id,action,actor,data) VALUES($1,$2,$3,$4)`, uuid.NewString(), "PROVIDER_"+strings.ToUpper(action), actor, string(raw)); err != nil {
		return 0, err
	}
	return revision, tx.Commit()
}
