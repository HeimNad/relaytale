package operations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"relaytale/internal/smtpclient"
)

type PreflightIssue struct {
	MessageID string `json:"message_id"`
	Reason    string `json:"reason"`
}
type PreflightReport struct {
	Complete  bool             `json:"complete"`
	Scanned   int              `json:"scanned"`
	Valid     int              `json:"valid"`
	Issues    []PreflightIssue `json:"issues"`
	NextAfter string           `json:"next_after"`
	HasMore   bool             `json:"has_more"`
}

// Preflight reads pending archives only. It neither migrates the database nor
// repairs MIME, writes audit events, resolves UNKNOWN or schedules delivery.
func Preflight(ctx context.Context, db *sql.DB, root, after string, limit int, maxBytes int64) (report PreflightReport, err error) {
	report.Issues = []PreflightIssue{}
	if limit < 1 || limit > 10000 || maxBytes < 1 || maxBytes > 1<<30 {
		return report, errors.New("invalid preflight bounds")
	}
	if after == "" {
		after = uuid.Nil.String()
	}
	if _, err = uuid.Parse(after); err != nil {
		return report, errors.New("invalid preflight cursor")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return report, errors.New("invalid archive root")
	}
	dir, err := os.OpenRoot(rootAbs)
	if err != nil {
		return report, errors.New("cannot open archive root")
	}
	defer dir.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,eml_path,eml_size,eml_sha256 FROM messages WHERE id>$1 AND archive_state='AVAILABLE' AND status IN ('QUEUED','TEMP_FAILED','SENDING','DELIVERY_UNKNOWN') ORDER BY id LIMIT $2`, after, limit+1)
	if err != nil {
		return report, err
	}
	defer rows.Close()
	for rows.Next() {
		if report.Scanned == limit {
			report.HasMore = true
			break
		}
		var id, path, hash string
		var size int64
		if err = rows.Scan(&id, &path, &size, &hash); err != nil {
			return report, err
		}
		reason := preflightArchive(ctx, dir, rootAbs, path, size, hash, maxBytes)
		if err = ctx.Err(); err != nil {
			return report, err
		}
		report.Scanned++
		report.NextAfter = id
		if reason == "" {
			report.Valid++
		} else {
			report.Issues = append(report.Issues, PreflightIssue{id, reason})
		}
	}
	if err = rows.Err(); err != nil {
		return report, err
	}
	if err = rows.Close(); err != nil {
		return report, err
	}
	err = tx.Commit()
	report.Complete = err == nil
	return report, err
}
func preflightArchive(ctx context.Context, root *os.Root, rootAbs, path string, size int64, wantHash string, maxBytes int64) string {
	if size < 1 || size > maxBytes {
		return "SIZE_LIMIT"
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "ARCHIVE_UNREADABLE"
	}
	relative, err := filepath.Rel(rootAbs, absolute)
	if err != nil {
		return "ARCHIVE_UNREADABLE"
	}
	f, err := root.Open(relative)
	if err != nil {
		return "ARCHIVE_UNREADABLE"
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return "ARCHIVE_UNREADABLE"
	}
	if stat.Size() != size {
		return "SIZE_MISMATCH"
	}
	hash := sha256.New()
	reader := &preflightCounter{Reader: io.TeeReader(io.LimitReader(f, size+1), hash)}
	if err = smtpclient.ValidateCanonical(ctx, reader); err != nil {
		if errors.Is(err, smtpclient.ErrNonCanonical) {
			return "NONCANONICAL_EML"
		}
		return "ARCHIVE_UNREADABLE"
	}
	if reader.n != size {
		return "SIZE_MISMATCH"
	}
	if fmt.Sprintf("%x", hash.Sum(nil)) != wantHash {
		return "HASH_MISMATCH"
	}
	return ""
}

type preflightCounter struct {
	io.Reader
	n int64
}

func (r *preflightCounter) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.n += int64(n)
	return n, err
}
