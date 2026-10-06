package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"relaytale/internal/message"
	"relaytale/internal/mimeinspect"
	"relaytale/internal/storage"
)

func sandboxRoute(path string) (string, string, bool) {
	if !strings.HasPrefix(path, "/v1/messages/") {
		return "", "", false
	}
	id, op, ok := strings.Cut(strings.TrimPrefix(path, "/v1/messages/"), "/")
	return id, op, ok && (op == "inspection" || op == "simulations")
}
func sandboxRequest(w http.ResponseWriter, r *http.Request, who *sendIdentity, id string) bool {
	if who.key.Environment != "sandbox" {
		respond(w, 403, "sandbox_identity_required")
		return false
	}
	if !canonicalUUID(id) || r.URL.RawPath != "" || r.URL.RawQuery != "" || len(r.Header.Values("Content-Encoding")) != 0 {
		respond(w, 400, "invalid_request")
		return false
	}
	return true
}
func inspectSandbox(ctx context.Context, w http.ResponseWriter, r *http.Request, receiver message.APIReceiver, who *sendIdentity, id string) {
	if !sandboxRequest(w, r, who, id) {
		return
	}
	if r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
		respond(w, 400, "body_not_allowed")
		return
	}
	tx, ok := beginWebsiteRead(ctx, w, receiver.Repo.DB, who)
	if !ok {
		return
	}
	defer tx.Rollback()
	var archive storage.Archive
	var state string
	err := tx.QueryRowContext(ctx, `SELECT eml_path,eml_size,eml_sha256,archive_state FROM messages WHERE id=$1 AND api_key_id=$2 AND environment='sandbox'`, id, who.key.ID).Scan(&archive.Path, &archive.Size, &archive.SHA256, &state)
	if errors.Is(err, sql.ErrNoRows) {
		respond(w, 404, "not_found")
		return
	}
	if err != nil {
		respond(w, 503, "unavailable")
		return
	}
	if state != "AVAILABLE" {
		respond(w, 410, "archive_unavailable")
		return
	}
	if archive.Size > mimeinspect.MaxBytes {
		respond(w, 413, "inspection_size_limit")
		return
	}
	if err = tx.Commit(); err != nil {
		respond(w, 503, "unavailable")
		return
	}
	reader, ok := receiver.Store.(interface {
		ReadVerified(context.Context, storage.Archive, int64) ([]byte, error)
	})
	if !ok {
		respond(w, 503, "inspection_unavailable")
		return
	}
	release, err := receiver.Resources.Acquire(ctx, mimeinspect.MemoryBudget, 0)
	if err != nil {
		respond(w, 503, "inspection_busy")
		return
	}
	defer release()
	raw, err := reader.ReadVerified(ctx, archive, mimeinspect.MaxBytes)
	if err != nil {
		respond(w, 503, "archive_verification_failed")
		return
	}
	report, err := mimeinspect.Inspect(raw)
	if err != nil {
		respond(w, 422, "mime_inspection_failed")
		return
	}
	output(w, 200, map[string]any{"schema_version": 1, "message_id": id, "environment": "sandbox", "simulation": true, "eml_size": archive.Size, "eml_sha256": archive.SHA256, "parts": report.Parts})
}

type simulationRequest struct {
	RecipientID string `json:"recipient_id"`
	Outcome     string `json:"outcome"`
}

var simulationKinds = map[string]string{
	"accepted":          "SANDBOX_SIMULATED_ACCEPTED",
	"temporary_failure": "SANDBOX_SIMULATED_TEMP_FAILED",
	"permanent_failure": "SANDBOX_SIMULATED_PERM_FAILED",
	"delivery_unknown":  "SANDBOX_SIMULATED_UNKNOWN",
	"bounce":            "SANDBOX_SIMULATED_BOUNCE",
}

