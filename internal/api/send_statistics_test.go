package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStatisticsWindowAndEnvelopeBounds(t *testing.T) {
	valid := "from=2026-09-01T00:00:00Z&until=2026-09-02T00:00:00Z"
	for _, q := range []string{"", "from=bad&until=bad", valid + "&api_key_id=x", valid + "&from=2026-09-01T00:00:00Z", "from=2026-09-02T00:00:00Z&until=2026-09-01T00:00:00Z", "from=2026-09-01T00:00:00Z&until=2026-09-01T00:00:00Z", "from=2026-09-01T00:00:00Z&until=2026-10-03T00:00:00Z", "from=2026-09-01T00:00:00.000000001Z&until=2026-09-02T00:00:00Z", "from=%xx&until=x"} {
		r := httptest.NewRequest("GET", "/v1/statistics", nil)
		r.URL.RawQuery = q
		if _, ok := statisticsPage(r); ok {
			t.Fatal("invalid statistics query", q)
		}
	}
	for _, edit := range []func(*http.Request){func(r *http.Request) { r.ContentLength = 1 }, func(r *http.Request) { r.TransferEncoding = []string{"chunked"} }, func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }, func(r *http.Request) { r.URL.RawPath = "/encoded" }} {
		r := httptest.NewRequest("GET", "/v1/statistics?"+valid, nil)
		edit(r)
		if _, ok := statisticsPage(r); ok {
			t.Fatal("invalid request envelope")
		}
	}
	r := httptest.NewRequest("GET", "/v1/statistics?"+valid, nil)
	if window, ok := statisticsPage(r); !ok || window.Until.Sub(window.From).Hours() != 24 {
		t.Fatal(window, ok)
	}
}
