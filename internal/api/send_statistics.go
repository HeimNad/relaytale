package api

import (
	"context"
	"database/sql"
	"math"
	"net/http"
	"net/url"
	"time"
)

const statisticsMessageLimit = 10000

type statisticsWindow struct{ From, Until time.Time }

func statisticsPage(r *http.Request) (statisticsWindow, bool) {
	var out statisticsWindow
	if r.URL.RawPath != "" || len(r.URL.RawQuery) > 256 || r.ContentLength != 0 || len(r.TransferEncoding) > 0 || len(r.Header.Values("Content-Encoding")) > 0 {
		return out, false
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(q) != 2 {
		return out, false
	}
	for k, v := range q {
		if (k != "from" && k != "until") || len(v) != 1 {
			return out, false
		}
	}
	out.From, err = time.Parse(time.RFC3339Nano, q.Get("from"))
	if err != nil {
		return out, false
	}
	out.Until, err = time.Parse(time.RFC3339Nano, q.Get("until"))
	if err != nil {
		return out, false
	}
	out.From = out.From.UTC()
	out.Until = out.Until.UTC()
	if out.From.Year() < 1 || out.Until.Year() > 9999 || out.From.Nanosecond()%1000 != 0 || out.Until.Nanosecond()%1000 != 0 || !out.From.Before(out.Until) || out.Until.Sub(out.From) > 31*24*time.Hour {
		return out, false
	}
	return out, true
}

type WebsiteStatistics struct {
	SchemaVersion                 int              `json:"schema_version"`
	Environment                   string           `json:"environment"`
	Simulation                    bool             `json:"simulation"`
	SnapshotAt                    time.Time        `json:"snapshot_at"`
	From                          time.Time        `json:"from"`
	Until                         time.Time        `json:"until"`
	MessageCount                  int              `json:"message_count"`
	MessagesByStatus              map[string]int   `json:"messages_by_status"`
	ArchivesByState               map[string]int   `json:"archives_by_state"`
	LogicalRawBytesByArchiveState map[string]int64 `json:"logical_raw_bytes_by_archive_state"`
	FeedbackAvailability          string           `json:"feedback_availability"`
}

func readWebsiteStatistics(ctx context.Context, w http.ResponseWriter, r *http.Request, db *sql.DB, who *sendIdentity) {
	window, ok := statisticsPage(r)
	if !ok {
		respond(w, 400, "invalid_query")
		return
	}
	tx, ok := beginWebsiteRead(ctx, w, db, who)
	if !ok {
		return
	}
	defer tx.Rollback()
	out := WebsiteStatistics{SchemaVersion: 1, Environment: who.key.Environment, Simulation: who.key.Environment == "sandbox", From: window.From, Until: window.Until, MessagesByStatus: map[string]int{}, ArchivesByState: map[string]int{}, LogicalRawBytesByArchiveState: map[string]int64{}, FeedbackAvailability: "NOT_EXPOSED"}
	if err := tx.QueryRowContext(ctx, "SELECT transaction_timestamp()").Scan(&out.SnapshotAt); err != nil {
		respond(w, 503, "unavailable")
		return
	}
	if window.Until.After(out.SnapshotAt) {
		respond(w, 400, "invalid_query")
		return
	}
	rows, err := tx.QueryContext(ctx, `SELECT status,archive_state,eml_size FROM messages WHERE api_key_id=$1 AND source_type='api' AND created_at>=$2 AND created_at<$3 ORDER BY created_at DESC,id DESC LIMIT $4`, who.key.ID, window.From, window.Until, statisticsMessageLimit+1)
	if err != nil {
		respond(w, 503, "unavailable")
		return
	}
	defer rows.Close()
	for rows.Next() {
		if out.MessageCount == statisticsMessageLimit {
			respond(w, 422, "statistics_window_too_large")
			return
		}
		var status, state string
		var size int64
		if err = rows.Scan(&status, &state, &size); err != nil || size < 0 || size > math.MaxInt64-out.LogicalRawBytesByArchiveState[state] {
			respond(w, 503, "unavailable")
			return
		}
		out.MessageCount++
		out.MessagesByStatus[status]++
		out.ArchivesByState[state]++
		out.LogicalRawBytesByArchiveState[state] += size
	}
	if err = rows.Err(); err != nil {
		respond(w, 503, "unavailable")
		return
	}
	if err = rows.Close(); err != nil {
		respond(w, 503, "unavailable")
		return
	}
	if err = tx.Commit(); err != nil {
		respond(w, 503, "unavailable")
		return
	}
	output(w, 200, out)
}
