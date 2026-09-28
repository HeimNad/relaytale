package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Sign binds the timestamp and exact immutable request bytes. Receivers must
// additionally persist event_id for deduplication; a timestamp is not a nonce.
func Sign(keys []Key, at time.Time, body []byte) (string, string) {
	stamp := strconv.FormatInt(at.Unix(), 10)
	signatures := make([]string, 0, len(keys))
	for _, key := range keys {
		secret, _ := hex.DecodeString(key.Secret)
		mac := hmac.New(sha256.New, secret)
		mac.Write([]byte(stamp + "."))
		mac.Write(body)
		signatures = append(signatures, key.ID+":"+hex.EncodeToString(mac.Sum(nil)))
	}
	return stamp, strings.Join(signatures, ",")
}
func Verify(key Key, stamp, signature string, body []byte, now time.Time) bool {
	if len(body) > 4096 || len(signature) > 256 {
		return false
	}
	ts, e := strconv.ParseInt(stamp, 10, 64)
	if e != nil || strconv.FormatInt(ts, 10) != stamp {
		return false
	}
	at := time.Unix(ts, 0)
	if at.Before(now.Add(-5*time.Minute)) || at.After(now.Add(5*time.Minute)) {
		return false
	}
	secret, e := hex.DecodeString(key.Secret)
	if e != nil || len(secret) != 32 || !keyID.MatchString(key.ID) {
		return false
	}
	_, expected := Sign([]Key{key}, at, body)
	for _, s := range strings.Split(signature, ",") {
		if hmac.Equal([]byte(s), []byte(expected)) {
			return true
		}
	}
	return false
}

// Deny non-public/special-use ranges including transition mechanisms that can
// tunnel to private IPv4 destinations. Recheck every connection, with no proxy.
var denied = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "::/96", "::ffff:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20", "fc00::/7", "fe80::/10", "ff00::/8"} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

func publicIP(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range denied {
		if p.Contains(ip) {
			return false
		}
	}
	// IPv6 public allocations are restricted to the current global-unicast block.
	return ip.Is4() || netip.MustParsePrefix("2000::/3").Contains(ip)
}

type Sender interface {
	Send(context.Context, Endpoint, []byte) (int, error)
}
type HTTPSender struct{}

func (HTTPSender) Send(ctx context.Context, endpoint Endpoint, body []byte) (int, error) {
	if ValidateURL(endpoint.URL) != nil || len(body) > 4096 {
		return 0, errors.New("invalid webhook request")
	}
	u, _ := url.Parse(endpoint.URL)
	tr := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.Hostname()}, DisableKeepAlives: true, DisableCompression: true, MaxResponseHeaderBytes: 8192, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second}
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return safeDial(ctx, address, u.Hostname(), net.DefaultResolver.LookupNetIP, (&net.Dialer{Timeout: 5 * time.Second}).DialContext)
	}

	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return sendHTTP(ctx, client, endpoint, body)
}
func sendHTTP(ctx context.Context, client *http.Client, endpoint Endpoint, body []byte) (int, error) {
	req, e := http.NewRequestWithContext(ctx, "POST", endpoint.URL, bytes.NewReader(body))
	if e != nil {
		return 0, errors.New("invalid webhook request")
	}
	stamp, sig := Sign(endpoint.Keys, time.Now(), body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "RelayTale-Webhook/1")
	req.Header.Set("X-RelayTale-Timestamp", stamp)
	req.Header.Set("X-RelayTale-Signature", sig)
	resp, e := client.Do(req)
	if e != nil {
		return 0, errors.New("webhook transport failed")
	}
	defer resp.Body.Close()
	n, e := io.Copy(io.Discard, io.LimitReader(resp.Body, 4097))
	if e != nil || n > 4096 {
		return resp.StatusCode, errors.New("webhook response invalid or oversized")
	}
	return resp.StatusCode, nil
}

func safeDial(ctx context.Context, address, expected string, lookup func(context.Context, string, string) ([]netip.Addr, error), dial func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host != expected || port != "443" {
		return nil, errors.New("invalid webhook destination")
	}
	ips, err := lookup(ctx, "ip", host)
	if err != nil || len(ips) == 0 || len(ips) > 16 {
		return nil, errors.New("webhook DNS unavailable")
	}
	for _, ip := range ips {
		if !publicIP(ip) {
			return nil, errors.New("webhook address blocked")
		}
	}
	return dial(ctx, "tcp", net.JoinHostPort(ips[0].String(), "443"))
}
