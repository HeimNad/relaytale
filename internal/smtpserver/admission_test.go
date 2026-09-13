package smtpserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
	"relaytale/internal/auth"
	"relaytale/internal/devtls"
)

func TestSourceIdentityAndBoundedAdmission(t *testing.T) {
	for _, pair := range [][2]string{{"127.0.0.1:1", "[::ffff:127.0.0.1]:2"}, {"[2001:db8::1]:1", "[2001:db8::abcd]:2"}} {
		a, _ := net.ResolveTCPAddr("tcp", pair[0])
		b, _ := net.ResolveTCPAddr("tcp", pair[1])
		if sourceKey(a) != sourceKey(b) {
			t.Fatal("source bypass")
		}
	}
	l := DefaultLimits()
	l.Connections = 2
	l.PerSource = 1
	a := newAdmission(l)
	one, ok := a.connect("one")
	if !ok {
		t.Fatal("first")
	}
	if _, ok = a.connect("one"); ok {
		t.Fatal("per source cap bypassed")
	}
	two, ok := a.connect("two")
	if !ok {
		t.Fatal("second")
	}
	if _, ok = a.connect("three"); ok {
		t.Fatal("global cap bypassed")
	}
	one()
	one()
	two()
	if a.total != 0 {
		t.Fatal("connection leak")
	}
	now := time.Now()
	a.now = func() time.Time { return now }
	for i := 0; i < maxSources; i++ {
		release, ok := a.connect(string(rune(i + 100)))
		if !ok {
			break
		}
		release()
	}
	if len(a.sources) > maxSources {
		t.Fatal("unbounded source map")
	}
	if _, ok = a.connect("overflow"); ok {
		t.Fatal("cache cap bypassed")
	}
	now = now.Add(6 * time.Minute)
	release, ok := a.connect("recovered")
	if !ok {
		t.Fatal("expired entries not reclaimed")
	}
	release()
	if len(a.sources) != 1 {
		t.Fatal("stale cache")
	}
}
func TestAuthBudgetRefillAndSourceIsolation(t *testing.T) {
	l := DefaultLimits()
	l.AuthBurst = 2
	l.AuthPerMinute = 2
	a := newAdmission(l)
	now := time.Now()
	a.now = func() time.Time { return now }
	connA, _ := a.connect("a")
	defer connA()
	connB, _ := a.connect("b")
	defer connB()
	first, ok := a.authenticate("a")
	if !ok {
		t.Fatal("first")
	}
	if _, ok = a.authenticate("a"); ok {
		t.Fatal("same source hash overlap")
	}
	other, ok := a.authenticate("b")
	if !ok {
		t.Fatal("other source starved")
	}
	other()
	first()
	first()
	second, ok := a.authenticate("a")
	if !ok {
		t.Fatal("second")
	}
	second()
	if _, ok = a.authenticate("a"); ok {
		t.Fatal("rate bypass")
	}
	now = now.Add(30 * time.Second)
	next, ok := a.authenticate("a")
	if !ok {
		t.Fatal("rate failed to refill")
	}
	next()
}
func TestSessionAbsoluteDeadlineCannotBeExtended(t *testing.T) {
	left, right := net.Pipe()
	defer right.Close()
	s := &Server{connections: make(map[net.Conn]struct{})}
	c := &trackedConn{Conn: left, server: s, release: func() {}, expires: time.Now().Add(50 * time.Millisecond)}
	defer c.Close()
	if err := c.SetReadDeadline(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, err := c.Read(make([]byte, 1))
	var e net.Error
	if !errors.As(err, &e) || !e.Timeout() {
		t.Fatal("absolute timeout not enforced", err)
	}
}

type blockingAuth struct {
	entered, release chan struct{}
	calls            atomic.Int32
}

func (a *blockingAuth) Authenticate(ctx context.Context, user, password string) (auth.Account, error) {
	a.calls.Add(1)
	if user == "hold" {
		close(a.entered)
		select {
		case <-a.release:
		case <-ctx.Done():
			return auth.Account{}, ctx.Err()
		}
	}
	return auth.Account{ID: "test"}, nil
}
func TestSMTPAuthFloodFromOneSourceLeavesAnotherUsable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("distinct loopback source addresses require Linux; exercised by Docker CI")
	}
	dir := t.TempDir()
	if err := devtls.Generate(dir); err != nil {
		t.Fatal(err)
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "smtp.crt"), filepath.Join(dir, "smtp.key"))
	if err != nil {
		t.Fatal(err)
	}
	pem, err := os.ReadFile(filepath.Join(dir, "smtp.crt"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem)
	a := &blockingAuth{entered: make(chan struct{}), release: make(chan struct{})}
	limits := DefaultLimits()
	limits.Connections = 3
	limits.PerSource = 2
	server := New(&Backend{Accounts: a, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, "localhost", cert, 1024, limits)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() { server.Close(); <-done })
	dial := func(ip string) *smtp.Client {
		d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(ip)}, Timeout: time.Second}
		conn, err := d.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		c, err := smtp.NewClientStartTLS(conn, &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12})
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	reject := func(ip string) {
		d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(ip)}, Timeout: time.Second}
		c, err := d.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(time.Second))
		_, err = c.Read(make([]byte, 1))
		var e net.Error
		if err == nil || (errors.As(err, &e) && e.Timeout()) {
			t.Fatal("connection limit did not close before greeting", err)
		}
	}
	attacker := dial("127.0.0.2")
	blocked := dial("127.0.0.2")
	reject("127.0.0.2")
	legitimate := dial("127.0.0.3")
	reject("127.0.0.4")
	held := make(chan error, 1)
	go func() { held <- attacker.Auth(sasl.NewPlainClient("", "hold", "test")) }()
	var once sync.Once
	release := func() { once.Do(func() { close(a.release) }) }
	defer release()
	select {
	case <-a.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("hash not started")
	}
	var smtpErr *smtp.SMTPError
	if err = blocked.Auth(sasl.NewPlainClient("", "another", "test")); !errors.As(err, &smtpErr) || smtpErr.Code != 454 {
		t.Fatal("same source not throttled", err)
	}
	if err = legitimate.Auth(sasl.NewPlainClient("", "legitimate", "test")); err != nil {
		t.Fatal("other source starved", err)
	}
	if a.calls.Load() != 2 {
		t.Fatal("throttled request reached password verification")
	}
	release()
	select {
	case err := <-held:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("authentication goroutine did not drain")
	}
}
