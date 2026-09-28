package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestStatusQueryBounds(t *testing.T) {
	id := uuid.NewString()
	for _, q := range []string{"?limit=0", "?limit=101", "?limit=-1", "?limit=x", "?limit=1&limit=2", "?before=", "?before=bad", "?access_token=secret", "?before=" + uuid.Nil.String(), "?limit=%xx", "?x=" + strings.Repeat("a", 300)} {
		r := httptest.NewRequest("GET", "/v1/messages/"+id, nil)
		r.URL.RawQuery = strings.TrimPrefix(q, "?")
		if _, _, ok := statusPage(r, id); ok {
			t.Fatal(q)
		}
	}
	for _, edit := range []func(*http.Request){func(r *http.Request) { r.ContentLength = 1 }, func(r *http.Request) { r.ContentLength = -1 }, func(r *http.Request) { r.TransferEncoding = []string{"chunked"} }, func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }, func(r *http.Request) { r.URL.RawPath = "/encoded" }} {
		r := httptest.NewRequest("GET", "/v1/messages/"+id, nil)
		edit(r)
		if _, _, ok := statusPage(r, id); ok {
			t.Fatal("invalid request accepted")
		}
	}
	r := httptest.NewRequest("GET", "/v1/messages/"+id+"?limit=100&before="+id, nil)
	before, limit, ok := statusPage(r, id)
	if !ok || before != id || limit != 100 {
		t.Fatal(before, limit, ok)
	}
}
