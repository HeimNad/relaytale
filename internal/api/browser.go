package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"relaytale/internal/auth"
)

//go:embed web/*
var webFiles embed.FS

type WebUser struct {
	ID           string `json:"id"`
	Role         string `json:"role"`
	PasswordHash string `json:"password_hash"`
}
type WebConfig struct{ Origin, Users string }

func ValidateWeb(c WebConfig) ([]WebUser, error) {
	if c.Origin == "" && c.Users == "" {
		return nil, nil
	}
	u, err := url.Parse(c.Origin)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback()))) {
		return nil, errors.New("WEB_UI_ORIGIN requires an HTTPS origin (HTTP only for loopback)")
	}
	if len(c.Users) > 32768 {
		return nil, errors.New("invalid WEB_UI_USERS")
	}
	var users []WebUser
	d := json.NewDecoder(strings.NewReader(c.Users))
	d.DisallowUnknownFields()
	if err := d.Decode(&users); err != nil {
		return nil, errors.New("invalid WEB_UI_USERS")
	}
	var extra any
	if d.Decode(&extra) != io.EOF || len(users) < 1 || len(users) > 32 {
		return nil, errors.New("invalid WEB_UI_USERS")
	}
	// Reuse the same identity/role validation without granting Bearer tokens.
	principals := []Principal{}
	for _, v := range users {
		if !auth.ValidHash(v.PasswordHash) {
			return nil, errors.New("invalid WEB_UI_USERS password hash")
		}
		principals = append(principals, Principal{v.ID, v.Role, strings.Repeat("x", 32) + v.ID})
	}
	raw, _ := json.Marshal(principals)
	if _, err := ParsePrincipals(string(raw)); err != nil {
		return nil, errors.New("invalid WEB_UI_USERS identity or role")
	}
	return users, nil
}

type browserSession struct {
	who           credential
	csrf          string
	expires, last time.Time
}
type loginSource struct {
	credits float64
	at      time.Time
}
type browserState struct {
	mu       sync.Mutex
	sessions map[[32]byte]browserSession
	sources  map[string]loginSource
	now      func() time.Time
}

