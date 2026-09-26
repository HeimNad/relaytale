package operations

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

type ArchiveInventoryItem struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}
type ArchiveInventoryReport struct {
	Complete  bool                   `json:"complete"`
	Truncated bool                   `json:"truncated"`
	Scanned   int                    `json:"scanned"`
	Items     []ArchiveInventoryItem `json:"items"`
}

// InventoryArchives is a bounded, read-only view of one UTC archive day.
// A missing visible reference is never permission to delete: a writer may still
// be publishing or committing, and filesystem and SQL snapshots are not atomic.
func InventoryArchives(ctx context.Context, db *sql.DB, root, day string, limit int) (report ArchiveInventoryReport, err error) {
	report.Items = []ArchiveInventoryItem{}
	date, e := time.Parse("2006-01-02", day)
	if e != nil || date.Format("2006-01-02") != day || limit < 1 || limit > 10000 || root == "" {
		return report, errors.New("invalid inventory bounds")
	}
	rootAbs, e := filepath.Abs(root)
	if e != nil {
		return report, e
	}
	dir, e := os.OpenRoot(rootAbs)
	if e != nil {
		return report, e
	}
	defer dir.Close()
	relative := date.Format("2006/01/02")
	// Do not deliberately traverse a symlink in the selected date hierarchy.
	var prefix string
	for _, part := range strings.Split(relative, "/") {
		prefix = filepath.Join(prefix, part)
		info, e := dir.Lstat(prefix)
		if e != nil {
			return report, e
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return report, errors.New("archive date is not a real directory")
		}
	}
	folder, e := dir.Open(relative)
	if e != nil {
		return report, e
	}
	defer folder.Close()
	// ReadDir(n) bounds memory even in an unexpectedly huge archive directory.
	entries, e := folder.ReadDir(limit + 1)
	if e != nil && !errors.Is(e, io.EOF) {
		return report, e
	}
	if len(entries) > limit {
		report.Truncated = true
		entries = entries[:limit]
	}
	tx, e := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		return report, e
	}
	defer tx.Rollback()
	for _, entry := range entries {
		if e = ctx.Err(); e != nil {
			return report, e
		}
		path := filepath.Join(relative, entry.Name())
		item := ArchiveInventoryItem{Path: filepath.ToSlash(path)}
		info, e := dir.Lstat(path)
		if e != nil {
			item.Reason = "ENTRY_UNREADABLE"
		} else if !info.Mode().IsRegular() {
			item.Reason = "NON_REGULAR_ENTRY"
		} else {
			id := strings.TrimSuffix(entry.Name(), ".eml")
			parsed, e := uuid.Parse(id)
			if e != nil || parsed.String() != id || entry.Name() != id+".eml" {
				item.Reason = "UNRECOGNIZED_NAME"
			} else {
				var referenced bool
				e = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE id=$1 OR eml_path=$2 OR eml_path=$3)`, id, filepath.Join(rootAbs, path), filepath.Join(root, path)).Scan(&referenced)
				if e != nil {
					return report, e
				}
				if referenced {
					item.Reason = "REFERENCE_PRESENT"
				} else {
					item.Reason = "NO_VISIBLE_REFERENCE_REVIEW"
				}
			}
		}
		report.Scanned++
		report.Items = append(report.Items, item)
	}
	if e = tx.Commit(); e != nil {
		return report, e
	}
	report.Complete = !report.Truncated
	return report, nil
}
