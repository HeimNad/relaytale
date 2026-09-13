package api

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"relaytale/internal/auth"
)

var testWebHash string
var hashOnce sync.Once

func webUsers(t *testing.T) string {
	t.Helper()
	hashOnce.Do(func() {
		var err error
		testWebHash, err = auth.Hash("browser-test-password-only")
		if err != nil {
			t.Fatal(err)
		}
	})
	raw, _ := json.Marshal([]WebUser{{"reader", "viewer", testWebHash}, {"owner", "admin", testWebHash}})
	return string(raw)
}
func webRequest(h http.Handler, method, path, origin string, cookie *http.Cookie, csrf, payload string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://console.example.test"+path, strings.NewReader(payload))
	r.Header.Set("Content-Type", "application/json")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestBrowserSessionAndAuthorization(t *testing.T) {
	const origin = "https://console.example.test"
	h, err := (Management{}).Handler("", WebConfig{origin, webUsers(t)})
	if err != nil {
		t.Fatal(err)
	}
	login := `{"id":"reader","password":"browser-test-password-only"}`
	for _, o := range []string{"", "https://evil.example.test"} {
		if w := webRequest(h, "POST", "/console/login", o, nil, "", login); w.Code != 403 {
			t.Fatal("login CSRF accepted", w.Code)
		}
	}
	w := webRequest(h, "POST", "/console/login", origin, nil, "", login)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var session map[string]string
	json.Unmarshal(w.Body.Bytes(), &session)
	cookie := w.Result().Cookies()[0]
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.Name != "__Host-relaytale_session" {
		t.Fatal("unsafe session cookie", cookie.Name)
	}
	if strings.Contains(w.Body.String(), cookie.Value) || strings.Contains(w.Body.String(), "password") {
		t.Fatal("session credential exposed")
	}
	for _, tc := range []struct {
		method, path, origin, csrf string
		want                       int
	}{
		{"GET", "/console/session", "", "", 200},
		{"GET", "/console/api/messages?limit=101", "", "", 400},
		{"POST", "/console/api/suppressions", origin, "", 403},
		{"POST", "/console/api/suppressions", origin, session["csrf"], 403},
		{"POST", "/console/logout", "https://evil.example.test", session["csrf"], 403},
		{"GET", "/admin/v1/messages", "", "", 401},
	} {
		got := webRequest(h, tc.method, tc.path, tc.origin, cookie, tc.csrf, `{}`)
		if got.Code != tc.want {
			t.Fatalf("%s want %d got %d", tc.path, tc.want, got.Code)
		}
	}
	w = webRequest(h, "POST", "/console/login", origin, cookie, "", login)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	// A fresh login rotates the old session, never reuses attacker-supplied IDs.
	if got := webRequest(h, "GET", "/console/session", "", cookie, "", ""); got.Code != 401 {
		t.Fatal("old session survives login")
	}
	cookie = w.Result().Cookies()[0]
	json.Unmarshal(w.Body.Bytes(), &session)
	if got := webRequest(h, "POST", "/console/logout", origin, cookie, session["csrf"], `{}`); got.Code != 200 {
		t.Fatal(got.Code)
	}
	if got := webRequest(h, "GET", "/console/session", "", cookie, "", ""); got.Code != 401 {
		t.Fatal("logout did not revoke")
	}
}
func TestBrowserExpiryThrottleAndIsolation(t *testing.T) {
	now := time.Now()
	s := &browserState{sessions: map[[32]byte]browserSession{}, sources: map[string]loginSource{}, now: func() time.Time { return now }}
	key := sha256.Sum256([]byte("test-session"))
	s.sessions[key] = browserSession{expires: now.Add(30 * time.Minute), last: now}
	now = now.Add(16 * time.Minute)
	if _, ok := s.session(key); ok {
		t.Fatal("idle expiry ignored")
	}
	s.sessions[key] = browserSession{expires: now, last: now}
	if _, ok := s.session(key); ok {
		t.Fatal("absolute expiry ignored")
	}
	for i := 0; i < 5; i++ {
		if !s.admit("ip") {
			t.Fatal("early limit")
		}
	}
	if s.admit("ip") {
		t.Fatal("unlimited login attempts")
	}
	now = now.Add(30 * time.Second)
	if !s.admit("ip") {
		t.Fatal("no recovery")
	}
	for i := 0; i < 1024; i++ {
		s.sources[string(rune(i))] = loginSource{at: now}
	}
	if s.admit("new-source") {
		t.Fatal("unbounded source cache")
	}
	h, err := (Management{}).Handler("", WebConfig{"https://console.example.test", webUsers(t)})
	if err != nil {
		t.Fatal(err)
	}
	w := webRequest(h, "GET", "/console/", "", nil, "", "")
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("missing browser isolation")
	}
	r := httptest.NewRequest("GET", "https://evil.example.test/console/", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("untrusted host accepted")
	}
	for _, origin := range []string{"http://public.example.test", "https://console.example.test/path", "https://name:password@console.example.test"} {
		if _, err := ValidateWeb(WebConfig{origin, webUsers(t)}); err == nil {
			t.Fatal("invalid origin", origin)
		}
	}
}
