package provider

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"
)

type TXTResolver interface {
	LookupTXT(context.Context, string) ([]string, error)
}
type TXTObservation struct {
	Status     string `json:"status"`
	Candidates int    `json:"candidate_records"`
}
type DomainDiagnostic struct {
	Domain          string         `json:"domain"`
	Selector        string         `json:"selector,omitempty"`
	ObservedAt      time.Time      `json:"observed_at"`
	SPF             TXTObservation `json:"spf"`
	DMARC           TXTObservation `json:"dmarc_exact_domain"`
	DKIM            TXTObservation `json:"dkim_selector"`
	AffectsDelivery bool           `json:"affects_delivery"`
}

func validDNSName(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

// DiagnoseDomain performs at most three exact TXT queries. It does not evaluate
// SPF includes, organizational DMARC fallback, DNSSEC or a DKIM signature.
func DiagnoseDomain(ctx context.Context, resolver TXTResolver, domain, selector string) (DomainDiagnostic, error) {
	domain = strings.ToLower(domain)
	selector = strings.ToLower(selector)
	out := DomainDiagnostic{Domain: domain, Selector: selector, ObservedAt: time.Now().UTC(), DKIM: TXTObservation{Status: "not_checked"}}
	if !validDNSName(domain) || !strings.Contains(domain, ".") || net.ParseIP(domain) != nil || (selector != "" && (!validDNSName(selector) || len(selector+"._domainkey."+domain) > 253)) || len("_dmarc."+domain) > 253 {
		return out, ErrInvalid
	}
	if resolver == nil {
		return out, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	lookup := func(name, kind string) TXTObservation {
		records, err := resolver.LookupTXT(ctx, name)
		if err != nil {
			var dns *net.DNSError
			if errors.As(err, &dns) && dns.IsNotFound {
				return TXTObservation{Status: "not_found"}
			}
			return TXTObservation{Status: "lookup_failed"}
		}
		o := TXTObservation{Status: "no_candidate"}
		for _, record := range records {
			text := strings.TrimSpace(record)
			upper := strings.ToUpper(text)
			match := false
			if kind == "spf" {
				match = upper == "V=SPF1" || strings.HasPrefix(upper, "V=SPF1 ")
			}
			if kind == "dmarc" {
				match = strings.HasPrefix(upper, "V=DMARC1;")
			}
			if kind == "dkim" {
				for _, tag := range strings.Split(text, ";") {
					key, _, ok := strings.Cut(tag, "=")
					if ok && strings.EqualFold(strings.TrimSpace(key), "p") {
						match = true
					}
				}
			}
			if match {
				o.Candidates++
			}
		}
		if o.Candidates > 0 {
			o.Status = "candidate_present"
		}
		return o
	}
	out.SPF = lookup(domain, "spf")
	out.DMARC = lookup("_dmarc."+domain, "dmarc")
	if selector != "" {
		out.DKIM = lookup(selector+"._domainkey."+domain, "dkim")
	}
	return out, nil
}
