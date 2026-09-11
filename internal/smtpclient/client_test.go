package smtpclient

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	"mailgateway/internal/provider"
	"mailgateway/internal/testsmtp"
)

const raw = "From: sender@example.test\r\nMessage-ID: <stable@example.test>\r\nDKIM-Signature: unchanged-test\r\n\r\n.line\r\nbody\r\n"

func TestProviderConversation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options testsmtp.Options
		status  string
	}{
		{"accepted", testsmtp.Options{}, Accepted},
		{"implicit TLS", testsmtp.Options{ImplicitTLS: true}, Accepted},
		{"auth rejected", testsmtp.Options{RejectAuth: true}, Permanent},
		{"missing STARTTLS", testsmtp.Options{NoSTARTTLS: true}, Temporary},
		{"partial recipients", testsmtp.Options{RejectRecipients: map[string]int{"two@example.test": 550}}, Partial},
		{"all recipients rejected", testsmtp.Options{RejectRecipients: map[string]int{"one@example.test": 550, "two@example.test": 550}}, Permanent},
		{"DATA temporary", testsmtp.Options{DataCode: 451}, Temporary},
		{"final temporary", testsmtp.Options{FinalCode: 451}, Temporary},
		{"final permanent", testsmtp.Options{FinalCode: 550}, Permanent},
		{"final connection lost", testsmtp.Options{DropFinal: true}, Unknown},
		{"final response timeout", testsmtp.Options{FinalDelay: 300 * time.Millisecond}, Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := testsmtp.Start(t, tc.options)
			host, port, _ := net.SplitHostPort(fake.Addr)
			number, _ := strconv.Atoi(port)
			security := "starttls"
			if tc.options.ImplicitTLS {
				security = "implicit_tls"
			}
			p := provider.Provider{Host: host, Port: number, Username: "provider-user", Security: security}
			timeout := 3 * time.Second
			if tc.options.FinalDelay > 0 {
				timeout = 150 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			armed := false
			out := (Client{Domain: "gateway.test", RootCAs: fake.Roots}).Send(ctx, p, "provider-password", "sender@example.test", []Recipient{{"one", "one@example.test"}, {"two", "two@example.test"}}, []byte(raw), func(context.Context) error { armed = true; return nil })
			if out.Status != tc.status {
				t.Fatalf("want %s got %+v", tc.status, out)
			}
			if tc.status == Accepted || tc.status == Partial || tc.status == Unknown {
				if !armed {
					t.Fatal("DATA sent without durable fence")
				}
				select {
				case payload := <-fake.Payloads:
					if !bytes.Equal(payload, []byte(raw)) {
						t.Fatal("raw MIME changed")
					}
				default:
					t.Fatal("no payload received")
				}
			}
			if tc.status == Unknown && out.DataCompletedAt.IsZero() {
				t.Fatal("missing completed DATA stage")
			}
			if bytes.Contains([]byte(out.Response), []byte("provider-password")) {
				t.Fatal("password leaked")
			}
		})
	}
}
func TestFenceFailurePreventsDATA(t *testing.T) {
	fake := testsmtp.Start(t, testsmtp.Options{})
	host, port, _ := net.SplitHostPort(fake.Addr)
	number, _ := strconv.Atoi(port)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out := (Client{RootCAs: fake.Roots}).Send(ctx, provider.Provider{Host: host, Port: number, Username: "provider-user", Security: "starttls"}, "provider-password", "sender@example.test", []Recipient{{"one", "one@example.test"}}, []byte(raw), func(context.Context) error { return errors.New("lease lost") })
	if out.Status != Temporary || !out.DataStartedAt.IsZero() {
		t.Fatalf("unsafe fence result: %+v", out)
	}
	select {
	case <-fake.Payloads:
		t.Fatal("sent despite failed fence")
	default:
	}
}

func TestUntrustedProviderCertificateRejected(t *testing.T) {
	fake := testsmtp.Start(t, testsmtp.Options{})
	host, port, _ := net.SplitHostPort(fake.Addr)
	number, _ := strconv.Atoi(port)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out := (Client{}).Send(ctx, provider.Provider{Host: host, Port: number, Username: "provider-user", Security: "starttls"}, "provider-password", "sender@example.test", []Recipient{{"one", "one@example.test"}}, []byte(raw), func(context.Context) error { t.Fatal("untrusted TLS reached DATA"); return nil })
	if out.Status != Temporary || out.ErrorClass != "TLS_ERROR" {
		t.Fatalf("untrusted certificate accepted: %+v", out)
	}
}

func TestProbeAndRecorderFailure(t *testing.T) {
	fake := testsmtp.Start(t, testsmtp.Options{})
	host, port, _ := net.SplitHostPort(fake.Addr)
	number, _ := strconv.Atoi(port)
	p := provider.Provider{Host: host, Port: number, Username: "provider-user", Security: "starttls", Timeout: 3 * time.Second}
	c := Client{Domain: "gateway.test", RootCAs: fake.Roots}
	out := c.Probe(context.Background(), p, "provider-password")
	if out.Status != "READY" || out.DNSCompletedAt.IsZero() {
		t.Fatalf("probe: %+v", out)
	}
	for _, key := range []string{"dns_ms", "connect_ms", "tls_ms", "auth_ms", "total_ms"} {
		if _, ok := out.Timings[key]; !ok {
			t.Fatalf("missing timing %s", key)
		}
	}
	select {
	case <-fake.Payloads:
		t.Fatal("probe sent mail")
	default:
	}
	armed := false
	out = c.Send(context.Background(), p, "provider-password", "sender@example.test", []Recipient{{"one", "one@example.test"}}, []byte(raw), func(context.Context) error { armed = true; return nil }, func(context.Context, Event) error { return errors.New("recorder unavailable") })
	if !out.RecorderError || armed || out.Status != Temporary {
		t.Fatalf("unsafe recorder failure: %+v", out)
	}
}

func TestLoginOnlyProvider(t *testing.T) {
	fake := testsmtp.Start(t, testsmtp.Options{LoginOnly: true})
	host, port, _ := net.SplitHostPort(fake.Addr)
	n, _ := strconv.Atoi(port)
	out := (Client{Domain: "gateway.test", RootCAs: fake.Roots}).Probe(context.Background(), provider.Provider{Host: host, Port: n, Username: "provider-user", Security: "starttls", Timeout: time.Second}, "provider-password")
	if out.Status != "READY" {
		t.Fatalf("LOGIN negotiation: %+v", out)
	}
}
