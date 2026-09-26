package operations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

const archiveRecoveryMax = 100 << 20

type ArchiveObservation struct {
	ID        string `json:"id"`
	ArchiveID string `json:"archive_id"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
}

// recoveryFile checks every component and hashes only a bounded regular file.
// The archive tree is owned by the service; external writers must be stopped.
func recoveryFile(ctx context.Context, root, path string) (ArchiveObservation, error) {
	var out ArchiveObservation
	parts := strings.Split(filepath.ToSlash(path), "/")
	if len(parts) != 4 {
		return out, errors.New("invalid archive path")
	}
	day, err := time.Parse("2006/01/02", strings.Join(parts[:3], "/"))
	if err != nil || day.Format("2006/01/02") != strings.Join(parts[:3], "/") {
		return out, errors.New("invalid archive day")
	}
	id := strings.TrimSuffix(parts[3], ".eml")
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.String() != id || parts[3] != id+".eml" {
		return out, errors.New("invalid archive name")
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return out, err
	}
	defer dir.Close()
	var prefix string
	for i, part := range parts {
		prefix = filepath.Join(prefix, part)
		st, err := dir.Lstat(prefix)
		if err != nil {
			return out, err
		}
		if st.Mode()&os.ModeSymlink != 0 || (i < 3 && !st.IsDir()) || (i == 3 && !st.Mode().IsRegular()) {
			return out, errors.New("unsafe archive entry")
		}
	}
	f, err := dir.Open(path)
	if err != nil {
		return out, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return out, err
	}
	if !st.Mode().IsRegular() || st.Size() > archiveRecoveryMax {
		return out, errors.New("archive exceeds recovery bounds")
	}
	h := sha256.New()
	buf := make([]byte, 32<<10)
	var size int64
	for {
		if err = ctx.Err(); err != nil {
			return out, err
		}
		n, e := f.Read(buf)
		size += int64(n)
		if size > archiveRecoveryMax {
			return out, errors.New("archive grew beyond recovery bounds")
		}
		h.Write(buf[:n])
		if e == io.EOF {
			break
		}
		if e != nil {
			return out, e
		}
	}
	current, err := dir.Lstat(path)
	if err != nil || !os.SameFile(st, current) || current.Size() != size || current.ModTime() != st.ModTime() {
		return out, errors.New("archive changed during inspection")
	}
	return ArchiveObservation{ArchiveID: id, Path: filepath.ToSlash(path), Size: size, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// ObserveArchive records immutable evidence only; it does not authorize removal.
func ObserveArchive(ctx context.Context, db *sql.DB, root, path string) (ArchiveObservation, error) {
	var out ArchiveObservation
	absolute, err := filepath.Abs(root)
	if err != nil {
		return out, err
	}
	out, err = recoveryFile(ctx, absolute, path)
	if err != nil {
		return out, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return out, err
	}
	// Serialize capacity accounting and repeated observations across scanners.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(78320421)`); err != nil {
		return out, err
	}
	var referenced bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE id=$1 OR eml_path=$2 OR eml_path=$3)`, out.ArchiveID, filepath.Join(absolute, path), filepath.Join(root, path)).Scan(&referenced); err != nil {
		return out, err
	}
	if referenced {
		return out, errors.New("archive has a visible reference")
	}
	err = tx.QueryRowContext(ctx, `SELECT id FROM archive_observations WHERE root=$1 AND path=$2 AND size=$3 AND sha256=$4`, absolute, out.Path, out.Size, out.SHA256).Scan(&out.ID)
	if errors.Is(err, sql.ErrNoRows) {
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM archive_observations`).Scan(&count); err != nil {
			return out, err
		}
		if count >= 100000 {
			return out, errors.New("archive observation capacity reached")
		}
		newID, e := uuid.NewV7()
		if e != nil {
			return out, e
		}
		out.ID = newID.String()
		_, err = tx.ExecContext(ctx, `INSERT INTO archive_observations(id,archive_id,root,path,size,sha256) VALUES($1,$2,$3,$4,$5,$6)`, out.ID, out.ArchiveID, absolute, out.Path, out.Size, out.SHA256)
	}
	if err != nil {
		return out, err
	}
	err = tx.Commit()
	return out, err
}

type ArchiveReclaimResult struct {
	ObservationID string `json:"observation_id"`
	Apply         bool   `json:"apply"`
	Retired       bool   `json:"retired"`
	Removed       bool   `json:"removed"`
}

