package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"relaytale/internal/feedback"
)

func publicationFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, feedback.ErrInvalidPublication):
		respond(w, 400, "invalid_request")
	case errors.Is(err, feedback.ErrPublication):
		respond(w, 409, "feedback_conflict")
	default:
		failure(w, err)
	}
}
func (a Management) feedbackLink(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var v feedback.LinkRequest
	if !body(w, r, &v) {
		return
	}
	if v.FeedbackID != id {
		respond(w, 400, "invalid_request")
		return
	}
	out, err := (feedback.Service{DB: a.DB}).Link(r.Context(), v, actor(r))
	if err != nil {
		publicationFailure(w, err)
		return
	}
	output(w, 201, out)
}
func (a Management) feedbackReview(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var v feedback.ReviewRequest
	if !body(w, r, &v) {
		return
	}
	out, err := (feedback.Service{DB: a.DB}).Review(r.Context(), id, v, actor(r))
	if err != nil {
		publicationFailure(w, err)
		return
	}
	output(w, 200, out)
}
func (a Management) feedbackLinks(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, ok := feedbackQuery(w, r); !ok {
		return
	}
	rows, err := a.DB.QueryContext(r.Context(), feedback.Projection+` WHERE l.feedback_id=$1 ORDER BY l.id LIMIT 100`, id)
	if err != nil {
		failure(w, err)
		return
	}
	defer rows.Close()
	out := []feedback.Publication{}
	for rows.Next() {
		item, e := feedback.ScanPublication(rows)
		if e != nil {
			failure(w, e)
			return
		}
		out = append(out, item)
	}
	if err = rows.Err(); err != nil {
		failure(w, err)
		return
	}
	output(w, 200, map[string]any{"items": out})
}
func readWebsiteFeedback(ctx context.Context, w http.ResponseWriter, r *http.Request, db *sql.DB, who *sendIdentity, id string) {
	if !canonicalUUID(id) || r.URL.RawPath != "" || len(r.URL.RawQuery) > 256 || r.ContentLength != 0 || len(r.TransferEncoding) > 0 || len(r.Header.Values("Content-Encoding")) > 0 {
		respond(w, 400, "invalid_query")
		return
	}
	if _, ok := feedbackQuery(w, r, "after", "limit"); !ok {
		return
	}
	after, limit, ok := page(w, r)
	if !ok {
		return
	}
	tx, ok := beginWebsiteRead(ctx, w, db, who)
	if !ok {
		return
	}
	defer tx.Rollback()
	var found string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE id=$1 AND api_key_id=$2 AND source_type='api' AND environment='production'`, id, who.key.ID).Scan(&found); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			respond(w, 404, "not_found")
		} else {
			failure(w, err)
		}
		return
	}
	rows, err := tx.QueryContext(ctx, feedback.Projection+` WHERE l.message_id=$1 AND l.id>$2 ORDER BY l.id LIMIT $3`, id, after, limit+1)
	if err != nil {
		failure(w, err)
		return
	}
	out := []feedback.Publication{}
	for rows.Next() {
		item, e := feedback.ScanPublication(rows)
		if e != nil {
			rows.Close()
			failure(w, e)
			return
		}
		out = append(out, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		failure(w, err)
		return
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	next := ""
	if more {
		next = out[len(out)-1].ID
	}
	if err = tx.Commit(); err != nil {
		failure(w, err)
		return
	}
	output(w, 200, map[string]any{"schema_version": 1, "message_id": id, "source_trust": "UNVERIFIED", "automatic_suppression": false, "items": out, "has_more": more, "next_after": next})
}
