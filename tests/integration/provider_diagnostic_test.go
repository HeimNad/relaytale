package integration

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"relaytale/internal/provider"
	"relaytale/internal/smtpclient"
	"relaytale/internal/testsmtp"
)

func TestProbeCapabilitiesRequireTLSAndNeverSendData(t *testing.T) {
	for _, opts := range []testsmtp.Options{{}, {ImplicitTLS: true}, {NoSTARTTLS: true}, {RejectAuth: true}} {
		t.Run(strconv.FormatBool(opts.ImplicitTLS)+strconv.FormatBool(opts.NoSTARTTLS)+strconv.FormatBool(opts.RejectAuth), func(t *testing.T) {
			f := delivery(t, opts, 1)
			host, port, _ := net.SplitHostPort(f.fake.Addr)
			number, _ := strconv.Atoi(port)
			p := provider.Provider{Host: host, Port: number, Security: "starttls", Username: "provider-user", Timeout: 3 * time.Second}
			if opts.ImplicitTLS {
				p.Security = "implicit_tls"
			}
			out := (smtpclient.Client{RootCAs: f.fake.Roots}).Probe(context.Background(), p, "provider-password")
			if opts.NoSTARTTLS {
				if out.Capabilities != nil || out.Status == "READY" {
					t.Fatal("plaintext capability promoted")
				}
			} else {
				if out.Capabilities == nil || !out.Capabilities.EightBitMIME || !out.Capabilities.AuthPlain || !out.Capabilities.AuthLogin {
					t.Fatal("secure EHLO missing")
				}
				if opts.RejectAuth {
					if out.Status == "READY" {
						t.Fatal("AUTH failure promoted")
					}
				} else if out.Status != "READY" {
					t.Fatal("probe failed", out.ErrorClass)
				}
			}
			if !out.MailFromAt.IsZero() || !out.DataStartedAt.IsZero() || len(out.Recipients) != 0 || len(f.fake.Payloads) != 0 {
				t.Fatal("probe attempted mail delivery")
			}
		})
	}
}