func randomToken() string { return base64.RawURLEncoding.EncodeToString(randomBytes()) }
func randomBytes() []byte { b := make([]byte, 32); _, _ = rand.Read(b); return b }
func (s *browserState) admit(source string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, v := range s.sources {
		if now.Sub(v.at) > 10*time.Minute {
			delete(s.sources, k)
		}
	}
	v, ok := s.sources[source]
	if !ok {
		if len(s.sources) >= 1024 {
			return false
		}
		v = loginSource{5, now}
	}
	v.credits = min(5, v.credits+max(0, now.Sub(v.at).Seconds())/30)
	v.at = now
	allowed := v.credits >= 1
	if allowed {
		v.credits--
	}
	s.sources[source] = v
	return allowed
}
func (s *browserState) session(key [32]byte) (browserSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.sessions[key]
	now := s.now()
	if !ok || !now.Before(v.expires) || now.Sub(v.last) > 15*time.Minute {
		delete(s.sessions, key)
		return browserSession{}, false
	}
	v.last = now
	s.sessions[key] = v
	return v, true
}
func loginPeer(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if ip == nil {
		return "unknown"
	}
	if ip.To4() != nil {
		return ip.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String()
}

func (a Management) browser(c WebConfig, operations, bearer http.Handler) (http.Handler, error) {
	users, err := ValidateWeb(c)
	if err != nil {
		return nil, err
	}
	origin, _ := url.Parse(c.Origin)
	secure := origin.Scheme == "https"
	cookieName := "relaytale_session"
	if secure {
		cookieName = "__Host-relaytale_session"
	}
	state := &browserState{sessions: map[[32]byte]browserSession{}, sources: map[string]loginSource{}, now: time.Now}
	loginSlot := make(chan struct{}, 1) // one additional 64 MiB Argon2 computation per process
	setCookie := func(w http.ResponseWriter, value string, age int) {
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: value, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode, MaxAge: age})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/console/") {
			bearer.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if !strings.EqualFold(r.Host, origin.Host) {
			respond(w, 403, "origin_mismatch")
			return
		}
		write := r.Method != "GET" && r.Method != "HEAD"
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" || (write && r.Header.Get("Origin") != c.Origin) || (r.Header.Get("Origin") != "" && r.Header.Get("Origin") != c.Origin) {
			respond(w, 403, "origin_mismatch")
			return
		}
		if (r.Method == "GET" || r.Method == "HEAD") && (r.URL.Path == "/console/" || r.URL.Path == "/console/app.js" || r.URL.Path == "/console/app.css") {
			path := strings.TrimPrefix(r.URL.Path, "/console/")
			kind := "text/html; charset=utf-8"
			if path == "" {
				path = "index.html"
			} else if path == "app.js" {
				kind = "text/javascript; charset=utf-8"
			} else {
				kind = "text/css; charset=utf-8"
			}
			b, e := webFiles.ReadFile("web/" + path)
			if e != nil {
				respond(w, 503, "assets_unavailable")
				return
			}
			w.Header().Set("Content-Type", kind)
			if r.Method != "HEAD" {
				_, _ = w.Write(b)
			}
			return
		}
		if r.URL.Path == "/console/login" && r.Method == "POST" {
			if !state.admit(loginPeer(r)) {
				respond(w, 429, "login_throttled")
				return
			}
			select {
			case loginSlot <- struct{}{}:
				defer func() { <-loginSlot }()
			default:
				respond(w, 429, "login_throttled")
				return
			}
			var v struct {
				ID       string `json:"id"`
				Password string `json:"password"`
			}
			if !body(w, r, &v) {
				return
			}
			hash := users[0].PasswordHash
			who := credential{}
			found := false
			for _, user := range users {
				if user.ID == v.ID {
					hash = user.PasswordHash
					who = credential{id: user.ID, role: user.Role}
					found = true
				}
			}
			valid := auth.Verify(hash, v.Password)
			v.Password = ""
			if !valid || !found {
				respond(w, 401, "invalid_login")
				return
			}
			if r.Context().Err() != nil {
				respond(w, 503, "cancelled")
				return
			}
			token, csrf := randomToken(), randomToken()
			now := state.now()
			state.mu.Lock()
			for k, s := range state.sessions {
				if !now.Before(s.expires) || now.Sub(s.last) > 15*time.Minute {
					delete(state.sessions, k)
				}
			}
			if old, e := r.Cookie(cookieName); e == nil {
				delete(state.sessions, sha256.Sum256([]byte(old.Value)))
			}
			if len(state.sessions) >= 256 {
				state.mu.Unlock()
				respond(w, 503, "session_capacity")
				return
			}
			state.sessions[sha256.Sum256([]byte(token))] = browserSession{who, csrf, now.Add(30 * time.Minute), now}
			state.mu.Unlock()
			setCookie(w, token, 1800)
			output(w, 200, map[string]string{"id": who.id, "role": who.role, "csrf": csrf, "expires_at": now.Add(30 * time.Minute).UTC().Format(time.RFC3339)})
			return
		}
		cookie, e := r.Cookie(cookieName)
		if e != nil || len(cookie.Value) > 128 {
			respond(w, 401, "session_expired")
			return
		}
		key := sha256.Sum256([]byte(cookie.Value))
		session, ok := state.session(key)
		if !ok {
			setCookie(w, "", -1)
			respond(w, 401, "session_expired")
			return
		}
		if write && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(session.csrf)) != 1 {
			respond(w, 403, "csrf_required")
			return
		}
		if r.URL.Path == "/console/session" && r.Method == "GET" {
			output(w, 200, map[string]string{"id": session.who.id, "role": session.who.role, "csrf": session.csrf, "expires_at": session.expires.UTC().Format(time.RFC3339)})
			return
		}
		if r.URL.Path == "/console/logout" && r.Method == "POST" {
			state.mu.Lock()
			delete(state.sessions, key)
			state.mu.Unlock()
			setCookie(w, "", -1)
			output(w, 200, map[string]string{"status": "signed_out"})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/console/api/") {
			clone := r.Clone(context.WithValue(r.Context(), principalKey{}, session.who))
			u := *r.URL
			u.Path = "/admin/v1/" + strings.TrimPrefix(r.URL.Path, "/console/api/")
			u.RawPath = ""
			clone.URL = &u
			operations.ServeHTTP(w, clone)
			return
		}
		http.NotFound(w, r)
	}), nil
}
