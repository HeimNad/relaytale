package operations

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

type RelationCapacity struct {
	Name       string `json:"name"`
	TableBytes int64  `json:"table_bytes"`
	IndexBytes int64  `json:"index_bytes"`
	TotalBytes int64  `json:"total_bytes"`
}
type ArchiveCapacity struct {
	MessageID     string `json:"message_id"`
	ArchiveState  string `json:"archive_state"`
	RecordedBytes int64  `json:"recorded_bytes"`
	ObservedBytes *int64 `json:"observed_bytes,omitempty"`
	Observation   string `json:"observation"`
}
type CapacityReport struct {
	SchemaVersion      int                `json:"schema_version"`
	SnapshotAt         time.Time          `json:"snapshot_at"`
	FinishedAt         time.Time          `json:"finished_at"`
	Consistency        string             `json:"consistency"`
	DeletionAuthorized bool               `json:"deletion_authorized"`
	HasMore            bool               `json:"has_more"`
	NextAfter          string             `json:"next_after"`
	Items              []ArchiveCapacity  `json:"items"`
	Relations          []RelationCapacity `json:"relations"`
}

// Capacity observes a bounded message cohort. SQL visibility, relation sizes and
// filesystem metadata are different observations, never an atomic disk snapshot.
func Capacity(ctx context.Context, db *sql.DB, root, after string, limit int) (out CapacityReport, err error) {
	// Errors discard all observations and cursors rather than exposing partial totals.
	defer func() {
		if err != nil {
			out = CapacityReport{}
		}
	}()
	if limit < 1 || limit > 1000 || root == "" {
		return out, errors.New("invalid capacity bounds")
	}
	if after == "" {
		after = uuid.Nil.String()
	}
	if _, err = uuid.Parse(after); err != nil {
		return out, errors.New("invalid capacity cursor")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return out, err
	}
	dir, err := os.OpenRoot(absolute)
	if err != nil {
		return out, errors.New("archive root unavailable")
	}
	defer dir.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	out = CapacityReport{SchemaVersion: 1, Consistency: "LIVE_OBSERVATIONS_NOT_ATOMIC", Items: []ArchiveCapacity{}, Relations: []RelationCapacity{}}
	if err = tx.QueryRowContext(ctx, "SELECT transaction_timestamp()").Scan(&out.SnapshotAt); err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,eml_path,archive_state,eml_size FROM messages WHERE id>$1 ORDER BY id LIMIT $2`, after, limit+1)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		if len(out.Items) == limit {
			out.HasMore = true
			break
		}
		var item ArchiveCapacity
		var path string
		if err = rows.Scan(&item.MessageID, &path, &item.ArchiveState, &item.RecordedBytes); err != nil {
			rows.Close()
			return out, err
		}
		if err = ctx.Err(); err != nil {
			rows.Close()
			return out, err
		}
		item.Observation, item.ObservedBytes = capacityFile(dir, absolute, path, item.RecordedBytes)
		out.Items = append(out.Items, item)
		out.NextAfter = item.MessageID
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return out, err
	}
	if closeErr != nil {
		return out, closeErr
	}
	// Fixed allowlist: no user-provided SQL relation names or unbounded catalogs.
	for _, name := range []string{"messages", "recipients", "events", "delivery_attempts", "api_submission_ledger", "webhook_outbox", "feedback_reports", "maintenance_audit"} {
		var relation RelationCapacity
		relation.Name = name
		err = tx.QueryRowContext(ctx, `SELECT pg_table_size(to_regclass($1)),pg_indexes_size(to_regclass($1)),pg_total_relation_size(to_regclass($1))`, name).Scan(&relation.TableBytes, &relation.IndexBytes, &relation.TotalBytes)
		if err != nil {
			return out, err
		}
		out.Relations = append(out.Relations, relation)
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	out.FinishedAt = time.Now().UTC()
	return out, nil
}
func capacityFile(root *os.Root, absolute, path string, want int64) (string, *int64) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "INVALID_PATH", nil
	}
	relative, err := filepath.Rel(absolute, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "OUTSIDE_ROOT", nil
	}
	// Reject symlinks at every component rather than deliberately following them.
	var prefix string
	parts := strings.Split(relative, string(filepath.Separator))
	for i, p := range parts {
		prefix = filepath.Join(prefix, p)
		info, e := root.Lstat(prefix)
		if e != nil {
			if errors.Is(e, os.ErrNotExist) {
				return "MISSING", nil
			}
			return "UNREADABLE", nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "SYMLINK_REVIEW", nil
		}
		if i < len(parts)-1 {
			if !info.IsDir() {
				return "NON_DIRECTORY_REVIEW", nil
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return "NON_REGULAR_REVIEW", nil
		}
		second, e := root.Lstat(prefix)
		if e != nil || !os.SameFile(info, second) || info.Size() != second.Size() || !info.ModTime().Equal(second.ModTime()) {
			return "CHANGED_DURING_OBSERVATION", nil
		}
		size := second.Size()
		if size != want {
			return "SIZE_MISMATCH_REVIEW", &size
		}
		return "REGULAR_SIZE_MATCH", &size
	}
	return "INVALID_PATH", nil
}
