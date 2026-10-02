package webhook

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

type Key struct {
	ID     string `json:"id"`
	Secret string `json:"secret"`
}
type Endpoint struct {
	SchemaVersion int    `json:"schema_version,omitempty"`
	ID            string `json:"id"`
	APIKeyID      string `json:"api_key_id"`
	URL           string `json:"url"`
	Keys          []Key  `json:"keys"`
}

var keyID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)

func canonicalID(s string) bool {
	id, e := uuid.Parse(s)
	return e == nil && id != uuid.Nil && id.String() == s
}
func ValidateURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || len(raw) > 2048 || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.Hostname() == "" || (u.Port() != "" && u.Port() != "443") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(raw, "\\\r\n\t ") {
		return errors.New("webhook requires HTTPS port 443 without credentials, query or fragment")
	}
	return nil
}
func Parse(raw string) ([]Endpoint, error) {
	if raw == "" {
		return nil, nil
	}
	bad := errors.New("invalid WEBHOOK_ENDPOINTS")
	if len(raw) > 65536 {
		return nil, bad
	}
	if uniqueJSON(json.NewDecoder(strings.NewReader(raw))) != nil {
		return nil, bad
	}
	var out []Endpoint
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&out) != nil || len(out) > 32 {
		return nil, bad
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, bad
	}
	ids, owners := map[string]bool{}, map[string]bool{}
	for _, v := range out {
		if (v.SchemaVersion != 0 && v.SchemaVersion != 1 && v.SchemaVersion != 2) || !canonicalID(v.ID) || !canonicalID(v.APIKeyID) || ids[v.ID] || owners[v.APIKeyID] || ValidateURL(v.URL) != nil || len(v.Keys) < 1 || len(v.Keys) > 2 {
			return nil, bad
		}
		ids[v.ID] = true
		owners[v.APIKeyID] = true
		kids := map[string]bool{}
		for _, k := range v.Keys {
			b, e := hex.DecodeString(k.Secret)
			if !keyID.MatchString(k.ID) || kids[k.ID] || e != nil || len(b) != 32 || hex.EncodeToString(b) != k.Secret {
				return nil, bad
			}
			kids[k.ID] = true
		}
	}
	return out, nil
}
func (e Endpoint) Version() int {
	if e.SchemaVersion == 2 {
		return 2
	}
	return 1
}

func (e Endpoint) Hash() string {
	b, _ := json.Marshal(e)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Sync is startup-only. An endpoint's destination and owner cannot be reassigned:
// old notifications must never silently travel to a new website.
func Sync(ctx context.Context, db *sql.DB, endpoints []Endpoint) error {
	raw, err := json.Marshal(endpoints)
	if err != nil {
		return errors.New("invalid webhook configuration")
	}
	if _, err = Parse(string(raw)); err != nil {
		return err
	}
	tx, e := begin(ctx, db)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(824014)`); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE webhook_endpoints SET enabled=false`); e != nil {
		return e
	}
	for _, v := range endpoints {
		var enabled bool
		if e = tx.QueryRowContext(ctx, `SELECT enabled AND prefix='rts_' FROM api_keys WHERE id=$1`, v.APIKeyID).Scan(&enabled); e != nil || !enabled {
			return errors.New("webhook requires configured enabled send identity")
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO webhook_endpoints(id,api_key_id,url,config_hash,enabled,schema_version) VALUES($1,$2,$3,$4,true,$5) ON CONFLICT(id) DO UPDATE SET config_hash=EXCLUDED.config_hash,enabled=true,schema_version=EXCLUDED.schema_version WHERE webhook_endpoints.api_key_id=EXCLUDED.api_key_id AND webhook_endpoints.url=EXCLUDED.url`, v.ID, v.APIKeyID, v.URL, v.Hash(), v.Version())
		if err != nil {
			return errors.New("webhook endpoint synchronization failed")
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return errors.New("webhook endpoint destination and owner are immutable")
		}
	}
	identities := make([]map[string]string, 0, len(endpoints))
	for _, v := range endpoints {
		identities = append(identities, map[string]string{"endpoint_id": v.ID, "api_key_id": v.APIKeyID, "config_hash": v.Hash()})
	}
	data, _ := json.Marshal(map[string]any{"endpoints": identities})
	if _, e = tx.ExecContext(ctx, `INSERT INTO maintenance_audit(id,action,actor,data) VALUES($1,'WEBHOOK_CONFIGURED','startup_config',$2)`, uuid.NewString(), string(data)); e != nil {
		return e
	}
	return tx.Commit()
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

func uniqueJSON(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); ok {
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return e
				}
				key, ok := k.(string)
				key = strings.ToLower(key)
				if !ok || seen[key] {
					return errors.New("duplicate JSON key")
				}
				seen[key] = true
				if e = uniqueJSON(d); e != nil {
					return e
				}
			}
		case '[':
			for d.More() {
				if e := uniqueJSON(d); e != nil {
					return e
				}
			}
		default:
			return errors.New("invalid JSON")
		}
		_, err = d.Token()
		return err
	}
	return nil
}
