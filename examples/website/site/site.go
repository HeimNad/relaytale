// Package site is a small sandbox-only reference backend, not a production SDK.
package site

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"relaytale/internal/api"
	"relaytale/internal/webhook"
)

var ErrConflict = errors.New("idempotency conflict")
var ErrInvalid = errors.New("invalid submission")
var ErrUnavailable = errors.New("gateway unavailable; retry the exact request and key")

type Config struct {
	DB                                  *sql.DB
	GatewayURL, GatewayToken, DemoToken string
	Keys                                []webhook.Key
}
type Site struct {
	config Config
	client *http.Client
}
type Submission struct {
	From string   `json:"from"`
	To   []string `json:"to"`
	MIME string   `json:"mime"`
}
type Record struct {
	ID           string          `json:"id"`
	MessageID    *string         `json:"message_id"`
	NeedsRefresh bool            `json:"needs_refresh"`
	Snapshot     json.RawMessage `json:"snapshot"`
}

const schema = `CREATE TABLE IF NOT EXISTS relaytale_demo_submissions (
 id UUID PRIMARY KEY,key_sha256 TEXT NOT NULL UNIQUE,request_sha256 TEXT NOT NULL,request JSONB NOT NULL,
 message_id UUID UNIQUE,revision BIGINT NOT NULL DEFAULT 0,needs_refresh BOOLEAN NOT NULL DEFAULT true,
 snapshot JSONB NOT NULL DEFAULT '{}'::jsonb,snapshot_at TIMESTAMPTZ,created_at TIMESTAMPTZ NOT NULL DEFAULT now());
 CREATE TABLE IF NOT EXISTS relaytale_demo_inbox (
 event_id UUID PRIMARY KEY,submission_id UUID NOT NULL REFERENCES relaytale_demo_submissions(id),
 message_id UUID NOT NULL,payload_sha256 TEXT NOT NULL,sequence BIGINT NOT NULL,
 received_at TIMESTAMPTZ NOT NULL DEFAULT now());`

func New(ctx context.Context, c Config) (*Site, error) {
	u, err := url.Parse(c.GatewayURL)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Hostname() == "" || u.Path != "" && u.Path != "/" {
		return nil, errors.New("invalid gateway URL")
	}
	ip := net.ParseIP(u.Hostname())
	local := u.Hostname() == "localhost" || ip != nil && ip.IsLoopback()
	if u.Scheme != "https" && !(u.Scheme == "http" && local) || c.DB == nil || !strings.HasPrefix(c.GatewayToken, "rts_") || len(c.GatewayToken) != 68 || len(c.DemoToken) < 32 || len(c.Keys) < 1 || len(c.Keys) > 2 {
		return nil, errors.New("explicit sandbox credentials and verification keys required")
	}
	endpoint := webhook.Endpoint{ID: uuid.NewString(), APIKeyID: uuid.NewString(), URL: "https://example.com/hook", Keys: c.Keys, SchemaVersion: 2}
	raw, _ := json.Marshal([]webhook.Endpoint{endpoint})
	if _, err = webhook.Parse(string(raw)); err != nil {
		return nil, errors.New("invalid verification keys")
	}
	if _, err = c.DB.ExecContext(ctx, schema); err != nil {
		return nil, err
	}
	c.GatewayURL = strings.TrimSuffix(c.GatewayURL, "/")
	return &Site{config: c, client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func digest(raw []byte) string { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }
func validKey(key string) bool {
	if len(key) < 1 || len(key) > 128 {
		return false
	}
	for _, c := range key {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}
func (s *Site) gateway(ctx context.Context, method, path string, raw []byte, headers http.Header) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.config.GatewayURL+path, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, ErrUnavailable
	}
	req.Header = headers.Clone()
	req.Header.Set("Authorization", "Bearer "+s.config.GatewayToken)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, ErrUnavailable
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return nil, resp.StatusCode, ErrUnavailable
	}
	if resp.Header.Get("X-RelayTale-Environment") != "sandbox" {
		return nil, resp.StatusCode, ErrUnavailable
	}
	return body, resp.StatusCode, nil
}

