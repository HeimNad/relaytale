// Package feedback handles untrusted delivery reports. Parsing never grants trust.
package feedback

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"regexp"
	"strings"
	"unicode/utf8"
)

const MaxBytes = 1 << 20
const maxStatusBytes = 64 << 10
const MaxRecipients = 100

var ErrSize = errors.New("feedback must contain 1..1048576 bytes")
var ErrFormat = errors.New("unsupported or malformed DSN; manual inspection required")

type Recipient struct {
	FinalRecipient    string `json:"final_recipient"`
	OriginalRecipient string `json:"original_recipient,omitempty"`
	Action            string `json:"action"`
	Status            string `json:"status"`
	Diagnostic        string `json:"diagnostic,omitempty"`
}
type Report struct {
	ReportingMTA string      `json:"reporting_mta"`
	EnvelopeID   string      `json:"original_envelope_id,omitempty"`
	Recipients   []Recipient `json:"recipients"`
}

// Read bounds raw evidence before any MIME parsing or database transaction.
func Read(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil {
		return nil, errors.New("cannot read feedback")
	}
	if len(b) == 0 || len(b) > MaxBytes {
		return nil, ErrSize
	}
	return b, nil
}

// Parse accepts a top-level RFC 3464 report only, never a forwarded/nested DSN.
// Human-readable and returned-message parts are opaque and cannot supply fields.
func Parse(raw []byte) (Report, error) {
	body, err := statusBody(raw)
	if err != nil {
		return Report{}, err
	}
	return parseStatus(body)
}

// statusBody enforces the same bounded MIME envelope for strict parsing and
// advisory explanation. Returned-message and human-readable parts stay opaque.
func statusBody(raw []byte) ([]byte, error) {
	bad := func() ([]byte, error) { return nil, ErrFormat }
	if len(raw) == 0 || len(raw) > MaxBytes {
		return nil, ErrSize
	}
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return bad()
	}
	if len(m.Header["Content-Type"]) != 1 {
		return bad()
	}
	kind, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil || kind != "multipart/report" || !strings.EqualFold(params["report-type"], "delivery-status") || params["boundary"] == "" || !plainEncoding(textproto.MIMEHeader(m.Header)) {
		return bad()
	}
	parts := multipart.NewReader(m.Body, params["boundary"])
	var status []byte
	count := 0
	for {
		p, e := parts.NextRawPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			return bad()
		}
		count++
		if count > 3 {
			return bad()
		}
		typ, _, e := mime.ParseMediaType(p.Header.Get("Content-Type"))
		if e != nil || len(p.Header["Content-Type"]) != 1 {
			return bad()
		}
		switch count {
		case 1:
			if typ != "text/plain" {
				return bad()
			}
		case 2:
			if typ != "message/delivery-status" || !plainEncoding(p.Header) {
				return bad()
			}
			body, e := io.ReadAll(io.LimitReader(p, maxStatusBytes+1))
			if e != nil || len(body) > maxStatusBytes {
				return bad()
			}
			status = body
		case 3:
			if typ != "message/rfc822" && typ != "text/rfc822-headers" {
				return bad()
			}
		}
		if _, e = io.Copy(io.Discard, p); e != nil {
			return bad()
		}
	}
	if count < 2 {
		return bad()
	}
	return status, nil
}
func plainEncoding(h textproto.MIMEHeader) bool {
	if len(h["Content-Transfer-Encoding"]) > 1 {
		return false
	}
	switch strings.ToLower(h.Get("Content-Transfer-Encoding")) {
	case "", "7bit", "8bit":
		return true
	}
	return false
}

var statusPattern = regexp.MustCompile(`^[245]\.[0-9]{1,3}\.[0-9]{1,3}$`)

func parseStatus(b []byte) (Report, error) {
	if !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return Report{}, ErrFormat
	}
	// Accept LF exports as well as wire CRLF; a bare CR is ambiguous.
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	if bytes.IndexByte(b, '\r') >= 0 {
		return Report{}, ErrFormat
	}
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(line) > 4096 {
			return Report{}, ErrFormat
		}
	}
	blocks := bytes.Split(bytes.Trim(b, "\n"), []byte("\n\n"))
	if len(blocks) < 2 || len(blocks) > MaxRecipients+1 {
		return Report{}, ErrFormat
	}
	var out Report
	for i, block := range blocks {
		h, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(append(bytes.Clone(block), '\n', '\n')))).ReadMIMEHeader()
		if err != nil {
			return Report{}, ErrFormat
		}
		for _, v := range h {
			if len(v) != 1 {
				return Report{}, ErrFormat
			}
		}
		if i == 0 {
			out.ReportingMTA = h.Get("Reporting-Mta")
			out.EnvelopeID = h.Get("Original-Envelope-Id")
			if !typed(out.ReportingMTA) {
				return Report{}, ErrFormat
			}
			continue
		}
		r := Recipient{FinalRecipient: h.Get("Final-Recipient"), OriginalRecipient: h.Get("Original-Recipient"), Action: strings.ToLower(h.Get("Action")), Status: h.Get("Status"), Diagnostic: h.Get("Diagnostic-Code")}
		if !typed(r.FinalRecipient) || (r.OriginalRecipient != "" && !typed(r.OriginalRecipient)) || !statusPattern.MatchString(r.Status) {
			return Report{}, ErrFormat
		}
		class := r.Status[0]
		switch r.Action {
		case "failed":
			if class != '5' {
				return Report{}, ErrFormat
			}
		case "delayed":
			if class != '4' {
				return Report{}, ErrFormat
			}
		case "delivered", "relayed", "expanded":
			if class != '2' {
				return Report{}, ErrFormat
			}
		default:
			return Report{}, ErrFormat
		}
		out.Recipients = append(out.Recipients, r)
	}
	return out, nil
}
func typed(s string) bool {
	a, b, ok := strings.Cut(s, ";")
	return ok && strings.TrimSpace(a) != "" && strings.TrimSpace(b) != ""
}
