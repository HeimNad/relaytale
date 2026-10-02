package webhook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func testEndpoint() Endpoint {
	return Endpoint{ID: "11111111-1111-4111-8111-111111111111", APIKeyID: "22222222-2222-4222-8222-222222222222", URL: "https://example.com/hook", Keys: []Key{{ID: "current", Secret: strings.Repeat("ab", 32)}, {ID: "previous", Secret: strings.Repeat("cd", 32)}}}
}
func TestSignVerifyRotation(t *testing.T) {
	e := testEndpoint()
	at := time.Now().Truncate(time.Second)
	body := []byte(`{"event_id":"stable"}`)
	stamp, sig := Sign(e.Keys, at, body)
	for _, k := range e.Keys {
		if !Verify(k, stamp, sig, body, at) {
			t.Fatal("signature failed")
		}
	}
	for _, tc := range []struct {
		stamp, sig string
		body       []byte
		now        time.Time
	}{
		{stamp, sig, []byte(`{"event_id":"other"}`), at}, {stamp, sig, body, at.Add(6 * time.Minute)}, {stamp, sig, body, at.Add(-6 * time.Minute)}, {"0" + stamp, sig, body, at}, {stamp, sig + strings.Repeat("x", 300), body, at}, {stamp, "current:" + strings.Repeat("0", 64), body, at},
	} {
		if Verify(e.Keys[0], tc.stamp, tc.sig, tc.body, tc.now) {
			t.Fatal("tamper/replay accepted")
		}
	}
}
func TestPublicAddresses(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "100.100.100.200", "192.168.1.1", "198.19.0.1", "0.0.0.0", "224.0.0.1", "::1", "::ffff:127.0.0.1", "64:ff9b::a00:1", "2002:a00:1::", "2001:db8::1", "fd00::1", "fe80::1", "ff02::1", "3fff::1"} {
		if publicIP(netip.MustParseAddr(s)) {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if !publicIP(netip.MustParseAddr(s)) {
			t.Fatal(s)
		}
	}
	for _, raw := range []string{"http://example.com/hook", "https://user:pass@example.com", "https://example.com:8443/hook", "https://example.com?token=secret", "https://example.com/#fragment", "https://example.com/with space"} {
		if ValidateURL(raw) == nil {
			t.Fatal(raw)
		}
	}
	_, err := (HTTPSender{}).Send(context.Background(), Endpoint{URL: "https://127.0.0.1/hook"}, []byte(`{}`))
	if err == nil {
		t.Fatal("loopback accepted")
	}
}
func TestHTTPSWireAndBounds(t *testing.T) {
	e := testEndpoint()
	body := []byte(`{"event_id":"stable","schema_version":1}`)
	var mode string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || !Verify(e.Keys[0], r.Header.Get("X-RelayTale-Timestamp"), r.Header.Get("X-RelayTale-Signature"), b, time.Now()) || string(b) != string(body) {
			t.Error("wire signature mismatch")
		}
		switch mode {
		case "large":
			w.Write([]byte(strings.Repeat("x", 4097)))
		case "redirect":
			w.Header().Set("Location", "https://127.0.0.1/secret")
			w.WriteHeader(302)
		default:
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	e.URL = server.URL
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	code, err := sendHTTP(context.Background(), client, e, body)
	if err != nil || code != 204 {
		t.Fatal(code, err)
	}
	mode = "large"
	if _, err = sendHTTP(context.Background(), client, e, body); err == nil {
		t.Fatal("oversized response accepted")
	}
	mode = "redirect"
	code, err = sendHTTP(context.Background(), client, e, body)
	if err != nil || code != 302 {
		t.Fatal(code, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = sendHTTP(ctx, client, e, body); err == nil {
		t.Fatal("cancellation ignored")
	}
	// Default client must not trust the synthetic certificate.
	if _, err = sendHTTP(context.Background(), &http.Client{Timeout: time.Second}, e, body); err == nil {
		t.Fatal("untrusted TLS accepted")
	}
}
func TestConfiguration(t *testing.T) {
	e := testEndpoint()
	raw, _ := json.Marshal([]Endpoint{e})
	if got, err := Parse(string(raw)); err != nil || len(got) != 1 {
		t.Fatal(err)
	}
	for _, raw := range []string{`[{"id":"x","id":"y"}]`, string(raw) + `{}`, `[{"unknown":true}]`, strings.Repeat("x", 65537)} {
		if _, err := Parse(raw); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
	duplicate, _ := json.Marshal([]Endpoint{e, e})
	if _, err := Parse(string(duplicate)); err == nil {
		t.Fatal("duplicate owner accepted")
	}
	if got, err := Parse(""); err != nil || len(got) != 0 {
		t.Fatal("default enabled")
	}
}

func TestDNSPinningAndRebinding(t *testing.T) {
	answers := []netip.Addr{netip.MustParseAddr("8.8.8.8")}
	lookups, dials := 0, 0
	lookup := func(context.Context, string, string) ([]netip.Addr, error) { lookups++; return answers, nil }
	dial := func(_ context.Context, network, address string) (net.Conn, error) {
		dials++
		if address != "8.8.8.8:443" || network != "tcp" {
			t.Fatal("unpinned address", address)
		}
		a, b := net.Pipe()
		b.Close()
		return a, nil
	}
	c, err := safeDial(context.Background(), "example.com:443", "example.com", lookup, dial)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	answers = append(answers, netip.MustParseAddr("127.0.0.1"))
	if _, err = safeDial(context.Background(), "example.com:443", "example.com", lookup, dial); err == nil {
		t.Fatal("mixed public/private answer accepted")
	}
	answers = []netip.Addr{netip.MustParseAddr("169.254.169.254")}
	if _, err = safeDial(context.Background(), "example.com:443", "example.com", lookup, dial); err == nil {
		t.Fatal("rebound address accepted")
	}
	if dials != 1 || lookups != 3 {
		t.Fatal(dials, lookups)
	}
}

func TestBusinessSchemaConfigurationCompatibility(t *testing.T) {
	e := testEndpoint()
	raw, _ := json.Marshal(struct {
		ID       string `json:"id"`
		APIKeyID string `json:"api_key_id"`
		URL      string `json:"url"`
		Keys     []Key  `json:"keys"`
	}{e.ID, e.APIKeyID, e.URL, e.Keys})
	digest := sha256.Sum256(raw)
	if e.Hash() != hex.EncodeToString(digest[:]) || e.Version() != 1 {
		t.Fatal("legacy endpoint hash changed")
	}
	for _, v := range []int{-1, 3} {
		e.SchemaVersion = v
		raw, _ := json.Marshal([]Endpoint{e})
		if _, err := Parse(string(raw)); err == nil {
			t.Fatal("unknown schema accepted")
		}
	}
	e.SchemaVersion = 2
	raw, _ = json.Marshal([]Endpoint{e})
	out, err := Parse(string(raw))
	if err != nil || out[0].Version() != 2 {
		t.Fatal(err)
	}
}
