package api

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"relaytale/internal/message"
)

// Authorization and response share one snapshot. Revocation after snapshot
// acquisition applies to later requests, not an already-running read.
func beginWebsiteRead(ctx context.Context, w http.ResponseWriter, db *sql.DB, who *sendIdentity) (*sql.Tx, bool) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		respond(w, 503, "unavailable")
		return nil, false
	}
	var enabled bool
	err = tx.QueryRowContext(ctx, `SELECT enabled AND prefix='rts_' AND key_hash=$2 FROM api_keys WHERE id=$1`, who.key.ID, hex.EncodeToString(who.digest[:])).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !enabled {
		tx.Rollback()
		respond(w, 401, "invalid_credentials")
		return nil, false
	}
	if err != nil {
		tx.Rollback()
		respond(w, 503, "unavailable")
		return nil, false
	}
	return tx, true
}

type businessQuery struct {
	Reference, Tag, Before string
	Limit                  int
}

func searchPage(r *http.Request) (businessQuery, bool) {
	out := businessQuery{Limit: 50}
	if r.URL.RawPath != "" || len(r.URL.RawQuery) > 1024 || r.ContentLength != 0 || len(r.TransferEncoding) > 0 || len(r.Header.Values("Content-Encoding")) > 0 {
		return out, false
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return out, false
	}
	for k, v := range q {
		if len(v) != 1 || (k != "client_reference" && k != "tag" && k != "before" && k != "limit") {
			return out, false
		}
	}
	out.Reference = q.Get("client_reference")
	out.Tag = q.Get("tag")
	out.Before = q.Get("before")
	if _, ok := q["client_reference"]; ok && !message.ValidReference(out.Reference) {
		return out, false
	}
	if _, ok := q["tag"]; ok && !message.ValidTag(out.Tag) {
		return out, false
	}
	if _, ok := q["before"]; ok && !canonicalUUID(out.Before) {
		return out, false
	}
	if out.Reference == "" && out.Tag == "" {
		return out, false
	}
	if v, ok := q["limit"]; ok {
		n, e := strconv.Atoi(v[0])
		if e != nil || n < 1 || n > 100 {
			return out, false
		}
		out.Limit = n
	}
	return out, true
}

type MessageSummary struct {
	MessageID   string           `json:"message_id"`
	Status      string           `json:"status"`
	Description string           `json:"description"`
	CreatedAt   time.Time        `json:"created_at"`
	Business    message.Business `json:"business"`
}
type MessageList struct {
	Simulation    bool             `json:"simulation,omitempty"`
	Environment   string           `json:"environment,omitempty"`
	SchemaVersion int              `json:"schema_version"`
	SnapshotAt    time.Time        `json:"snapshot_at"`
	Messages      []MessageSummary `json:"messages"`
	HasMore       bool             `json:"has_more"`
	NextBefore    string           `json:"next_before,omitempty"`
}

func readMessageList(ctx context.Context, w http.ResponseWriter, r *http.Request, db *sql.DB, who *sendIdentity) {
	q, ok := searchPage(r)
	if !ok {
		respond(w, 400, "invalid_query")
		return
	}
	tx, ok := beginWebsiteRead(ctx, w, db, who)
	if !ok {
		return
	}
	defer tx.Rollback()
	out := MessageList{SchemaVersion: 2, Messages: []MessageSummary{}}
	if who.key.Environment == "sandbox" {
		out.Simulation = true
		out.Environment = "sandbox"
	}
	if err := tx.QueryRowContext(ctx, `SELECT transaction_timestamp()`).Scan(&out.SnapshotAt); err != nil {
		respond(w, 503, "unavailable")
		return
	}
	var beforeTime time.Time
	if q.Before != "" {
		err := tx.QueryRowContext(ctx, `SELECT created_at FROM messages WHERE id=$4 AND api_key_id=$1 AND source_type='api' AND ($2='' OR client_reference=$2) AND ($3='' OR business_tags @> ARRAY[$3]::text[])`, who.key.ID, q.Reference, q.Tag, q.Before).Scan(&beforeTime)
		if errors.Is(err, sql.ErrNoRows) {
			respond(w, 400, "invalid_cursor")
			return
		}
		if err != nil {
			respond(w, 503, "unavailable")
			return
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id::text,status,created_at,client_reference,to_jsonb(business_tags) FROM messages WHERE api_key_id=$1 AND source_type='api' AND ($2='' OR client_reference=$2) AND ($3='' OR business_tags @> ARRAY[$3]::text[]) AND ($4='' OR (created_at,id)<($5::timestamptz,NULLIF($4,'')::uuid)) ORDER BY created_at DESC,id DESC LIMIT $6`, who.key.ID, q.Reference, q.Tag, q.Before, beforeTime, q.Limit+1)
	if err != nil {
		respond(w, 503, "unavailable")
		return
	}
	for rows.Next() {
		var v MessageSummary
		var tags []byte
		if err = rows.Scan(&v.MessageID, &v.Status, &v.CreatedAt, &v.Business.ClientReference, &tags); err != nil {
			break
		}
		if err = json.Unmarshal(tags, &v.Business.Tags); err != nil {
			break
		}
		v.Description = statusDescription(v.Status)
		out.Messages = append(out.Messages, v)
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil || rowErr != nil {
		respond(w, 503, "unavailable")
		return
	}
	if len(out.Messages) > q.Limit {
		out.Messages = out.Messages[:q.Limit]
		out.HasMore = true
		out.NextBefore = out.Messages[len(out.Messages)-1].MessageID
	}
	if err = tx.Commit(); err != nil {
		respond(w, 503, "unavailable")
		return
	}
	output(w, 200, out)
}
