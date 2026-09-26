package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"relaytale/internal/api"
	"relaytale/internal/resource"
	"relaytale/internal/storage"
	"relaytale/internal/testsmtp"
)

func TestHTTPSubmissionDefaultSizeBoundary(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, receiver := sendFixture(t, f)
	receiver.MaxBytes = 25 << 20
	receiver.Store = storage.LocalStore{Root: f.root, MaxBytes: receiver.MaxBytes}
	receiver.Resources = resource.New(8<<20, 64<<20)
	config, _ := json.Marshal([]api.SendKey{key})
	handler, err := api.NewSendHandler(context.Background(), string(config), receiver)
	if err != nil {
		t.Fatal(err)
	}
	prefix := f.raw()
	fill := int(receiver.MaxBytes) - len(prefix) - 2
	raw := prefix + strings.Repeat("a\r\n", fill/3) + strings.Repeat("a", fill%3) + "\r\n"
	if int64(len(raw)) != receiver.MaxBytes {
		t.Fatal("fixture size")
	}
	requireStatus(t, sendRequest(handler, key, "size-boundary", raw, nil), 202)
	requireStatus(t, sendRequest(handler, key, "size-boundary", raw, nil), 202)
	requireStatus(t, sendRequest(handler, key, "too-large", raw+"x", nil), 413)
	if emlCount(t, f.root) != 1 {
		t.Fatal("large replay published duplicate")
	}
	assertCount(t, f, 1, `SELECT count(*) FROM messages WHERE api_key_id=$1 AND eml_size=$2`, key.ID, receiver.MaxBytes)
	assertHTTPResourcesReleased(t, receiver)
}

func TestHTTPSubmissionStalledSocketTimesOut(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	handler, key, receiver := sendFixture(t, f)
	done := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r); done <- struct{}{} }))
	defer server.Close()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "http://"), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(16 * time.Second))
	_, err = fmt.Fprintf(conn, "POST /v1/messages HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nContent-Type: message/rfc822\r\nIdempotency-Key: stalled\r\nX-RelayTale-From: %s\r\nX-RelayTale-To: one@example.test\r\nContent-Length: 1000\r\nConnection: close\r\n\r\nFrom:", key.Token, key.AllowedFrom[0])
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "POST"})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 503 {
		t.Fatal(response.StatusCode)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler retained admission")
	}
	assertHTTPResourcesReleased(t, receiver)
	assertCount(t, f, 0, `SELECT count(*) FROM messages WHERE api_key_id=$1`, key.ID)
	if emlCount(t, f.root) != 0 {
		t.Fatal("partial stalled body published")
	}
	requireStatus(t, sendRequest(handler, key, "stalled", f.raw(), nil), 202)
}
