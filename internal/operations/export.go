package operations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"
)

type Filter struct {
	MessageID    string
	Since, Until time.Time
}
type ExportReport struct {
	Version    int              `json:"version"`
	SnapshotAt time.Time        `json:"snapshot_at"`
	Records    map[string]int64 `json:"records"`
}

func Export(ctx context.Context, db *sql.DB, w io.Writer, f Filter) (ExportReport, error) {
	report := ExportReport{Version: 1, SnapshotAt: time.Now().UTC(), Records: map[string]int64{}}
	if f.Until.IsZero() {
		f.Until = report.SnapshotAt
	}
	if f.Since.IsZero() {
		f.Since = time.Unix(0, 0)
	}
	if !f.Since.Before(f.Until) {
		return report, errors.New("since must precede until")
	}
	var id any
	if f.MessageID != "" {
		parsed, err := uuid.Parse(f.MessageID)
		if err != nil {
			return report, errors.New("invalid message ID")
		}
		id = parsed.String()
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL timezone='UTC'`); err != nil {
		return report, err
	}
	// Acquire the snapshot before emitting its metadata.
	if err := tx.QueryRowContext(ctx, `SELECT transaction_timestamp()`).Scan(&report.SnapshotAt); err != nil {
		return report, err
	}
	enc := json.NewEncoder(w)
	if err := enc.Encode(map[string]any{"type": "manifest", "schema_version": 1, "snapshot_at": report.SnapshotAt, "since": f.Since.UTC(), "until": f.Until.UTC(), "message_id": f.MessageID, "contains_eml": false, "contains_credentials": false}); err != nil {
		return report, err
	}
	predicate := `($1::uuid IS NULL OR m.id=$1) AND m.created_at >= $2 AND m.created_at < $3`
	queries := []struct{ kind, query string }{
		{"message", `SELECT jsonb_build_object('id',m.id,'message_id',m.message_id,'source_type',m.source_type,'envelope_from',m.envelope_from,'header_from',m.header_from,'subject',m.subject,'created_at',m.created_at,'queued_at',m.queued_at,'completed_at',m.completed_at,'status',m.status,'eml_size',m.eml_size,'eml_sha256',m.eml_sha256,'archive_state',m.archive_state,'eml_purged_at',m.eml_purged_at,'last_error',m.last_error) FROM messages m WHERE ` + predicate + ` ORDER BY m.created_at,m.id`},
		{"recipient", `SELECT to_jsonb(r) FROM recipients r JOIN messages m ON m.id=r.message_id WHERE ` + predicate + ` ORDER BY m.created_at,m.id,r.id`},
		{"attempt", `SELECT to_jsonb(a)-'raw_debug_log'-'claim_token' FROM delivery_attempts a JOIN messages m ON m.id=a.message_id WHERE ` + predicate + ` ORDER BY m.created_at,m.id,a.attempt_number`},
		{"attempt_recipient", `SELECT to_jsonb(ar) FROM attempt_recipients ar JOIN delivery_attempts a ON a.id=ar.attempt_id JOIN messages m ON m.id=a.message_id WHERE ` + predicate + ` ORDER BY m.created_at,m.id,a.attempt_number,ar.recipient_id`},
		{"event", `SELECT to_jsonb(e) FROM events e JOIN messages m ON m.id=e.message_id WHERE ` + predicate + ` ORDER BY e.event_time,e.id`},
	}
	for _, q := range queries {
		rows, err := tx.QueryContext(ctx, q.query, id, f.Since, f.Until)
		if err != nil {
			return report, err
		}
		for rows.Next() {
			var data json.RawMessage
			if err := rows.Scan(&data); err != nil {
				rows.Close()
				return report, err
			}
			if err := enc.Encode(map[string]any{"type": q.kind, "data": data}); err != nil {
				rows.Close()
				return report, err
			}
			report.Records[q.kind]++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return report, err
		}
		rows.Close()
	}
	if err := enc.Encode(map[string]any{"type": "summary", "records": report.Records}); err != nil {
		return report, err
	}
	return report, tx.Commit()
}
func Audit(ctx context.Context, db *sql.DB, action, actor string, data any) error {
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO maintenance_audit(id,action,actor,data) VALUES($1,$2,$3,$4)`, id.String(), action, actor, string(raw))
	return err
}
