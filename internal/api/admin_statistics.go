package api

import (
	"database/sql"
	"math"
	"net/http"
	"sort"
	"time"
)

// Capacity groups use stable API identities, not credential names or tokens.
// SMTP and internal messages are grouped by source; raw bytes are logical only.
type CapacityGroup struct {
	Source                        string           `json:"source"`
	APIKeyID                      string           `json:"api_key_id,omitempty"`
	Environment                   string           `json:"environment"`
	MessageCount                  int              `json:"message_count"`
	MessagesByStatus              map[string]int   `json:"messages_by_status"`
	ArchivesByState               map[string]int   `json:"archives_by_state"`
	LogicalRawBytesByArchiveState map[string]int64 `json:"logical_raw_bytes_by_archive_state"`
}
type CapacityStatistics struct {
	SchemaVersion int             `json:"schema_version"`
	SnapshotAt    time.Time       `json:"snapshot_at"`
	From          time.Time       `json:"from"`
	Until         time.Time       `json:"until"`
	MessageCount  int             `json:"message_count"`
	Groups        []CapacityGroup `json:"groups"`
}

func (a Management) capacityStatistics(w http.ResponseWriter, r *http.Request) {
	// net/http GET patterns also match HEAD; do not run expensive reads for HEAD.
	if r.Method != http.MethodGet {
		respond(w, 405, "method_not_allowed")
		return
	}
	window, ok := statisticsPage(r)
	if !ok {
		respond(w, 400, "invalid_query")
		return
	}
	tx, err := a.DB.BeginTx(r.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		respond(w, 503, "unavailable")
		return
	}
	defer tx.Rollback()
	out := CapacityStatistics{SchemaVersion: 1, From: window.From, Until: window.Until, Groups: []CapacityGroup{}}
	if err = tx.QueryRowContext(r.Context(), "SELECT transaction_timestamp()").Scan(&out.SnapshotAt); err != nil {
		respond(w, 503, "unavailable")
		return
	}
	if window.Until.After(out.SnapshotAt) {
		respond(w, 400, "invalid_query")
		return
	}
	rows, err := tx.QueryContext(r.Context(), `SELECT source_type,COALESCE(api_key_id::text,''),environment,status,archive_state,eml_size FROM messages WHERE created_at >= $1 AND created_at < $2 ORDER BY created_at DESC LIMIT $3`, window.From, window.Until, statisticsMessageLimit+1)
	if err != nil {
		respond(w, 503, "unavailable")
		return
	}
	defer rows.Close()
	groups := map[string]*CapacityGroup{}
	for rows.Next() {
		if out.MessageCount == statisticsMessageLimit {
			respond(w, 422, "statistics_window_too_large")
			return
		}
		var source, id, env, status, state string
		var size int64
		if err = rows.Scan(&source, &id, &env, &status, &state, &size); err != nil {
			respond(w, 503, "unavailable")
			return
		}
		if source != "api" {
			id = ""
		}
		key := source + "/" + env + "/" + id
		group := groups[key]
		if group == nil {
			if len(groups) == 256 {
				respond(w, 422, "statistics_window_too_large")
				return
			}
			group = &CapacityGroup{Source: source, APIKeyID: id, Environment: env, MessagesByStatus: map[string]int{}, ArchivesByState: map[string]int{}, LogicalRawBytesByArchiveState: map[string]int64{}}
			groups[key] = group
		}
		if size < 0 || size > math.MaxInt64-group.LogicalRawBytesByArchiveState[state] {
			respond(w, 503, "unavailable")
			return
		}
		group.MessageCount++
		group.MessagesByStatus[status]++
		group.ArchivesByState[state]++
		group.LogicalRawBytesByArchiveState[state] += size
		out.MessageCount++
	}
	if err = rows.Err(); err != nil {
		respond(w, 503, "unavailable")
		return
	}
	if err = rows.Close(); err != nil {
		respond(w, 503, "unavailable")
		return
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out.Groups = append(out.Groups, *groups[k])
	}
	if err = tx.Commit(); err != nil {
		respond(w, 503, "unavailable")
		return
	}
	output(w, 200, out)
}
