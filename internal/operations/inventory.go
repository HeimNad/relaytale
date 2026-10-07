package operations

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

type ArchiveInventoryItem struct {
	Path          string `json:"path"`
	Reason        string `json:"reason"`
	ObservedBytes *int64 `json:"observed_bytes,omitempty"`
}
type ArchiveInventoryReport struct {
	Complete  bool                   `json:"complete"`
	Truncated bool                   `json:"truncated"`
	NextAfter string                 `json:"next_after"`
	Scanned   int                    `json:"scanned"`
	Items     []ArchiveInventoryItem `json:"items"`
}

// InventoryArchives is a bounded, read-only view of one UTC archive day.
// A missing visible reference is never permission to delete: a writer may still
// be publishing or committing, and filesystem and SQL snapshots are not atomic.
func InventoryArchives(ctx context.Context, db *sql.DB, root, day string, limit int) (report ArchiveInventoryReport, err error) {
	return InventoryArchivesPage(ctx, db, root, day, "", limit)
}

// InventoryArchivesPage resumes strictly after a basename in lexical order.
// Cursors are only valid for the same root/day; concurrent changes are not a snapshot.
func InventoryArchivesPage(ctx context.Context, db *sql.DB, root, day, cursor string, limit int) (report ArchiveInventoryReport, err error) {
	report.Items = []ArchiveInventoryItem{}
	if len(cursor) > 340 {
		return report, errors.New("invalid inventory cursor")
	}
	decoded, e := base64.RawURLEncoding.DecodeString(cursor)
	if e != nil {
		return report, errors.New("invalid inventory cursor")
	}
	after := string(decoded)
	date, e := time.Parse("2006-01-02", day)
	if e != nil || date.Format("2006-01-02") != day || limit < 1 || limit > 10000 || root == "" || len(after) > 255 || strings.ContainsAny(after, "/\x00") || after == "." || after == ".." {
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
	names, e := inventoryPageNames(ctx, folder, after, limit+1)
	if e != nil {
		return report, e
	}
	if len(names) > limit {
		report.Truncated = true
		names = names[:limit]
	}
	tx, e := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		return report, e
	}
	defer tx.Rollback()
	for _, name := range names {
		if e = ctx.Err(); e != nil {
			return report, e
		}
		path := filepath.Join(relative, name)
		item := ArchiveInventoryItem{Path: filepath.ToSlash(path)}
		info, e := dir.Lstat(path)
		if e != nil {
			item.Reason = "ENTRY_UNREADABLE"
		} else if !info.Mode().IsRegular() {
			item.Reason = "NON_REGULAR_ENTRY"
		} else {
			id := strings.TrimSuffix(name, ".eml")
			parsed, e := uuid.Parse(id)
			if e != nil || parsed.String() != id || name != id+".eml" {
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

		if e == nil && info.Mode().IsRegular() {
			second, checkErr := dir.Lstat(path)
			if checkErr != nil || !os.SameFile(info, second) || info.Size() != second.Size() || !info.ModTime().Equal(second.ModTime()) {
				item.Reason = "CHANGED_DURING_OBSERVATION"
			} else {
				size := second.Size()
				item.ObservedBytes = &size
			}
		}
		report.Scanned++
		report.Items = append(report.Items, item)
	}
	if e = tx.Commit(); e != nil {
		return report, e
	}
	if len(names) > 0 {
		report.NextAfter = base64.RawURLEncoding.EncodeToString([]byte(names[len(names)-1]))
	}
	report.Complete = !report.Truncated
	return report, nil
}
