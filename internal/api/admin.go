package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"relaytale/internal/encryption"
	"relaytale/internal/provider"
	"relaytale/internal/queue"
	"relaytale/internal/suppression"
)

type Principal struct {
	ID    string `json:"id"`
	Role  string `json:"role"`
	Token string `json:"token"`
}
type credential struct {
	id, role string
	hash     [32]byte
}
type actorKey struct{}

// ParsePrincipals is also used during startup validation. Tokens are environment
// only, distinct from metrics/SMTP credentials, and never accepted in URLs.
func ParsePrincipals(raw string) ([]Principal, error) {
	if raw == "" {
		return nil, nil
	}
	if len(raw) > 32768 {
		return nil, errors.New("invalid ADMIN_API_KEYS")
	}
	var out []Principal
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&out); err != nil {
		return nil, errors.New("invalid ADMIN_API_KEYS")
	}
	var extra any
	if d.Decode(&extra) != io.EOF || len(out) < 1 || len(out) > 32 {
		return nil, errors.New("invalid ADMIN_API_KEYS")
	}
	ids, tokens := map[string]bool{}, map[string]bool{}
	for _, p := range out {
		if len(p.ID) < 1 || len(p.ID) > 128 || strings.IndexFunc(p.ID, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r))
		}) >= 0 || (p.Role != "viewer" && p.Role != "operator" && p.Role != "admin") || len(p.Token) < 32 || len(p.Token) > 512 || strings.IndexFunc(p.Token, func(r rune) bool { return r < 33 || r > 126 }) >= 0 || ids[p.ID] || tokens[p.Token] {
			return nil, errors.New("invalid ADMIN_API_KEYS")
		}
		ids[p.ID] = true
		tokens[p.Token] = true
	}
	return out, nil
}

type Management struct {
	DB  *sql.DB
	Box *encryption.Box
}

func (a Management) Handler(raw string, web ...WebConfig) (http.Handler, error) {
	principals, err := ParsePrincipals(raw)
	if err != nil {
		return nil, err
	}
	creds := make([]credential, 0, len(principals))
	for _, p := range principals {
		creds = append(creds, credential{p.ID, p.Role, sha256.Sum256([]byte(p.Token))})
	}
	operations := a.operations()
	bearer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || len(token) > 512 || len(r.Header.Values("Authorization")) != 1 {
			respond(w, 401, "unauthorized")
			return
		}
		hash := sha256.Sum256([]byte(token))
		var who credential
		found := false
		for _, c := range creds {
			if subtle.ConstantTimeCompare(hash[:], c.hash[:]) == 1 {
				who = c
				found = true
			}
		}
		if !found {
			respond(w, 401, "unauthorized")
			return
		}
		// This first API is for non-browser clients: no ambient cookie auth/CORS.
		if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			respond(w, 403, "browser_origin_not_supported")
			return
		}
		operations.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, who)))
	})
	if len(web) > 0 {
		if _, err := ValidateWeb(web[0]); err != nil {
			return nil, err
		}
	}
	if len(web) > 0 && web[0].Origin != "" {
		return a.browser(web[0], operations, bearer)
	}
	if len(principals) == 0 {
		return http.NotFoundHandler(), nil
	}
	return bearer, nil
}

type principalKey struct{}

