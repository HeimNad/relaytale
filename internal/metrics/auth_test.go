package metrics

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBearerAuthorizationBeforeDatabaseAccess(t *testing.T) {
	h := &Handler{Token: strings.Repeat("a", 32)} // nil DB proves unauthorized requests never collect.
	for _, value := range []string{"", "Basic a", "Bearer wrong", "Bearer " + h.Token + " extra"} {
		r := httptest.NewRequest("GET", "/metrics", nil)
		r.Header.Set("Authorization", value)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 || strings.Contains(w.Body.String(), h.Token) {
			t.Fatal("authorization leak/bypass")
		}
	}
	// A valid token reaches collection: hold the scrape lock to avoid requiring DB.
	h.mu.Lock()
	defer h.mu.Unlock()
	r := httptest.NewRequest("GET", "/metrics", nil).WithContext(context.Background())
	r.Header.Set("Authorization", "Bearer "+h.Token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal("valid token rejected")
	}
}
