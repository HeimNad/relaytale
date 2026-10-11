package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"
	"relaytale/internal/message"
)

func cancelWebsiteMessage(ctx context.Context, w http.ResponseWriter, r *http.Request, repo message.Postgres, who *sendIdentity, id string) {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil || parsed.String() != id {
		respond(w, 404, "message_not_found")
		return
	}
	if r.URL.RawQuery != "" || r.URL.RawPath != "" || len(r.Header.Values("Content-Encoding")) != 0 {
		respond(w, 400, "invalid_query")
		return
	}
	// This operation has no mutable options; repeated calls are naturally idempotent.
	body, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if err != nil || len(body) != 0 {
		respond(w, 400, "empty_body_required")
		return
	}
	replay, err := repo.CancelPending(ctx, who.key.ID, id)
	switch {
	case errors.Is(err, message.ErrControlNotFound):
		respond(w, 404, "message_not_found")
		return
	case errors.Is(err, message.ErrNotCancellable):
		respond(w, 409, "message_not_cancellable")
		return
	case errors.Is(err, message.ErrSubmissionDisabled):
		respond(w, 401, "invalid_credentials")
		return
	case err != nil:
		respond(w, 503, "unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"message_id": id, "status": "CANCELLED", "replayed": replay})
}
