package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func adminConfig() string {
	b, _ := json.Marshal([]Principal{{"reader", "viewer", strings.Repeat("v", 32)}, {"operator", "operator", strings.Repeat("o", 32)}, {"owner", "admin", strings.Repeat("a", 32)}})
	return string(b)
}
func TestManagementGuardsBeforeDatabase(t *testing.T) {
	h, err := (Management{}).Handler(adminConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, path, token, body, origin string
		want                                    int
	}{
		{"viewer preview", "POST", "/admin/v1/templates/00000000-0000-0000-0000-000000000001/drafts/notice/preview", strings.Repeat("v", 32), `{}`, "", 403},
		{"preview actor spoof", "POST", "/admin/v1/templates/00000000-0000-0000-0000-000000000001/drafts/notice/preview", strings.Repeat("o", 32), `{"actor":"owner"}`, "", 400},
		{"viewer draft source", "GET", "/admin/v1/templates/00000000-0000-0000-0000-000000000001/drafts/notice/revisions/1", strings.Repeat("v", 32), "", "", 403},
		{"operator publish", "POST", "/admin/v1/templates/00000000-0000-0000-0000-000000000001/drafts/notice/publish", strings.Repeat("o", 32), `{}`, "", 403},
		{"operator retire", "POST", "/admin/v1/templates/00000000-0000-0000-0000-000000000001/versions/notice/v/retire", strings.Repeat("o", 32), `{}`, "", 403},
		{"draft actor spoof", "POST", "/admin/v1/templates/00000000-0000-0000-0000-000000000001/drafts", strings.Repeat("o", 32), `{"actor":"owner"}`, "", 400},
		{"draft expected required", "POST", "/admin/v1/templates/00000000-0000-0000-0000-000000000001/drafts", strings.Repeat("o", 32), `{"template":{}}`, "", 400},
		{"draft revision syntax", "GET", "/admin/v1/templates/00000000-0000-0000-0000-000000000001/drafts/notice/revisions/01", strings.Repeat("a", 32), "", "", 400},
		{"retire acknowledgement", "POST", "/admin/v1/templates/00000000-0000-0000-0000-000000000001/versions/notice/v/retire", strings.Repeat("a", 32), `{"reason":"end"}`, "", 400},
		{"draft duplicate JSON", "POST", "/admin/v1/templates/00000000-0000-0000-0000-000000000001/drafts", strings.Repeat("o", 32), `{"expected_revision":0,"expected_revision":1}`, "", 400},
		{"missing", "GET", "/admin/v1/messages", "", "", "", 401},
		{"wrong", "GET", "/admin/v1/messages", "wrong", "", "", 401},
		{"viewer write", "POST", "/admin/v1/suppressions", strings.Repeat("v", 32), `{}`, "", 403},
		{"operator provider", "POST", "/admin/v1/providers", strings.Repeat("o", 32), `{}`, "", 403},
		{"browser", "GET", "/admin/v1/messages", strings.Repeat("a", 32), "", "https://example.test", 403},
		{"actor spoof", "POST", "/admin/v1/suppressions", strings.Repeat("o", 32), `{"actor":"owner"}`, "", 400},
		{"duplicate", "POST", "/admin/v1/suppressions", strings.Repeat("o", 32), `{"reason":"first","reason":"second"}`, "", 400},
		{"trailing", "POST", "/admin/v1/suppressions", strings.Repeat("o", 32), `{} {}`, "", 400},
		{"oversized", "POST", "/admin/v1/suppressions", strings.Repeat("o", 32), `{"reason":"` + strings.Repeat("x", 17000) + `"}`, "", 400},
		{"limit", "GET", "/admin/v1/messages?limit=101", strings.Repeat("v", 32), "", "", 400},
		{"query token", "GET", "/admin/v1/messages?token=secret", strings.Repeat("v", 32), "", "", 400},
		{"bad uuid", "GET", "/admin/v1/messages/nope", strings.Repeat("v", 32), "", "", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			if tc.token != "" {
				r.Header.Set("Authorization", "Bearer "+tc.token)
			}
			r.Header.Set("Origin", tc.origin)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("got %d %s", w.Code, w.Body)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("cacheable admin response")
			}
		})
	}
	disabled, _ := (Management{}).Handler("")
	w := httptest.NewRecorder()
	disabled.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/v1/messages", nil))
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
}
func TestPrincipalConfigurationRejectsInvalidAndDuplicateKeys(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, adminConfig() + `[]`, `[{"id":"a","role":"admin","token":"short"}]`, `[{"id":"a","role":"superuser","token":"` + strings.Repeat("x", 32) + `"}]`, `[{"id":"a","role":"admin","token":"` + strings.Repeat("x", 32) + `"},{"id":"b","role":"viewer","token":"` + strings.Repeat("x", 32) + `"}]`} {
		if _, err := ParsePrincipals(raw); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
}
