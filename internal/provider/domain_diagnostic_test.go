package provider

import (
	"context"
	"net"
	"reflect"
	"testing"
)

type diagnosticDNS struct{ calls []string }

func (d *diagnosticDNS) LookupTXT(ctx context.Context, name string) ([]string, error) {
	d.calls = append(d.calls, name)
	switch name {
	case "example.test":
		return []string{"v=spf1 include:ignored.test -all", "v=spf1 -all"}, nil
	case "_dmarc.example.test":
		return nil, &net.DNSError{IsNotFound: true}
	default:
		return []string{"p=;"}, nil
	}
}
func TestDomainDiagnosticDoesNotPromoteDNSPresence(t *testing.T) {
	dns := &diagnosticDNS{}
	r, e := DiagnoseDomain(context.Background(), dns, "EXAMPLE.TEST", "s1")
	if e != nil {
		t.Fatal(e)
	}
	if r.SPF.Candidates != 2 || r.DMARC.Status != "not_found" || r.DKIM.Status != "candidate_present" || r.AffectsDelivery {
		t.Fatal("invalid DNS interpretation")
	}
	if !reflect.DeepEqual(dns.calls, []string{"example.test", "_dmarc.example.test", "s1._domainkey.example.test"}) {
		t.Fatal("unrequested DNS lookup", dns.calls)
	}
	for _, domain := range []string{"127.0.0.1", "bad..test", "a@test", "-bad.test", "a.test\n"} {
		if _, e := DiagnoseDomain(context.Background(), dns, domain, ""); e == nil {
			t.Fatal("invalid name accepted")
		}
	}
}
