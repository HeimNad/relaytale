package feedback

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func sample(status string) string {
	return "From: daemon@example.test\r\nMIME-Version: 1.0\r\nContent-Type: multipart/report; report-type=delivery-status; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nHuman text is not evidence.\r\n--b\r\nContent-Type: message/delivery-status\r\n\r\nReporting-MTA: dns; mx.example.test\r\nOriginal-Envelope-Id: untrusted-id\r\n\r\n" + status + "\r\n--b--\r\n"
}

const failed = "Final-Recipient: rfc822; missing@example.test\r\nAction: failed\r\nStatus: 5.1.1\r\nDiagnostic-Code: smtp; 550 no mailbox\r\n"

func TestParseMultiRecipient(t *testing.T) {
	raw := sample(failed + "\r\nFinal-Recipient: rfc822; later@example.test\r\nAction: delayed\r\nStatus: 4.2.2\r\n")
	for _, raw := range []string{raw, strings.ReplaceAll(raw, "\r\n", "\n")} {
		report, err := Parse([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Recipients) != 2 || report.Recipients[0].Status != "5.1.1" || report.Recipients[1].Action != "delayed" || report.EnvelopeID != "untrusted-id" {
			t.Fatalf("%+v", report)
		}
	}
}
func TestParseRejectsAmbiguity(t *testing.T) {
	raw := sample(failed)
	cases := map[string]string{
		"duplicate":              strings.Replace(raw, "Status: 5.1.1", "Status: 5.1.1\r\nStatus: 4.2.2", 1),
		"inconsistent":           strings.Replace(raw, "Status: 5.1.1", "Status: 4.1.1", 1),
		"missing":                strings.Replace(raw, "Final-Recipient: rfc822; missing@example.test\r\n", "", 1),
		"arbitrary-action":       strings.Replace(raw, "Action: failed", "Action: suppress", 1),
		"bare-cr":                strings.Replace(raw, "550 no mailbox", "550\rno mailbox", 1),
		"nul":                    strings.Replace(raw, "550 no mailbox", "550\x00no mailbox", 1),
		"forwarded":              strings.Replace(raw, "multipart/report", "multipart/mixed", 1),
		"truncated":              strings.Replace(raw, "--b--", "", 1),
		"encoded-status":         strings.Replace(raw, "Content-Type: message/delivery-status", "Content-Type: message/delivery-status\r\nContent-Transfer-Encoding: base64", 1),
		"duplicate-content-type": strings.Replace(raw, "Content-Type: message/delivery-status", "Content-Type: message/delivery-status\r\nContent-Type: text/plain", 1),
		"long-field":             sample(strings.Replace(failed, "550 no mailbox", strings.Repeat("a", 4097), 1)),
		"too-many":               sample(strings.Repeat(failed+"\r\n", MaxRecipients+1)),
		"second-status":          strings.Replace(raw, "--b--", "--b\r\nContent-Type: message/delivery-status\r\n\r\n"+failed+"\r\n--b--", 1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(raw)); err == nil {
				t.Fatal("ambiguous DSN accepted")
			}
		})
	}
}
func TestReturnedMessageCannotSupplyStatus(t *testing.T) {
	raw := strings.Replace(sample(failed), "--b--", "--b\r\nContent-Type: message/rfc822\r\n\r\nSubject: failed\r\nStatus: 5.7.1\r\n\r\nAction: delivered\r\n\r\n--b--", 1)
	r, err := Parse([]byte(raw))
	if err != nil || len(r.Recipients) != 1 || r.Recipients[0].Status != "5.1.1" {
		t.Fatal(r, err)
	}
}
func TestReadBound(t *testing.T) {
	for _, n := range []int{0, MaxBytes + 1} {
		if _, err := Read(bytes.NewReader(make([]byte, n))); !errors.Is(err, ErrSize) {
			t.Fatal(n, err)
		}
	}
	if b, err := Read(bytes.NewReader(make([]byte, MaxBytes))); err != nil || len(b) != MaxBytes {
		t.Fatal(err)
	}
}
func FuzzParse(f *testing.F) {
	f.Add([]byte(sample(failed)))
	f.Add([]byte("not a DSN"))
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := Parse(b)
		if err == nil && (len(r.Recipients) == 0 || len(r.Recipients) > MaxRecipients) {
			t.Fatal("invalid report bounds")
		}
	})
}
