package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBusinessQueryAndHeaderBounds(t *testing.T) {
	for _, q := range []string{"", "client_reference=", "tag=", "tag=A", "tag=a&tag=b", "client_reference=one&client_reference=two", "client_reference=x&before=bad", "tag=a&limit=101", "tag=a&limit=0", "tag=a&project_id=x", "tag=a&access_token=x", "tag=%xx", "tag=a&schema_version=2", "client_reference=" + strings.Repeat("a", 129)} {
		r := httptest.NewRequest("GET", "/v1/messages", nil)
		r.URL.RawQuery = q
		if _, ok := searchPage(r); ok {
			t.Fatal(q)
		}
	}
	for _, edit := range []func(*http.Request){func(r *http.Request) { r.ContentLength = 1 }, func(r *http.Request) { r.TransferEncoding = []string{"chunked"} }, func(r *http.Request) { r.URL.RawPath = "/encoded" }, func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }} {
		r := httptest.NewRequest("GET", "/v1/messages?tag=order", nil)
		edit(r)
		if _, ok := searchPage(r); ok {
			t.Fatal("invalid read accepted")
		}
	}
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Header.Add("X-RelayTale-Client-Reference", "order:1")
	r.Header.Add("X-RelayTale-Tag", "z")
	r.Header.Add("X-RelayTale-Tag", "a")
	if b, ok := businessHeaders(r); !ok || b.Tags[0] != "a" {
		t.Fatal(b, ok)
	}
	r.Header.Add("X-RelayTale-Client-Reference", "order:2")
	if _, ok := businessHeaders(r); ok {
		t.Fatal("duplicate reference")
	}
	id := "11111111-1111-4111-8111-111111111111"
	for _, v := range []string{"0", "3", "", "2&schema_version=2"} {
		r := httptest.NewRequest("GET", "/v1/messages/"+id+"?schema_version="+v, nil)
		if _, _, ok := statusPage(r, id); ok {
			t.Fatal(v)
		}
	}
}
