package feedback

import (
	"errors"
	"strings"
	"testing"
)

// Synthetic structures based on the real IMAP validation. No private original
// headers, addresses, queue identifiers, signatures or message bodies are copied.
func TestProviderGenericFailureWithReturnedHeaders(t *testing.T) {
	for _, diagnostic := range []string{"smtp; 550 Mailbox unavailable or access denied", "smtp; 550 User not found", "smtp; 550 Mailbox not found"} {
		t.Run(diagnostic, func(t *testing.T) {
			raw := sample("Final-Recipient: rfc822; target@example.test\r\nAction: failed\r\nStatus: 5.0.0\r\nRemote-MTA: dns; destination.example.test\r\nDiagnostic-Code: " + diagnostic + "\r\n")
			raw = strings.Replace(raw, "Original-Envelope-Id: untrusted-id\r\n", "X-ZoneMTA-Queue-ID: synthetic-queue\r\nX-ZoneMTA-Sender: rfc822; sender@example.test\r\n", 1)
			raw = "In-Reply-To: <synthetic@example.test>\r\nReferences: <synthetic@example.test>\r\n" + raw
			raw = strings.Replace(raw, "--b--", "--b\r\nContent-Type: text/rfc822-headers\r\n\r\nMessage-ID: <synthetic@example.test>\r\nOriginal-Envelope-Id: must-not-be-used\r\nStatus: 5.1.1\r\nAction: delivered\r\n\r\n--b--", 1)
			report, err := Parse([]byte(raw))
			if err != nil || len(report.Recipients) != 1 {
				t.Fatal(report, err)
			}
			r := report.Recipients[0]
			if r.Status != "5.0.0" || r.Action != "failed" || r.Diagnostic != diagnostic || report.EnvelopeID != "" {
				t.Fatal("inferred fields from diagnostic or returned headers", report)
			}
		})
	}
}
func TestProviderWelcomeMessageIsNotFeedback(t *testing.T) {
	raw := "From: welcome@example.test\r\nSubject: Welcome\r\nX-Pm-Webmail-Alert: synthetic\r\nAuthentication-Results: example.test; dkim=pass\r\nContent-Type: multipart/mixed; boundary=outer\r\n\r\n--outer\r\nContent-Type: multipart/alternative; boundary=inner\r\n\r\n--inner\r\nContent-Type: text/plain\r\n\r\nWelcome.\r\n--inner\r\nContent-Type: text/html\r\n\r\n<p>Welcome.</p>\r\n--inner--\r\n--outer--\r\n"
	if _, err := Parse([]byte(raw)); !errors.Is(err, ErrFormat) {
		t.Fatal("ordinary welcome message accepted as DSN", err)
	}
}
