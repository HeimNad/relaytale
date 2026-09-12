// Package metrics exposes bounded, aggregate Prometheus gauges without mail PII.
package metrics

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"time"

	"relaytale/internal/resource"
)

type Handler struct {
	DB        *sql.DB
	Resources *resource.Limiter
	mu        sync.Mutex
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if !h.mu.TryLock() {
		http.Error(w, "metrics scrape busy", http.StatusServiceUnavailable)
		return
	}
	defer h.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	var b bytes.Buffer
	if err := h.collect(ctx, &b); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, "relaytale_metrics_scrape_success 0")
		return
	}
	fmt.Fprintln(&b, "relaytale_metrics_scrape_success 1")
	_, _ = w.Write(b.Bytes())
}
func gauge(b *bytes.Buffer, name, help string, value any) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n%s %v\n", name, help, name, name, value)
}
func (h *Handler) collect(ctx context.Context, b *bytes.Buffer) error {
	fmt.Fprintln(b, "# HELP relaytale_messages Messages by durable projection, not inbox delivery.\n# TYPE relaytale_messages gauge")
	rows, err := h.DB.QueryContext(ctx, `SELECT status,count(*) FROM messages GROUP BY status`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var status string
		var n int64
		if err = rows.Scan(&status, &n); err != nil {
			rows.Close()
			return err
		}
		fmt.Fprintf(b, "relaytale_messages{status=%q} %d\n", messageStatus(status), n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var age float64
	if err = h.DB.QueryRowContext(ctx, `SELECT coalesce(greatest(0,extract(epoch FROM clock_timestamp()-min(created_at))),0) FROM messages WHERE status='QUEUED'`).Scan(&age); err != nil {
		return err
	}
	gauge(b, "relaytale_queue_oldest_seconds", "Age of oldest queued message including blocked routes.", age)
	fmt.Fprintln(b, "# HELP relaytale_attempts_last_24h Attempts started in rolling 24 hours; this is a gauge.\n# TYPE relaytale_attempts_last_24h gauge")
	rows, err = h.DB.QueryContext(ctx, `SELECT CASE WHEN result IN ('IN_PROGRESS','SMTP_ACCEPTED','PARTIAL_ACCEPTED','TEMP_FAILED','PERM_FAILED','DELIVERY_UNKNOWN') THEN result ELSE 'OTHER' END,count(*) FROM delivery_attempts WHERE started_at>clock_timestamp()-interval '24 hours' GROUP BY 1`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var status string
		var n int64
		if err = rows.Scan(&status, &n); err != nil {
			rows.Close()
			return err
		}
		fmt.Fprintf(b, "relaytale_attempts_last_24h{result=%q} %d\n", status, n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var limited, open, half int64
	err = h.DB.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE (p.hourly_limit IS NOT NULL AND q.hourly>=p.hourly_limit) OR (p.daily_limit IS NOT NULL AND q.daily>=p.daily_limit)),count(*) FILTER(WHERE h.circuit_state='OPEN'),count(*) FILTER(WHERE h.circuit_state='HALF_OPEN') FROM providers p LEFT JOIN provider_health h ON h.provider_id=p.id CROSS JOIN LATERAL(SELECT coalesce(sum(units) FILTER(WHERE reserved_at>clock_timestamp()-interval '1 hour'),0) hourly,coalesce(sum(units),0) daily FROM provider_quota WHERE provider_id=p.id AND reserved_at>clock_timestamp()-interval '24 hours') q WHERE p.enabled`).Scan(&limited, &open, &half)
	if err != nil {
		return err
	}
	gauge(b, "relaytale_providers_quota_exhausted", "Enabled providers with exhausted rolling quota.", limited)
	gauge(b, "relaytale_providers_circuit_open", "Enabled providers recorded OPEN; enforcement can be disabled.", open)
	gauge(b, "relaytale_providers_circuit_half_open", "Enabled providers recorded HALF_OPEN.", half)
	stats := h.Resources.Stats()
	gauge(b, "relaytale_memory_reserved_bytes", "Active work reservations, not process RSS.", stats.Memory)
	gauge(b, "relaytale_memory_budget_bytes", "Admission working-set budget.", stats.MemoryLimit)
	gauge(b, "relaytale_snapshot_reserved_bytes", "Conservative maximum outbound snapshot reservations.", stats.Spool)
	gauge(b, "relaytale_snapshot_budget_bytes", "Outbound snapshot disk budget.", stats.SpoolLimit)
	gauge(b, "relaytale_resource_waiters", "Operations waiting before receiving or claiming.", stats.Waiting)
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	gauge(b, "relaytale_go_heap_alloc_bytes", "Current Go heap allocation; excludes OS cache.", mem.HeapAlloc)
	gauge(b, "relaytale_go_heap_sys_bytes", "Heap address space obtained from OS.", mem.HeapSys)
	gauge(b, "relaytale_go_goroutines", "Current goroutine count.", runtime.NumGoroutine())
	db := h.DB.Stats()
	gauge(b, "relaytale_db_connections_open", "Connections in application SQL pool.", db.OpenConnections)
	gauge(b, "relaytale_db_connections_in_use", "Checked-out SQL pool connections.", db.InUse)
	return nil
}
func messageStatus(s string) string {
	switch s {
	case "RECEIVED", "ARCHIVED", "QUEUED", "SENDING", "SMTP_ACCEPTED", "PARTIAL_ACCEPTED", "TEMP_FAILED", "PERM_FAILED", "DELIVERY_UNKNOWN", "BOUNCED", "DELIVERED", "SUPPRESSED", "CANCELLED":
		return s
	}
	return "OTHER"
}
