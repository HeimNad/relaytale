package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"relaytale/internal/auth"
	"relaytale/internal/message"
	"relaytale/internal/storage"
)

type SendKey struct {
	Environment string                   `json:"environment,omitempty"`
	Streams     []message.StreamPolicy   `json:"streams,omitempty"`
	Budget      message.SubmissionBudget `json:"budget,omitempty"`
	ID          string                   `json:"id"`
	Token       string                   `json:"token"`
	AllowedFrom []string                 `json:"allowed_from"`
}

func ParseSendKeys(raw string) ([]SendKey, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	invalid := errors.New("invalid SEND_API_KEYS configuration")
	if len(raw) > 65536 {
		return nil, invalid
	}
	if e := uniqueJSON(json.NewDecoder(strings.NewReader(raw))); e != nil {
		return nil, invalid
	}
	var keys []SendKey
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(&keys); e != nil || len(keys) == 0 || len(keys) > 32 {
		return nil, invalid
	}
	ids, tokens := map[string]bool{}, map[string]bool{}
	for i := range keys {
		k := &keys[i]
		if k.Environment == "" {
			k.Environment = "production"
		}
		if k.Environment != "production" && k.Environment != "sandbox" {
			return nil, invalid
		}
		var streamErr error
		k.Streams, streamErr = message.CanonicalStreams(k.Streams)
		if streamErr != nil {
			return nil, invalid
		}
		if !k.Budget.Valid() {
			return nil, invalid
		}
		id, e := uuid.Parse(k.ID)
		if e != nil || id == uuid.Nil || id.String() != k.ID || ids[k.ID] || tokens[k.Token] || len(k.Token) != 68 || !strings.HasPrefix(k.Token, "rts_") || len(k.AllowedFrom) == 0 || len(k.AllowedFrom) > 32 {
			return nil, invalid
		}
		b, e := hex.DecodeString(k.Token[4:])
		if e != nil || len(b) != 32 || hex.EncodeToString(b) != k.Token[4:] {
			return nil, invalid
		}
		seen := map[string]bool{}
		for j, address := range k.AllowedFrom {
			canonical, e := auth.CanonicalAddress(address)
			if e != nil || seen[canonical] {
				return nil, invalid
			}
			seen[canonical] = true
			k.AllowedFrom[j] = canonical
		}
		ids[k.ID], tokens[k.Token] = true, true
	}
	return keys, nil
}

type sendIdentity struct {
	key     SendKey
	digest  [32]byte
	mu      sync.Mutex
	tokens  float64
	updated time.Time
	active  int
}

func (p *sendIdentity) acquire() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	p.tokens += now.Sub(p.updated).Seconds()
	if p.tokens > 10 {
		p.tokens = 10
	}
	p.updated = now
	if p.active >= 2 || p.tokens < 1 {
		return false
	}
	p.tokens--
	p.active++
	return true
}
func (p *sendIdentity) release() { p.mu.Lock(); p.active--; p.mu.Unlock() }