// Submit persists immutable bytes before the first external operation. Repeating
// the key reuses the same gateway key, MIME and generated business reference.
func (s *Site) Submit(ctx context.Context, key string, v Submission) (Record, error) {
	if !validKey(key) || len(v.MIME) < 1 || len(v.MIME) > 2<<20 || len(v.To) < 1 || len(v.To) > 100 || len(v.From) > 254 {
		return Record{}, ErrInvalid
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return Record{}, err
	}
	proposed := uuid.NewString()
	_, err = s.config.DB.ExecContext(ctx, `INSERT INTO relaytale_demo_submissions(id,key_sha256,request_sha256,request) VALUES($1,$2,$3,$4) ON CONFLICT(key_sha256) DO NOTHING`, proposed, digest([]byte(key)), digest(raw), string(raw))
	if err != nil {
		return Record{}, err
	}
	var id, storedHash string
	if err = s.config.DB.QueryRowContext(ctx, `SELECT id,request_sha256 FROM relaytale_demo_submissions WHERE key_sha256=$1`, digest([]byte(key))).Scan(&id, &storedHash); err != nil {
		return Record{}, err
	}
	if storedHash != digest(raw) {
		return Record{}, ErrConflict
	}
	// An immutable credential's environment is checked BEFORE posting mail. A
	// production credential is refused; checking only the POST response is too late.
	_, code, err := s.gateway(ctx, "GET", "/v1/messages?client_reference=relaytale-demo-preflight", nil, http.Header{})
	if err != nil || code != 200 {
		return Record{}, ErrUnavailable
	}
	headers := http.Header{"Content-Type": {"message/rfc822"}, "Idempotency-Key": {key}, "X-RelayTale-From": {v.From}, "X-RelayTale-To": v.To, "X-RelayTale-Client-Reference": {id}}
	body, code, err := s.gateway(ctx, "POST", "/v1/messages", []byte(v.MIME), headers)
	if err != nil || code != 202 {
		return Record{}, ErrUnavailable
	}
	var result struct {
		MessageID  string `json:"message_id"`
		Simulation bool   `json:"simulation"`
		Status     string `json:"status"`
	}
	if json.Unmarshal(body, &result) != nil || !canonical(result.MessageID) || !result.Simulation || result.Status != "SANDBOX_STORED" {
		return Record{}, ErrUnavailable
	}
	res, err := s.config.DB.ExecContext(ctx, `UPDATE relaytale_demo_submissions SET message_id=$2 WHERE id=$1 AND (message_id IS NULL OR message_id=$2)`, id, result.MessageID)
	if err != nil {
		return Record{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return Record{}, ErrConflict
	}
	return s.Get(ctx, id)
}
func canonical(id string) bool {
	u, e := uuid.Parse(id)
	return e == nil && u != uuid.Nil && u.String() == id
}
func (s *Site) Get(ctx context.Context, id string) (Record, error) {
	var out Record
	if !canonical(id) {
		return out, ErrInvalid
	}
	err := s.config.DB.QueryRowContext(ctx, `SELECT id,message_id,needs_refresh,snapshot FROM relaytale_demo_submissions WHERE id=$1`, id).Scan(&out.ID, &out.MessageID, &out.NeedsRefresh, &out.Snapshot)
	return out, err
}

// Reconcile never treats notification ordering as delivery-state ordering. The
// persisted revision fences callbacks arriving while the status read is running;
// snapshot_at also prevents concurrent older reads overwriting newer snapshots.
func (s *Site) Reconcile(ctx context.Context, id string) error {
	var mid sql.NullString
	var revision int64
	if err := s.config.DB.QueryRowContext(ctx, `SELECT message_id,revision FROM relaytale_demo_submissions WHERE id=$1`, id).Scan(&mid, &revision); err != nil {
		return err
	}
	if !mid.Valid {
		return nil
	}
	body, code, err := s.gateway(ctx, "GET", "/v1/messages/"+mid.String+"?schema_version=2", nil, http.Header{})
	if err != nil || code != 200 {
		return ErrUnavailable
	}
	var status api.MessageStatus
	if json.Unmarshal(body, &status) != nil || status.MessageID != mid.String || !status.Simulation || status.Environment != "sandbox" || status.SchemaVersion != 2 || status.Business == nil || status.Business.ClientReference != id || status.SnapshotAt.IsZero() {
		return ErrUnavailable
	}
	_, err = s.config.DB.ExecContext(ctx, `UPDATE relaytale_demo_submissions SET snapshot=$2,snapshot_at=$3,needs_refresh=false WHERE id=$1 AND revision=$4 AND (snapshot_at IS NULL OR snapshot_at<=$3)`, id, string(body), status.SnapshotAt, revision)
	return err
}
func (s *Site) ReconcilePending(ctx context.Context) error {
	rows, err := s.config.DB.QueryContext(ctx, `SELECT id FROM relaytale_demo_submissions WHERE needs_refresh AND message_id IS NOT NULL ORDER BY created_at LIMIT 100`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var failures []error
	for _, id := range ids {
		if err = s.Reconcile(ctx, id); err != nil {
			failures = append(failures, err)
		}
		if ctx.Err() != nil {
			break
		}
	}
	return errors.Join(failures...)
}

type notification struct {
	SchemaVersion        int       `json:"schema_version"`
	EventID              string    `json:"event_id"`
	Type                 string    `json:"type"`
	MessageID            string    `json:"message_id"`
	RecipientID          *string   `json:"recipient_id"`
	AttemptID            *string   `json:"attempt_id"`
	OccurredAt           time.Time `json:"occurred_at"`
	Sequence             int64     `json:"sequence"`
	FeedbackAvailability string    `json:"feedback_availability"`
	Environment          string    `json:"environment"`
	Simulation           bool      `json:"simulation"`
	Business             struct {
		ClientReference string   `json:"client_reference"`
		Tags            []string `json:"tags"`
	} `json:"business"`
}

func knownNotice(kind string) bool {
	switch kind {
	case "SANDBOX_STORED", "SANDBOX_SIMULATED_ACCEPTED", "SANDBOX_SIMULATED_TEMP_FAILED", "SANDBOX_SIMULATED_PERM_FAILED", "SANDBOX_SIMULATED_UNKNOWN", "SANDBOX_SIMULATED_BOUNCE":
		return true
	}
	return false
}
func (s *Site) receive(ctx context.Context, r *http.Request) error {
	if len(r.Header.Values("X-RelayTale-Timestamp")) != 1 || len(r.Header.Values("X-RelayTale-Signature")) != 1 || len(r.Header.Values("Content-Encoding")) != 0 {
		return ErrInvalid
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	if err != nil || len(raw) > 4096 {
		return ErrInvalid
	}
	verified := false
	for _, key := range s.config.Keys {
		if webhook.Verify(key, r.Header.Get("X-RelayTale-Timestamp"), r.Header.Get("X-RelayTale-Signature"), raw, time.Now()) {
			verified = true
		}
	}
	if !verified {
		return ErrInvalid
	}
	var v notification
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&v) != nil || d.Decode(new(any)) != io.EOF || v.SchemaVersion != 2 || !canonical(v.EventID) || !canonical(v.MessageID) || !canonical(v.Business.ClientReference) || v.Sequence < 1 || !v.Simulation || v.Environment != "sandbox" || !knownNotice(v.Type) || v.AttemptID != nil || v.OccurredAt.IsZero() || v.RecipientID != nil && !canonical(*v.RecipientID) || v.FeedbackAvailability != "NOT_EXPOSED" {
		return ErrInvalid
	}
	tx, err := s.config.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return err
	}
	var current sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT message_id FROM relaytale_demo_submissions WHERE id=$1 FOR UPDATE`, v.Business.ClientReference).Scan(&current)
	if err != nil {
		return err
	}
	if current.Valid && current.String != v.MessageID {
		return ErrConflict
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO relaytale_demo_inbox(event_id,submission_id,message_id,payload_sha256,sequence) VALUES($1,$2,$3,$4,$5) ON CONFLICT(event_id) DO NOTHING`, v.EventID, v.Business.ClientReference, v.MessageID, digest(raw), v.Sequence)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		var hash string
		if err = tx.QueryRowContext(ctx, `SELECT payload_sha256 FROM relaytale_demo_inbox WHERE event_id=$1`, v.EventID).Scan(&hash); err != nil {
			return err
		}
		if hash != digest(raw) {
			return ErrConflict
		}
		return nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE relaytale_demo_submissions SET message_id=$2,revision=revision+1,needs_refresh=true WHERE id=$1`, v.Business.ClientReference, v.MessageID)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if r.URL.RawQuery != "" || r.URL.RawPath != "" {
		reply(w, 400, map[string]string{"error": "invalid_request"})
		return
	}
	if r.URL.Path == "/relaytale/hooks" {
		if r.Method != "POST" {
			w.Header().Set("Allow", "POST")
			reply(w, 405, map[string]string{"error": "method_not_allowed"})
			return
		}
		media, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" || len(params) > 0 || len(r.Header.Values("Content-Type")) != 1 {
			reply(w, 415, map[string]string{"error": "use_application_json"})
			return
		}
		err = s.receive(ctx, r)
		if err != nil {
			code := 503
			if errors.Is(err, ErrInvalid) {
				code = 400
			}
			if errors.Is(err, ErrConflict) {
				code = 409
			}
			reply(w, code, map[string]string{"error": "notification_not_committed"})
			return
		}
		w.WriteHeader(204)
		return
	}
	supplied := sha256.Sum256([]byte(r.Header.Get("Authorization")))
	expected := sha256.Sum256([]byte("Bearer " + s.config.DemoToken))
	if len(r.Header.Values("Authorization")) != 1 || subtle.ConstantTimeCompare(supplied[:], expected[:]) != 1 {
		reply(w, 401, map[string]string{"error": "invalid_credentials"})
		return
	}
	if r.Header.Get("Origin") != "" {
		reply(w, 403, map[string]string{"error": "server_only"})
		return
	}
	if r.URL.Path == "/demo/submissions" && r.Method == "POST" {
		media, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" || len(params) > 0 || len(r.Header.Values("Content-Type")) != 1 {
			reply(w, 415, map[string]string{"error": "use_application_json"})
			return
		}
		var v Submission
		raw, err := io.ReadAll(io.LimitReader(r.Body, (3<<20)+1))
		if err != nil || len(raw) > 3<<20 {
			reply(w, 413, map[string]string{"error": "request_too_large"})
			return
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if d.Decode(&v) != nil || d.Decode(new(any)) != io.EOF || len(r.Header.Values("Idempotency-Key")) != 1 {
			reply(w, 400, map[string]string{"error": "invalid_request"})
			return
		}
		result, err := s.Submit(ctx, r.Header.Get("Idempotency-Key"), v)
		if err != nil {
			code := 503
			if errors.Is(err, ErrConflict) {
				code = 409
			}
			if errors.Is(err, ErrInvalid) {
				code = 400
			}
			reply(w, code, map[string]string{"error": "submission_not_confirmed_retry_same_key"})
			return
		}
		reply(w, 202, result)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/demo/submissions/")
	if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/demo/submissions/") && canonical(id) {
		out, err := s.Get(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			reply(w, 404, map[string]string{"error": "not_found"})
			return
		}
		if err != nil {
			reply(w, 503, map[string]string{"error": "unavailable"})
			return
		}
		reply(w, 200, out)
		return
	}
	http.NotFound(w, r)
}
func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
