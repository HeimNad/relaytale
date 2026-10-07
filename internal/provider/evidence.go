package provider

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Evidence is an operator-supplied claim. It never grants delivery permission.
type Evidence struct {
	Topic      string    `json:"topic"`
	Value      string    `json:"value"`
	Source     string    `json:"source"`
	SourceURL  string    `json:"source_url"`
	ObservedAt time.Time `json:"observed_at"`
}

func (e Evidence) Validate(now time.Time) error {
	if e.ObservedAt.IsZero() || e.ObservedAt.Year() < 1 || e.ObservedAt.Year() > 9999 || e.ObservedAt.After(now.Add(5*time.Minute)) {
		return ErrInvalid
	}
	switch e.Topic {
	case "max_message_bytes", "max_recipients":
		n, err := strconv.ParseInt(e.Value, 10, 64)
		max := int64(1 << 30)
		if e.Topic == "max_recipients" {
			max = 100000
		}
		if err != nil || n < 1 || n > max || strconv.FormatInt(n, 10) != e.Value {
			return ErrInvalid
		}
	case "smtputf8", "8bitmime", "provider_dkim_signing":
		if e.Value != "supported" && e.Value != "unsupported" && e.Value != "unknown" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	if e.Source != "operator_report" && e.Source != "provider_documentation" {
		return ErrInvalid
	}
	if e.Source == "provider_documentation" && e.SourceURL == "" {
		return ErrInvalid
	}
	if e.SourceURL != "" {
		u, err := url.Parse(e.SourceURL)
		if err != nil || len(e.SourceURL) > 2048 || !utf8.ValidString(e.SourceURL) || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(e.SourceURL, "\r\n\x00") {
			return ErrInvalid
		}
	}
	return nil
}
func RecordEvidence(ctx context.Context, db *sql.DB, id string, expected int64, e Evidence, actor, reason string) error {
	if !validQuotaID(id) || expected < 1 || !validQuotaAudit(actor, reason) || e.Validate(time.Now()) != nil {
		return ErrInvalid
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return err
	}
	var revision int64
	if err = tx.QueryRowContext(ctx, `SELECT revision FROM providers WHERE id=$1 FOR NO KEY UPDATE`, id).Scan(&revision); err != nil {
		return err
	}
	if revision != expected {
		return ErrConflict
	}
	if err = quotaAudit(ctx, tx, "PROVIDER_CAPABILITY_EVIDENCE", actor, map[string]any{"provider_id": id, "provider_revision": revision, "evidence": e, "reason": reason, "trust": "operator_claim", "affects_delivery": false}); err != nil {
		return err
	}
	return tx.Commit()
}

// EvidenceHistory is bounded; newest records come first and older history remains
// in the append-only maintenance export. URLs are references, never fetched.
func EvidenceHistory(ctx context.Context, db *sql.DB, id string, limit int) ([]json.RawMessage, error) {
	if !validQuotaID(id) || limit < 1 || limit > 1000 {
		return nil, ErrInvalid
	}
	rows, err := db.QueryContext(ctx, `SELECT jsonb_build_object('id',a.id,'recorded_at',a.created_at,'actor',a.actor,'data',a.data,'current_provider_revision',p.revision,'configuration_changed',a.data->>'provider_revision'<>p.revision::text,'older_than_30_days',(a.data->'evidence'->>'observed_at')::timestamptz<clock_timestamp()-interval '30 days') FROM maintenance_audit a JOIN providers p ON p.id=$1 WHERE a.action='PROVIDER_CAPABILITY_EVIDENCE' AND a.data->>'provider_id'=p.id::text ORDER BY a.created_at DESC,a.id DESC LIMIT $2`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []json.RawMessage{}
	for rows.Next() {
		var raw json.RawMessage
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, rows.Err()
}