// NewSendHandler snapshots explicit configuration. Rotation/revocation requires
// restart; every request also checks the persisted enabled flag and token hash.
func NewSendHandler(ctx context.Context, raw string, receiver message.APIReceiver) (http.Handler, error) {
	keys, e := ParseSendKeys(raw)
	if e != nil {
		return nil, e
	}
	if len(keys) == 0 {
		return nil, nil
	}
	if receiver.MaxBytes < 1 || receiver.MaxBytes > 100<<20 || receiver.Store == nil || receiver.Repo.DB == nil {
		return nil, errors.New("invalid HTTP submission receiver")
	}
	tx, e := receiver.Repo.DB.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on`); e != nil {
		return nil, e
	}
	people := make([]*sendIdentity, 0, len(keys))
	for _, key := range keys {
		digest := sha256.Sum256([]byte(key.Token))
		_, e = tx.ExecContext(ctx, `INSERT INTO api_keys(id,name,key_hash,prefix,enabled,submission_minute_limit,submission_hour_limit,submission_day_limit,environment) VALUES($1,$2,$3,'rts_',true,$4,$5,$6,$7)
 ON CONFLICT(id) DO UPDATE SET key_hash=EXCLUDED.key_hash,enabled=true,submission_minute_limit=EXCLUDED.submission_minute_limit,submission_hour_limit=EXCLUDED.submission_hour_limit,submission_day_limit=EXCLUDED.submission_day_limit WHERE api_keys.prefix='rts_' AND api_keys.environment=EXCLUDED.environment`, key.ID, "http:"+key.ID, hex.EncodeToString(digest[:]), key.Budget.Minute, key.Budget.Hour, key.Budget.Day, key.Environment)
		if e != nil {
			return nil, errors.New("send API identity synchronization failed")
		}
		var hash, environment string
		if e = tx.QueryRowContext(ctx, `SELECT key_hash,environment FROM api_keys WHERE id=$1 AND prefix='rts_' AND enabled`, key.ID).Scan(&hash, &environment); e != nil || hash != hex.EncodeToString(digest[:]) || environment != key.Environment {
			return nil, errors.New("send API identity conflicts with an existing key")
		}
		if _, e = tx.ExecContext(ctx, `UPDATE api_message_streams SET enabled=false WHERE api_key_id=$1 AND name<>'normal'`, key.ID); e != nil {
			return nil, e
		}
		for _, stream := range key.Streams {
			if _, e = tx.ExecContext(ctx, `INSERT INTO api_message_streams(api_key_id,name,enabled,weight,minute_limit,hour_limit,day_limit) VALUES($1,$2,true,$3,$4,$5,$6) ON CONFLICT(api_key_id,name) DO UPDATE SET enabled=true,weight=EXCLUDED.weight,minute_limit=EXCLUDED.minute_limit,hour_limit=EXCLUDED.hour_limit,day_limit=EXCLUDED.day_limit`, key.ID, stream.Name, stream.Weight, stream.Budget.Minute, stream.Budget.Hour, stream.Budget.Day); e != nil {
				return nil, e
			}
		}
		auditData, _ := json.Marshal(map[string]any{"id": key.ID, "allowed_from": key.AllowedFrom, "budget": key.Budget, "streams": key.Streams, "environment": key.Environment})
		if _, e = tx.ExecContext(ctx, `INSERT INTO maintenance_audit(id,action,actor,data) VALUES($1,'API_SEND_CONFIGURED','startup_config',$2)`, uuid.NewString(), string(auditData)); e != nil {
			return nil, errors.New("send API configuration audit failed")
		}
		key.Token = ""
		people = append(people, &sendIdentity{key: key, digest: digest, tokens: 10, updated: time.Now()})
	}
	if e = tx.Commit(); e != nil {
		return nil, errors.New("send API identity commit failed")
	}
	slots := make(chan struct{}, 4)
	readSlots := make(chan struct{}, 4)
	readLimits := make(map[string]*sendIdentity, len(people))
	for _, p := range people {
		readLimits[p.key.ID] = &sendIdentity{tokens: 10, updated: time.Now()}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		readID := strings.TrimPrefix(r.URL.Path, "/v1/messages/")
		list := r.URL.Path == "/v1/messages" && r.Method == "GET"
		read := strings.HasPrefix(r.URL.Path, "/v1/messages/") && readID != "" && !strings.Contains(readID, "/")
		read = read || list
		if r.URL.Path != "/v1/messages" && !read {
			http.NotFound(w, r)
			return
		}
		method := "POST"
		if read {
			method = "GET"
		}
		if r.Method != method {
			if r.URL.Path == "/v1/messages" {
				w.Header().Set("Allow", "GET, POST")
			} else {
				w.Header().Set("Allow", method)
			}
			respond(w, 405, "method_not_allowed")
			return
		}
		if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			respond(w, 403, "browser_submission_forbidden")
			return
		}
		admission := slots
		if read {
			admission = readSlots
		}
		select {
		case admission <- struct{}{}:
			defer func() { <-admission }()
		default:
			w.Header().Set("Retry-After", "1")
			respond(w, 429, "busy")
			return
		}
		values := r.Header.Values("Authorization")
		if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") || len(values[0]) != 75 {
			respond(w, 401, "invalid_credentials")
			return
		}
		supplied := sha256.Sum256([]byte(strings.TrimPrefix(values[0], "Bearer ")))
		var who *sendIdentity
		for _, p := range people {
			if subtle.ConstantTimeCompare(supplied[:], p.digest[:]) == 1 {
				who = p
			}
		}
		if who == nil {
			respond(w, 401, "invalid_credentials")
			return
		}
		w.Header().Set("X-RelayTale-Environment", who.key.Environment)
		limiter := who
		if read {
			limiter = readLimits[who.key.ID]
		}
		if !limiter.acquire() {
			w.Header().Set("Retry-After", "1")
			respond(w, 429, "rate_or_concurrency_limit")
			return
		}
		defer limiter.release()
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if list {
			readMessageList(ctx, w, r, receiver.Repo.DB, who)
			return
		}
		if read {
			readMessageStatus(ctx, w, r, receiver.Repo.DB, who, readID)
			return
		}
		// Bound stalled body reads, which a context alone cannot interrupt.
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(10 * time.Second))
		var enabled bool
		e := receiver.Repo.DB.QueryRowContext(ctx, `SELECT enabled AND key_hash=$2 FROM api_keys WHERE id=$1`, who.key.ID, hex.EncodeToString(who.digest[:])).Scan(&enabled)
		if errors.Is(e, sql.ErrNoRows) || (e == nil && !enabled) {
			respond(w, 401, "invalid_credentials")
			return
		}
		if e != nil {
			respond(w, 503, "unavailable")
			return
		}
		media, params, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if len(r.Header.Values("Content-Type")) != 1 || e != nil || media != "message/rfc822" || len(params) != 0 || len(r.Header.Values("Content-Encoding")) != 0 {
			respond(w, 415, "use_message_rfc822")
			return
		}
		if r.URL.RawQuery != "" {
			respond(w, 400, "invalid_query")
			return
		}
		idem := r.Header.Values("Idempotency-Key")
		if len(idem) != 1 || len(idem[0]) < 1 || len(idem[0]) > 128 {
			respond(w, 400, "invalid_idempotency_key")
			return
		}
		for _, b := range idem[0] {
			if b < 33 || b > 126 {
				respond(w, 400, "invalid_idempotency_key")
				return
			}
		}
		streamHeaders := r.Header.Values("X-RelayTale-Stream")
		stream := "normal"
		if len(streamHeaders) > 1 || len(streamHeaders) == 1 && streamHeaders[0] == "" {
			respond(w, 400, "invalid_stream")
			return
		}
		if len(streamHeaders) == 1 {
			stream = streamHeaders[0]
		}
		if _, e := message.CanonicalStream(stream); e != nil {
			respond(w, 400, "invalid_stream")
			return
		}
		permitted := false
		for _, p := range who.key.Streams {
			if p.Name == stream {
				permitted = true
			}
		}
		if !permitted {
			respond(w, 403, "stream_not_allowed")
			return
		}
		var expiresAt time.Time
		expiryHeaders := r.Header.Values("X-RelayTale-Expires-At")
		if len(expiryHeaders) > 1 {
			respond(w, 400, "invalid_expiry")
			return
		}
		if len(expiryHeaders) == 1 {
			var expiryErr error
			expiresAt, expiryErr = time.Parse(time.RFC3339, expiryHeaders[0])
			if expiryErr != nil {
				respond(w, 400, "invalid_expiry")
				return
			}
			if _, expiryErr = message.CanonicalExpiry(expiresAt); expiryErr != nil || expiresAt.IsZero() {
				respond(w, 400, "invalid_expiry")
				return
			}
		}
		business, valid := businessHeaders(r)
		if !valid {
			respond(w, 400, "invalid_business_reference")
			return
		}
		from := r.Header.Values("X-RelayTale-From")
		to := r.Header.Values("X-RelayTale-To")
		if len(from) != 1 || len(to) < 1 || len(to) > 100 {
			respond(w, 400, "invalid_envelope")
			return
		}
		sender, e := auth.CanonicalAddress(from[0])
		if e != nil {
			respond(w, 400, "invalid_envelope")
			return
		}
		account := auth.Account{AllowedFrom: who.key.AllowedFrom}
		if !account.Allows(sender) {
			respond(w, 403, "sender_denied")
			return
		}
		recipients := make([]string, 0, len(to))
		seen := map[string]bool{}
		for _, address := range to {
			canonical, e := auth.CanonicalAddress(address)
			if e != nil || seen[canonical] {
				respond(w, 400, "invalid_recipients")
				return
			}
			seen[canonical] = true
			recipients = append(recipients, canonical)
		}
		if r.ContentLength > receiver.MaxBytes {
			respond(w, 413, "message_too_large")
			return
		}
		result, e := receiver.ReceiveExpiring(ctx, message.Envelope{Account: account, From: sender, Recipients: recipients}, io.LimitReader(r.Body, receiver.MaxBytes+1), who.key.ID, idem[0], stream, expiresAt, business)
		if e != nil {
			switch {
			case errors.Is(e, message.ErrExpiredSubmission):
				respond(w, 410, "expiry_elapsed")
			case errors.Is(e, message.ErrInvalidExpiry):
				respond(w, 422, "expiry_out_of_range")
			case errors.Is(e, storage.ErrTooLarge):
				respond(w, 413, "message_too_large")
			case errors.Is(e, message.ErrSenderDenied):
				respond(w, 403, "sender_denied")
			case errors.Is(e, message.ErrInvalidMessage), errors.Is(e, message.ErrInvalidSubmission):
				respond(w, 400, "invalid_message")
			case errors.Is(e, message.ErrStreamDenied):
				respond(w, 403, "stream_not_allowed")
			case errors.Is(e, message.ErrStreamBudget):
				w.Header().Set("Retry-After", "60")
				respond(w, 429, "stream_budget_exhausted")
			case errors.Is(e, message.ErrSubmissionBudget):
				w.Header().Set("Retry-After", "60")
				respond(w, 429, "recipient_budget_exhausted")
			case errors.Is(e, message.ErrSubmissionBatchTooLarge):
				respond(w, 422, "recipient_batch_exceeds_budget")
			case errors.Is(e, message.ErrSubmissionDisabled):
				respond(w, 401, "invalid_credentials")
			case errors.Is(e, message.ErrIdempotencyConflict):
				respond(w, 409, "idempotency_conflict")
			default:
				w.Header().Set("Retry-After", "1")
				respond(w, 503, "retry_same_request_and_key")
			}
			return
		}
		response := map[string]any{"message_id": result.MessageID, "replayed": result.Replayed, "status": "GATEWAY_ACCEPTED"}
		if who.key.Environment == "sandbox" {
			response["status"] = "SANDBOX_STORED"
			response["simulation"] = true
			response["environment"] = "sandbox"
		}
		output(w, 202, response)
	}), nil
}

func businessHeaders(r *http.Request) (message.Business, bool) {
	refs, tags := r.Header.Values("X-RelayTale-Client-Reference"), r.Header.Values("X-RelayTale-Tag")
	if len(refs) > 1 || len(refs) == 1 && refs[0] == "" {
		return message.Business{}, false
	}
	b := message.Business{Tags: tags}
	if len(refs) == 1 {
		b.ClientReference = refs[0]
	}
	canonical, err := b.Canonical()
	return canonical, err == nil
}
