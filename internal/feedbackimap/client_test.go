package feedbackimap

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func imapFixture(t *testing.T, mode string) (Config, *x509.CertPool, func() []string) {
	t.Helper()
	certServer := httptest.NewTLSServer(nil)
	cert := certServer.TLS.Certificates[0]
	roots := x509.NewCertPool()
	roots.AddCert(certServer.Certificate())
	certServer.Close()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var commands []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] fixture\r\n")
		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return
			}
			tag, command := fields[0], fields[1]
			mu.Lock()
			commands = append(commands, strings.TrimSpace(line))
			mu.Unlock()
			switch command {
			case "CAPABILITY":
				fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1\r\n%s OK done\r\n", tag)
			case "LOGIN":
				if mode == "auth-fail" {
					fmt.Fprintf(conn, "%s NO private-secret-from-server\r\n", tag)
				} else {
					fmt.Fprintf(conn, "%s OK logged in\r\n", tag)
				}
			case "EXAMINE":
				fmt.Fprintf(conn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 42] epoch\r\n* OK [UIDNEXT 2] next\r\n%s OK [READ-ONLY] selected\r\n", tag)
			case "UID":
				if strings.Contains(line, "BODY.PEEK") {
					if mode == "stall" {
						io.Copy(io.Discard, conn)
						return
					}
					if mode == "oversize" {
						fmt.Fprint(conn, "* 1 FETCH (UID 1 BODY[]<0> {999999999}\r\n")
						io.Copy(io.Discard, conn)
						return
					}
					payload := "Subject: test\r\n\r\nhello\r\n"
					fmt.Fprintf(conn, "* 1 FETCH (UID 1 BODY[]<0> {%d}\r\n%s)\r\n%s OK fetched\r\n", len(payload), payload, tag)
				} else {
					fmt.Fprintf(conn, "* 1 FETCH (UID 1 RFC822.SIZE 24)\r\n%s OK fetched\r\n", tag)
				}
			default:
				fmt.Fprintf(conn, "%s BAD forbidden operation\r\n", tag)
			}
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			t.Error("fixture did not stop")
		}
	})
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	p, _ := strconv.Atoi(port)
	return Config{Host: host, Port: p, Username: "reader", Password: "password", Folder: "Bounces"}, roots, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), commands...) }
}
func TestReadOnlyProtocol(t *testing.T) {
	cfg, roots, commands := imapFixture(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	box, err := dial(ctx, cfg, roots)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	snap, err := box.Select(ctx)
	if err != nil || snap.UIDValidity != 42 || snap.UIDNext != 2 {
		t.Fatal(snap, err)
	}
	meta, err := box.Metadata(ctx, 1, 1)
	if err != nil || len(meta) != 1 || meta[0].UID != 1 {
		t.Fatal(meta, err)
	}
	raw, present, err := box.Body(ctx, 1)
	if err != nil || !present || string(raw) != "Subject: test\r\n\r\nhello\r\n" {
		t.Fatal(string(raw), present, err)
	}
	box.Close()
	transcript := strings.Join(commands(), "\n")
	if !strings.Contains(transcript, "EXAMINE") || !strings.Contains(transcript, "BODY.PEEK[]<0.1048577>") {
		t.Fatal(transcript)
	}
	for _, bad := range []string{" SELECT ", " STORE ", " EXPUNGE", " CLOSE", " DELETE", " MOVE"} {
		if strings.Contains(transcript, bad) {
			t.Fatal("mailbox mutation", transcript)
		}
	}
}
func TestIMAPBoundsAndAuthentication(t *testing.T) {
	for _, mode := range []string{"oversize", "stall", "auth-fail", "untrusted-tls"} {
		t.Run(mode, func(t *testing.T) {
			cfg, roots, _ := imapFixture(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
			defer cancel()
			if mode == "untrusted-tls" {
				roots = x509.NewCertPool()
			}
			box, err := dial(ctx, cfg, roots)
			if mode == "auth-fail" || mode == "untrusted-tls" {
				if err == nil {
					box.Close()
					t.Fatal("accepted invalid credentials/certificate")
				}
				if strings.Contains(err.Error(), "private-secret") {
					t.Fatal("server error leaked")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer box.Close()
			if _, err = box.Select(ctx); err != nil {
				t.Fatal(err)
			}
			if _, _, err = box.Body(ctx, 1); err == nil {
				t.Fatal("expected bounded failure")
			}
		})
	}
}
func TestIdentityExcludesPasswordAndSeparatesFolder(t *testing.T) {
	c := Config{Host: "mail.example", Port: 993, Username: "reader", Password: "one", Folder: "Bounces"}
	id := c.Identity()
	c.Password = "two"
	if c.Identity() != id {
		t.Fatal("password rotation changed identity")
	}
	c.Folder = "INBOX"
	if c.Identity() == id {
		t.Fatal("folder not bound")
	}
}
func TestWholeWireBudget(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := &boundedConn{Conn: a}
	c.remaining.Store(4)
	defer c.Close()
	go func() { _, _ = b.Write([]byte("12345678")) }()
	buf := make([]byte, 20)
	n, err := c.Read(buf)
	if err != nil || n != 4 {
		t.Fatal(n, err)
	}
	if _, err = c.Read(buf); err == nil {
		t.Fatal("budget bypass")
	}
}