func (a Management) operations() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/v1/providers", a.providers)
	mux.HandleFunc("GET /admin/v1/providers/{id}", a.provider)
	mux.HandleFunc("POST /admin/v1/providers", a.createProvider)
	mux.HandleFunc("PUT /admin/v1/providers/{id}", a.configureProvider)
	mux.HandleFunc("POST /admin/v1/providers/{id}/credentials", a.rotateProvider)
	mux.HandleFunc("GET /admin/v1/messages", a.messages)
	mux.HandleFunc("GET /admin/v1/messages/{id}", a.message)
	mux.HandleFunc("GET /admin/v1/messages/{id}/recipients", a.recipients)
	mux.HandleFunc("GET /admin/v1/messages/{id}/attempts", a.attempts)
	mux.HandleFunc("GET /admin/v1/messages/{id}/events", a.events)
	mux.HandleFunc("GET /admin/v1/suppressions", a.suppressions)
	mux.HandleFunc("POST /admin/v1/suppressions", a.addSuppression)
	mux.HandleFunc("POST /admin/v1/suppressions/{id}/release", a.releaseSuppression)
	mux.HandleFunc("POST /admin/v1/recipients/{id}/resolve-unknown", a.resolve)
	slots := make(chan struct{}, 8)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who := r.Context().Value(principalKey{}).(credential)
		if r.Method != "GET" && r.Method != "HEAD" {
			if who.role == "viewer" || (strings.HasPrefix(r.URL.Path, "/admin/v1/providers") && who.role != "admin") {
				respond(w, 403, "forbidden")
				return
			}
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			respond(w, 503, "busy")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		mux.ServeHTTP(w, r.WithContext(context.WithValue(ctx, actorKey{}, who.id)))
	})
}
func actor(r *http.Request) string { return r.Context().Value(actorKey{}).(string) }
func output(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func body(w http.ResponseWriter, r *http.Request, v any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		respond(w, 415, "json_required")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	raw, err := io.ReadAll(r.Body)
	if err == nil && !utf8.Valid(raw) {
		err = errors.New("invalid UTF-8")
	}
	if err == nil {
		err = uniqueJSON(json.NewDecoder(bytes.NewReader(raw)))
	}
	if err == nil {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		err = d.Decode(v)
	}
	if err != nil {
		respond(w, 400, "invalid_json")
		return false
	}
	return true
}
func pathID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		respond(w, 400, "invalid_id")
		return "", false
	}
	return id, true
}
func failure(w http.ResponseWriter, err error) {
	var pe *pgconn.PgError
	switch {
	case errors.Is(err, sql.ErrNoRows):
		respond(w, 404, "not_found")
	case errors.Is(err, provider.ErrInvalid):
		respond(w, 400, "invalid_request")
	case errors.Is(err, provider.ErrConflict), errors.Is(err, suppression.ErrActive), errors.Is(err, suppression.ErrStale), errors.Is(err, suppression.ErrBlocked), errors.Is(err, queue.ErrStaleResolution):
		respond(w, 409, "state_conflict")
	case errors.As(err, &pe) && pe.Code == "23505":
		respond(w, 409, "state_conflict")
	default:
		respond(w, 503, "operation_failed")
	}
}
func page(w http.ResponseWriter, r *http.Request) (string, int, bool) {
	q := r.URL.Query()
	for k, v := range q {
		if (k != "after" && k != "limit") || len(v) != 1 {
			respond(w, 400, "invalid_query")
			return "", 0, false
		}
	}
	after := q.Get("after")
	if after == "" {
		after = uuid.Nil.String()
	}
	if _, err := uuid.Parse(after); err != nil {
		respond(w, 400, "invalid_cursor")
		return "", 0, false
	}
	n := 50
	if s := q.Get("limit"); s != "" {
		var err error
		n, err = strconv.Atoi(s)
		if err != nil {
			n = 0
		}
	}
	if n < 1 || n > 100 {
		respond(w, 400, "invalid_limit")
		return "", 0, false
	}
	return after, n, true
}

// Queries below select explicit public fields. Never serialize DB rows wholesale.
func (a Management) list(w http.ResponseWriter, r *http.Request, query string, args ...any) {
	after, n, ok := page(w, r)
	if !ok {
		return
	}
	args = append([]any{after, n + 1}, args...)
	rows, err := a.DB.QueryContext(r.Context(), query, args...)
	if err != nil {
		failure(w, err)
		return
	}
	defer rows.Close()
	items := []json.RawMessage{}
	last := ""
	more := false
	for rows.Next() {
		if len(items) == n {
			more = true
			break
		}
		var id string
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			failure(w, err)
			return
		}
		items = append(items, json.RawMessage(raw))
		last = id
	}
	if err = rows.Err(); err != nil {
		failure(w, err)
		return
	}
	output(w, 200, map[string]any{"items": items, "next_after": last, "has_more": more})
}
func (a Management) one(w http.ResponseWriter, r *http.Request, query string) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var raw []byte
	if err := a.DB.QueryRowContext(r.Context(), query, id).Scan(&raw); err != nil {
		failure(w, err)
		return
	}
	output(w, 200, json.RawMessage(raw))
}

// Reject duplicate object keys and trailing JSON before typed decoding so a
// proxy, audit client and handler cannot interpret different write requests.
func uniqueJSON(d *json.Decoder) error {
	var value func(int) error
	value = func(depth int) error {
		if depth > 16 {
			return errors.New("JSON nesting too deep")
		}
		tok, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				k, ok := key.(string)
				k = strings.ToLower(k)
				if !ok || seen[k] {
					return errors.New("duplicate JSON key")
				}
				seen[k] = true
				if err = value(depth + 1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return errors.New("invalid JSON object")
			}
		case '[':
			for d.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return errors.New("invalid JSON array")
			}
		default:
			return errors.New("invalid JSON value")
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