// ReclaimArchive requires an observation at least 24h old. A durable retirement
// fence is committed before unlink. All normal ingress generates a fresh UUID
// and the DB trigger prevents a delayed writer ever enqueuing a retired UUID.
func ReclaimArchive(ctx context.Context, db *sql.DB, root, observation, actor, reason string, apply bool) (result ArchiveReclaimResult, err error) {
	result.ObservationID = observation
	result.Apply = apply
	if _, err = uuid.Parse(observation); err != nil {
		return result, err
	}
	if apply && (len(strings.TrimSpace(actor)) < 1 || len(actor) > 128 || len(strings.TrimSpace(reason)) < 1 || len(reason) > 1000) {
		return result, errors.New("actor and reason required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return result, err
	}
	var o ArchiveObservation
	var old bool
	err = db.QueryRowContext(ctx, `SELECT archive_id,path,size,sha256,observed_at<=clock_timestamp()-interval '24 hours' FROM archive_observations WHERE id=$1 AND root=$2`, observation, absolute).Scan(&o.ArchiveID, &o.Path, &o.Size, &o.SHA256, &old)
	if err != nil {
		return result, err
	}
	if !old {
		return result, errors.New("observation must age at least 24 hours")
	}
	var existing string
	err = db.QueryRowContext(ctx, `SELECT observation_id FROM archive_retirements WHERE archive_id=$1`, o.ArchiveID).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if existing != "" && existing != observation {
		return result, errors.New("different retirement evidence exists")
	}
	result.Retired = existing != ""
	current, inspectErr := recoveryFile(ctx, absolute, o.Path)
	missing := errors.Is(inspectErr, os.ErrNotExist)
	if inspectErr != nil && !(missing && result.Retired) {
		return result, inspectErr
	}
	if inspectErr == nil && (current.Size != o.Size || current.SHA256 != o.SHA256) {
		return result, errors.New("archive differs from observation")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout='2s'`); err != nil {
		return result, err
	}
	// Wait for prior message writes and fence new ones until retirement is durable.
	if _, err = tx.ExecContext(ctx, `LOCK TABLE messages IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return result, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,eml_path FROM messages`)
	if err != nil {
		return result, err
	}
	referenced := false
	target := filepath.Join(absolute, o.Path)
	targetInfo, statErr := os.Stat(target)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		rows.Close()
		return result, statErr
	}

	for rows.Next() {
		var id, path string
		if err = rows.Scan(&id, &path); err != nil {
			rows.Close()
			return result, err
		}
		normalized, e := filepath.Abs(path)
		if e != nil {
			rows.Close()
			return result, e
		}
		sameFile := false
		if targetInfo != nil {
			other, statErr := os.Stat(normalized)
			if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
				rows.Close()
				return result, statErr
			}
			sameFile = statErr == nil && os.SameFile(targetInfo, other)
		}
		if id == o.ArchiveID || normalized == target || sameFile {
			referenced = true
			break
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	rows.Close()
	if referenced {
		return result, errors.New("archive is referenced; removal refused")
	}
	if !apply {
		return result, nil
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO archive_retirements(archive_id,observation_id,actor,reason) VALUES($1,$2,$3,$4) ON CONFLICT(archive_id) DO NOTHING`, o.ArchiveID, observation, actor, reason); err != nil {
		return result, err
	}
	var chosen string
	if err = tx.QueryRowContext(ctx, `SELECT observation_id FROM archive_retirements WHERE archive_id=$1`, o.ArchiveID).Scan(&chosen); err != nil {
		return result, err
	}
	if chosen != observation {
		return result, errors.New("concurrent retirement conflict")
	}
	// A failed/ambiguous COMMIT must never be followed by deletion in this call.
	if err = tx.Commit(); err != nil {
		return result, err
	}
	result.Retired = true
	if inspectErr == nil {
		current, err = recoveryFile(ctx, absolute, o.Path)
		if err != nil {
			return result, err
		}
		if current.Size != o.Size || current.SHA256 != o.SHA256 {
			return result, errors.New("archive changed after retirement")
		}
		dir, e := os.OpenRoot(absolute)
		if e != nil {
			return result, e
		}
		defer dir.Close()
		if err = dir.Remove(o.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
	}
	// Sync the directory even on retries after an unlink whose result was lost.
	dir, e := os.OpenRoot(absolute)
	if e != nil {
		return result, e
	}
	defer dir.Close()
	parent, e := dir.Open(filepath.Dir(o.Path))
	if e != nil {
		return result, e
	}
	err = parent.Sync()
	parent.Close()
	if err != nil {
		return result, err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO archive_reclaims(archive_id) VALUES($1) ON CONFLICT DO NOTHING`, o.ArchiveID)
	if err != nil {
		return result, err
	}
	result.Removed = true
	return result, nil
}
