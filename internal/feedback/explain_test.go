package feedback

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func nonstandardSample() string {
	return sample("Final-Recipient: target@example.test\r\nAction: failed\r\nStatus: 554\r\nDiagnostic-Code: error; 554 5.1.1 rejected\r\n\r\nRemote-MTA: dns; mx.example.test\r\n")
}
func TestExplanationKeepsFieldsAndBoundaries(t *testing.T) {
	raw := nonstandardSample()
	r, err := Explain([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if r.StrictlyParsed || r.Trust != "UNVERIFIED" || r.AutomaticActionAllowed || len(r.Blocks) != 3 {
		t.Fatal(r)
	}
	if r.Blocks[1].Fields["Status"][0] != "554" || r.Blocks[1].Hints.SMTPCode != 554 || r.Blocks[1].Hints.Mailbox != "target@example.test" || r.Blocks[2].Hints.Mailbox != "" {
		t.Fatal(r)
	}
	if !strings.Contains(r.RawStatus, "\r\n\r\nRemote-MTA:") || len(r.Issues) != 5 {
		t.Fatal(r)
	}
	if _, err = Parse([]byte(raw)); err == nil {
		t.Fatal("strict parser relaxed")
	}
}
func TestExplanationNoGuessOnDuplicatesOrProse(t *testing.T) {
	raw := strings.Replace(nonstandardSample(), "Status: 554", "Status: 554\r\nStatus: 550", 1)
	r, err := Explain([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Blocks[1].Fields["Status"]) != 2 || r.Blocks[1].Hints.SMTPCode != 0 || r.Blocks[1].Hints.Mailbox != "" {
		t.Fatal(r)
	}
	for i := 0; i < 10; i++ {
		again, _ := Explain([]byte(raw))
		a, _ := json.Marshal(r)
		b, _ := json.Marshal(again)
		if !bytes.Equal(a, b) {
			t.Fatal("unstable explanation")
		}
	}
	raw = strings.Replace(nonstandardSample(), "Status: 554", "Status: nonsense", 1)
	r, err = Explain([]byte(raw))
	if err != nil || r.Blocks[1].Hints.SMTPCode != 0 {
		t.Fatal(r, err)
	}
}
func TestExplanationRejectsUnsafeEnvelopes(t *testing.T) {
	base := nonstandardSample()
	for _, raw := range []string{strings.Replace(base, "multipart/report", "multipart/mixed", 1), strings.Replace(base, "--b--", "", 1), strings.Replace(base, "Status: 554", "Status: 55\r4", 1), strings.Replace(base, "Status: 554", "Status: \x00", 1), strings.Replace(base, "Status: 554", strings.Repeat("x", 4097), 1), strings.Replace(base, "Content-Type: message/delivery-status", "Content-Type: message/delivery-status\r\nContent-Transfer-Encoding: base64", 1)} {
		if _, e := Explain([]byte(raw)); e == nil {
			t.Fatal("unsafe envelope accepted")
		}
	}
}
func TestStandardExplanationRemainsAdvisory(t *testing.T) {
	r, e := Explain([]byte(sample(failed)))
	if e != nil || !r.StrictlyParsed || r.AutomaticActionAllowed || len(r.Issues) != 0 {
		t.Fatal(r, e)
	}
}
func FuzzExplain(f *testing.F) {
	f.Add([]byte(nonstandardSample()))
	f.Add([]byte(sample(failed)))
	f.Fuzz(func(t *testing.T, b []byte) {
		r, e := Explain(b)
		if e == nil && (r.AutomaticActionAllowed || r.Trust != "UNVERIFIED" || len(r.RawStatus) > maxStatusBytes || len(r.Blocks) > 201) {
			t.Fatal("bounds or trust violated")
		}
	})
}