func simulateSandbox(ctx context.Context, w http.ResponseWriter, r *http.Request, db *sql.DB, who *sendIdentity, id string) {
	if !sandboxRequest(w, r, who, id) {
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(10 * time.Second))
	media, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if len(r.Header.Values("Content-Type")) != 1 || err != nil || media != "application/json" || len(params) > 0 {
		respond(w, 415, "use_application_json")
		return
	}
	keys := r.Header.Values("Idempotency-Key")
	if len(keys) != 1 || len(keys[0]) < 1 || len(keys[0]) > 128 {
		respond(w, 400, "invalid_idempotency_key")
		return
	}
	for _, c := range keys[0] {
		if c < 33 || c > 126 {
			respond(w, 400, "invalid_idempotency_key")
			return
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 513))
	if err != nil || len(body) > 512 {
		respond(w, 400, "invalid_simulation")
		return
	}
	if uniqueJSON(json.NewDecoder(bytes.NewReader(body))) != nil {
		respond(w, 400, "invalid_simulation")
		return
	}
	var request simulationRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || !canonicalUUID(request.RecipientID) || simulationKinds[request.Outcome] == "" {
		respond(w, 400, "invalid_simulation")
		return
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		respond(w, 503, "unavailable")
		return
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		respond(w, 503, "unavailable")
		return
	}
	var enabled bool
	err = tx.QueryRowContext(ctx, `SELECT enabled AND prefix='rts_' AND key_hash=$2 AND environment='sandbox' FROM api_keys WHERE id=$1 FOR SHARE`, who.key.ID, hex.EncodeToString(who.digest[:])).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !enabled {
		respond(w, 401, "invalid_credentials")
		return
	}
	if err != nil {
		respond(w, 503, "unavailable")
		return
	}
	var found string
	err = tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE id=$1 AND api_key_id=$2 AND environment='sandbox' FOR UPDATE`, id, who.key.ID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		respond(w, 404, "not_found")
		return
	}
	if err != nil {
		respond(w, 503, "unavailable")
		return
	}
	digest := sha256.Sum256([]byte(keys[0]))
	keyHash := hex.EncodeToString(digest[:])
	var eventID, recipient, outcome string
	err = tx.QueryRowContext(ctx, `SELECT event_id,recipient_id,outcome FROM sandbox_simulations WHERE message_id=$1 AND key_sha256=$2`, id, keyHash).Scan(&eventID, &recipient, &outcome)
	if err == nil {
		if recipient != request.RecipientID || outcome != request.Outcome {
			respond(w, 409, "idempotency_conflict")
			return
		}
		output(w, 200, map[string]any{"event_id": eventID, "replayed": true, "environment": "sandbox", "simulation": true})
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		respond(w, 503, "unavailable")
		return
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM sandbox_simulations WHERE message_id=$1`, id).Scan(&count); err != nil {
		respond(w, 503, "unavailable")
		return
	}
	if count >= 100 {
		respond(w, 409, "simulation_limit")
		return
	}
	if err = tx.QueryRowContext(ctx, `SELECT id FROM recipients WHERE id=$1 AND message_id=$2`, request.RecipientID, id).Scan(&found); errors.Is(err, sql.ErrNoRows) {
		respond(w, 404, "recipient_not_found")
		return
	} else if err != nil {
		respond(w, 503, "unavailable")
		return
	}
	eventID = uuid.Must(uuid.NewV7()).String()
	data, _ := json.Marshal(map[string]any{"simulation": true, "environment": "sandbox", "outcome": request.Outcome})
	_, err = tx.ExecContext(ctx, `INSERT INTO events(id,message_id,recipient_id,event_type,source,data) VALUES($1,$2,$3,$4,'sandbox',$5)`, eventID, id, request.RecipientID, simulationKinds[request.Outcome], string(data))
	if err == nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO sandbox_simulations(message_id,key_sha256,recipient_id,outcome,event_id) VALUES($1,$2,$3,$4,$5)`, id, keyHash, request.RecipientID, request.Outcome, eventID)
	}
	if err != nil {
		respond(w, 503, "retry_same_request_and_key")
		return
	}
	if err = tx.Commit(); err != nil {
		respond(w, 503, "retry_same_request_and_key")
		return
	}
	output(w, 200, map[string]any{"event_id": eventID, "replayed": false, "environment": "sandbox", "simulation": true})
}
