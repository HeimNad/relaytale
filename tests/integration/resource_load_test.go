package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"relaytale/internal/resource"
	"relaytale/internal/testsmtp"
)

type tableSample struct {
	Table                                                         string `json:"table"`
	Live, Dead, HeapBytes, IndexBytes, Autovacuums, ManualVacuums int64
}

func sampleTables(ctx context.Context, db *sql.DB) ([]tableSample, error) {
	rows, err := db.QueryContext(ctx, `SELECT relname,n_live_tup,n_dead_tup,pg_table_size(relid),pg_indexes_size(relid),autovacuum_count,vacuum_count FROM pg_stat_user_tables WHERE schemaname='public' AND relname IN ('messages','recipients','delivery_attempts','attempt_recipients','events','provider_quota') ORDER BY relname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tableSample
	for rows.Next() {
		var s tableSample
		if err = rows.Scan(&s.Table, &s.Live, &s.Dead, &s.HeapBytes, &s.IndexBytes, &s.Autovacuums, &s.ManualVacuums); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
func rssBytes() uint64 {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) > 1 {
				v, _ := strconv.ParseUint(fields[1], 10, 64)
				return v * 1024
			}
		}
	}
	return 0
}
func snapshotFDs() int {
	files, _ := os.ReadDir("/proc/self/fd")
	n := 0
	for _, f := range files {
		path, _ := os.Readlink("/proc/self/fd/" + f.Name())
		if strings.Contains(path, "relaytale-send-") {
			n++
		}
	}
	return n
}

// Opt-in reproducible local load experiment; never connects to a real provider.
// Normal CI does not spend minutes running this capacity experiment.
func TestResourceLoad(t *testing.T) {
	raw := os.Getenv("RELAYTALE_LOAD_SECONDS")
	if raw == "" {
		t.Skip("set RELAYTALE_LOAD_SECONDS=120 for isolated load experiment")
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 30 || seconds > 300 {
		t.Fatal("load duration must be 30..300 seconds")
	}
	var received atomic.Int64
	f := delivery(t, testsmtp.Options{DiscardPayload: true, OnMessage: func(_ int64, _ string) { received.Add(1) }}, 4)
	f.worker.Log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	f.worker.Resources = resource.New(32<<20, 100<<20)
	f.worker.MaxBytes = 25 << 20
	mustExec(t, f, `UPDATE providers SET timeout_seconds=30 WHERE id=$1`, providerID(t, f))
	before, err := sampleTables(context.Background(), f.db)
	if err != nil {
		t.Fatal(err)
	}
	var walStart string
	if err = f.db.QueryRow(`SELECT pg_current_wal_lsn()::text`).Scan(&walStart); err != nil {
		t.Fatal(err)
	}
	claims, stopClaims := context.WithCancel(context.Background())
	operations, stopOperations := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() {
		stopClaims()
		stopOperations()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("load worker leak")
		}
	}()
	total := 0
	for i := 0; i < 40; i++ {
		if _, err := receiveSized(context.Background(), f, f.worker.Resources, 32<<10, 25<<20); err != nil {
			t.Fatal(err)
		}
		total++
	}
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)
	baseRSS := rssBytes()
	baseGoroutines := runtime.NumGoroutine()
	start := time.Now()
	go func() { defer close(done); f.worker.Run(claims, operations, 4) }()
	var peakHeap, peakRSS atomic.Uint64
	var peakLocks, peakConnections atomic.Int64
	sampling, stopSampling := context.WithCancel(context.Background())
	sampleDone := make(chan struct{})
	defer func() { stopSampling(); <-sampleDone }()
	go func() {
		defer close(sampleDone)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-sampling.Done():
				return
			case <-ticker.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > peakHeap.Load() {
					peakHeap.Store(m.HeapAlloc)
				}
				if rss := rssBytes(); rss > peakRSS.Load() {
					peakRSS.Store(rss)
				}
				ctx, cancel := context.WithTimeout(sampling, time.Second)
				var locks int64
				_ = f.db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'`).Scan(&locks)
				cancel()
				if locks > peakLocks.Load() {
					peakLocks.Store(locks)
				}
				if n := int64(f.db.Stats().OpenConnections); n > peakConnections.Load() {
					peakConnections.Store(n)
				}
			}
		}
	}()
	productionDeadline := start.Add(time.Duration(seconds) * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for time.Now().Before(productionDeadline) {
		<-ticker.C
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err = receiveSized(ctx, f, f.worker.Resources, 32<<10, 25<<20)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		total++
	}
	producedAt := time.Now()
	drainDeadline := producedAt.Add(90 * time.Second)
	for {
		var pending int
		if err = f.db.QueryRow(`SELECT count(*) FROM messages WHERE smtp_account_id=$1 AND status<>'SMTP_ACCEPTED'`, f.accountID).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending == 0 {
			break
		}
		if time.Now().After(drainDeadline) {
			t.Fatal("queue failed to drain", pending)
		}
		time.Sleep(100 * time.Millisecond)
	}
	elapsed := time.Since(start)
	drain := time.Since(producedAt)
	stopClaims()
	<-done
	stopSampling()
	<-sampleDone
	if received.Load() != int64(total) {
		t.Fatal("duplicate/lost load submission", total, received.Load())
	}
	var p50, p95, p99 float64
	if err = f.db.QueryRow(`SELECT percentile_cont(0.5) WITHIN GROUP(ORDER BY extract(epoch FROM completed_at-created_at)),percentile_cont(0.95) WITHIN GROUP(ORDER BY extract(epoch FROM completed_at-created_at)),percentile_cont(0.99) WITHIN GROUP(ORDER BY extract(epoch FROM completed_at-created_at)) FROM messages WHERE smtp_account_id=$1`, f.accountID).Scan(&p50, &p95, &p99); err != nil {
		t.Fatal(err)
	}
	after, err := sampleTables(context.Background(), f.db)
	if err != nil {
		t.Fatal(err)
	}
	var walBytes int64
	if err = f.db.QueryRow(`SELECT pg_wal_lsn_diff(pg_current_wal_lsn(),$1::pg_lsn)::bigint`, walStart).Scan(&walBytes); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"messages", "recipients", "delivery_attempts", "attempt_recipients", "events", "provider_quota"} {
		if _, err = f.db.Exec(`VACUUM (ANALYZE) ` + table); err != nil {
			t.Fatal(err)
		}
	}
	vacuumed, err := sampleTables(context.Background(), f.db)
	if err != nil {
		t.Fatal(err)
	}
	resources := f.worker.Resources.Stats()
	if resources.Memory != 0 || resources.Spool != 0 || resources.Waiting != 0 || snapshotFDs() != 0 {
		t.Fatal("resource leak after drain", resources)
	}
	report := map[string]any{
		"duration_seconds": seconds, "message_bytes": 32 << 10, "messages": total, "initial_backlog": 40, "workers": 4,
		"elapsed_seconds": elapsed.Seconds(), "drain_seconds": drain.Seconds(), "accepted_per_second": float64(total) / elapsed.Seconds(),
		"latency_seconds": map[string]float64{"p50": p50, "p95": p95, "p99": p99}, "baseline_heap_bytes": baseline.HeapAlloc, "peak_heap_bytes": peakHeap.Load(), "baseline_rss_bytes": baseRSS, "peak_rss_bytes": peakRSS.Load(),
		"baseline_goroutines": baseGoroutines, "final_goroutines": runtime.NumGoroutine(), "snapshot_fds_after_drain": snapshotFDs(), "resource_reservations": resources,
		"peak_database_connections": peakConnections.Load(), "sampled_peak_lock_waiters": peakLocks.Load(), "wal_bytes": walBytes, "tables_before": before, "tables_after": after, "tables_after_manual_vacuum": vacuumed,
	}
	encoded, _ := json.Marshal(report)
	fmt.Println("LOAD_REPORT " + string(encoded))
	if peakHeap.Load() > 128<<20 {
		t.Fatal("load fixture exceeded declared 128 MiB Go heap acceptance bound")
	}
}
