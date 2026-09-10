package operations

import (
	"context"
	"database/sql"
	"log/slog"
	"time"
)

// Run is opt-in. Every run is bounded and uses the same audited cleanup path.
func Run(ctx context.Context, db *sql.DB, root string, interval time.Duration, p Retention, log *slog.Logger) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		run, cancel := context.WithTimeout(ctx, time.Minute)
		p.Apply = true
		p.Actor = "scheduler"
		p.Now = time.Time{}
		report, err := Cleanup(run, db, root, p)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Error("maintenance cleanup failed", "component", "maintenance", "failed_archives", len(report.Failures))
		} else {
			log.Info("maintenance cleanup finished", "component", "maintenance", "purged_archives", report.Purged, "debug_logs_cleared", report.DebugCleared)
		}
	}
}
