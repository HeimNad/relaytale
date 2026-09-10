package operations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

type Retention struct {
	EMLDays, DebugDays, Batch int
	Apply                     bool
	Actor                     string
	Now                       time.Time
}
type CleanupItem struct {
	ID    string `json:"message_id"`
	State string `json:"archive_state"`
	path  string
}
type CleanupReport struct {
	DryRun          bool          `json:"dry_run"`
	Archives        []CleanupItem `json:"archives"`
	DebugCandidates int           `json:"debug_candidates"`
	Purged          int           `json:"purged_archives"`
	DebugCleared    int           `json:"debug_logs_cleared"`
	Failures        []string      `json:"failed_message_ids,omitempty"`
}

const terminal = `m.status IN ('SMTP_ACCEPTED','PERM_FAILED','DELIVERED','BOUNCED','SUPPRESSED','CANCELLED') AND m.locked_by IS NULL
 AND NOT EXISTS(SELECT 1 FROM delivery_attempts a WHERE a.message_id=m.id AND a.result='IN_PROGRESS')
 AND NOT EXISTS(SELECT 1 FROM recipients r WHERE r.message_id=m.id AND r.status IN ('QUEUED','SENDING','TEMP_FAILED','DELIVERY_UNKNOWN'))`

func validateRetention(p *Retention) error {
	if p.Now.IsZero() {
		p.Now = time.Now().UTC()
	}
	if p.Actor == "" {
		p.Actor = "local_cli"
	}
	if p.EMLDays < 0 || p.DebugDays < 0 || p.EMLDays > 36500 || p.DebugDays > 36500 || p.Batch < 1 || p.Batch > 1000 {
		return errors.New("retention days must be 0..36500 and batch 1..1000; 0 days disables that cleanup")
	}
	return nil
}
func Cleanup(ctx context.Context, db *sql.DB, root string, p Retention) (CleanupReport, error) {
	report := CleanupReport{DryRun: !p.Apply, Archives: []CleanupItem{}}
	if err := validateRetention(&p); err != nil {
		return report, err
	}
	// One cleanup run at a time. This does not lock delivery workers or exporters.
	conn, err := db.Conn(ctx)
	if err != nil {
		return report, err
	}
	defer conn.Close()
	var acquired bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(78320419)`).Scan(&acquired); err != nil {
		return report, err
	}
	if !acquired {
		return report, errors.New("another cleanup is running")
	}
	defer func() {
		release, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(release, `SELECT pg_advisory_unlock(78320419)`)
	}()
	if p.EMLDays > 0 {
		cutoff := p.Now.AddDate(0, 0, -p.EMLDays)
		rows, err := conn.QueryContext(ctx, `SELECT m.id,m.archive_state,m.eml_path FROM messages m WHERE `+terminal+` AND m.archive_state<>'PURGED' AND (m.completed_at<$1 OR m.archive_state='PURGE_PENDING') ORDER BY m.completed_at,m.id LIMIT $2`, cutoff, p.Batch)
		if err != nil {
			return report, err
		}
		for rows.Next() {
			var item CleanupItem
			if err := rows.Scan(&item.ID, &item.State, &item.path); err != nil {
				rows.Close()
				return report, err
			}
			report.Archives = append(report.Archives, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return report, err
		}
		rows.Close()
	}
	if p.DebugDays > 0 {
		err := conn.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT id FROM delivery_attempts WHERE finished_at<$1 AND result<>'IN_PROGRESS' AND raw_debug_log IS NOT NULL ORDER BY finished_at,id LIMIT $2) d`, p.Now.AddDate(0, 0, -p.DebugDays), p.Batch).Scan(&report.DebugCandidates)
		if err != nil {
			return report, err
		}
	}
	if !p.Apply {
		return report, nil
	}
	if err := Audit(ctx, db, "CLEANUP_STARTED", p.Actor, map[string]any{"eml_days": p.EMLDays, "debug_days": p.DebugDays, "batch": p.Batch}); err != nil {
		return report, err
	}
	for _, item := range report.Archives {
		if err := purge(ctx, conn, root, item, p); err != nil {
			report.Failures = append(report.Failures, item.ID)
			if err := Audit(ctx, db, "ARCHIVE_PURGE_FAILED", p.Actor, map[string]string{"message_id": item.ID}); err != nil {
				return report, err
			}
		} else {
			report.Purged++
		}
	}
	if p.DebugDays > 0 {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return report, err
		}
		defer tx.Rollback()
		res, err := tx.ExecContext(ctx, `UPDATE delivery_attempts SET raw_debug_log=NULL WHERE id IN (SELECT id FROM delivery_attempts WHERE finished_at<$1 AND result<>'IN_PROGRESS' AND raw_debug_log IS NOT NULL ORDER BY finished_at,id LIMIT $2 FOR UPDATE SKIP LOCKED)`, p.Now.AddDate(0, 0, -p.DebugDays), p.Batch)
		if err != nil {
			return report, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return report, err
		}
		report.DebugCleared = int(n)
		if err := auditTx(ctx, tx, "DEBUG_LOGS_PURGED", p.Actor, map[string]any{"count": n, "before": p.Now.AddDate(0, 0, -p.DebugDays)}); err != nil {
			return report, err
		}
		if err := tx.Commit(); err != nil {
			return report, err
		}
	}
	if err := Audit(ctx, db, "CLEANUP_FINISHED", p.Actor, report); err != nil {
		return report, err
	}
	if len(report.Failures) > 0 {
		return report, fmt.Errorf("%d archive purges failed; pending files can be retried", len(report.Failures))
	}
	return report, nil
}
func auditTx(ctx context.Context, tx *sql.Tx, action, actor string, data any) error {
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO maintenance_audit(id,action,actor,data) VALUES($1,$2,$3,$4)`, id.String(), action, actor, string(raw))
	return err
}
func purge(ctx context.Context, conn *sql.Conn, root string, item CleanupItem, p Retention) error {
	// Commit the intent first, then remove the file, then commit the completion.
	// A crash at either boundary resumes from PURGE_PENDING without losing metadata.
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return err
	}
	var path string
	err = tx.QueryRowContext(ctx, `UPDATE messages m SET archive_state='PURGE_PENDING' WHERE m.id=$1 AND `+terminal+` AND m.archive_state<>'PURGED' AND (m.completed_at<$2 OR m.archive_state='PURGE_PENDING') RETURNING eml_path`, item.ID, p.Now.AddDate(0, 0, -p.EMLDays)).Scan(&path)
	if err != nil {
		return err
	}
	if err := auditTx(ctx, tx, "ARCHIVE_PURGE_PENDING", p.Actor, map[string]string{"message_id": item.ID}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil {
		return err
	}
	confined, err := os.OpenRoot(rootAbs)
	if err != nil {
		return err
	}
	defer confined.Close()
	if err := confined.Remove(relative); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir, err := confined.Open(filepath.Dir(relative))
	if err != nil {
		return err
	}
	err = dir.Sync()
	dir.Close()
	if err != nil {
		return err
	}
	tx2, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx2.Rollback()
	_, err = tx2.ExecContext(ctx, `UPDATE messages SET archive_state='PURGED',eml_purged_at=now() WHERE id=$1 AND archive_state='PURGE_PENDING'`, item.ID)
	if err != nil {
		return err
	}
	eventID, err := uuid.NewV7()
	if err != nil {
		return err
	}
	if _, err := tx2.ExecContext(ctx, `INSERT INTO events(id,message_id,event_type,event_time,source,data) VALUES($1,$2,'ARCHIVE_PURGED',now(),'maintenance','{}')`, eventID.String(), item.ID); err != nil {
		return err
	}
	if err := auditTx(ctx, tx2, "ARCHIVE_PURGED", p.Actor, map[string]string{"message_id": item.ID}); err != nil {
		return err
	}
	return tx2.Commit()
}
