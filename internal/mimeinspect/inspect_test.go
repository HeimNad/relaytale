package mimeinspect

import (
	"strings"
	"testing"
)

func TestInspectNestedMIMEWithoutBodyExposure(t *testing.T) {
	raw := "From: sender@example.test\r\nContent-Type: multipart/mixed; boundary=outer\r\n\r\n--outer\r\nContent-Type: multipart/alternative; boundary=inner\r\n\r\n--inner\r\nContent-Type: text/plain\r\n\r\nPRIVATE BODY\r\n--inner\r\nContent-Type: text/html\r\n\r\n<script>PRIVATE</script>\r\n--inner--\r\n--outer\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename*=utf-8''%E6%B5%8B%E8%AF%95.txt\r\nContent-Transfer-Encoding: base64\r\n\r\nYWJj\r\n--outer--\r\n"
	report, err := Inspect([]byte(raw))
	if err != nil || len(report.Parts) != 5 {
		t.Fatal(report, err)
	}
	part := report.Parts[4]
	if part.Filename != "测试.txt" || part.Path != "1.2" || part.DecodedBytes == nil || *part.DecodedBytes != 3 {
		t.Fatal(part)
	}
}
func TestInspectionBoundsAndMalformedEncoding(t *testing.T) {
	for _, raw := range []string{
		"Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: text/plain\r\n\r\nx",
		"Content-Transfer-Encoding: base64\r\n\r\n!!!!\r\n",
		"X-Long: " + strings.Repeat("x", 8193) + "\r\n\r\nbody\r\n",
		strings.Repeat("x", MaxBytes+1),
	} {
		if _, err := Inspect([]byte(raw)); err == nil {
			t.Fatal("malformed or oversized MIME accepted")
		}
	}
	raw := "Content-Type: multipart/mixed; boundary=x\r\n\r\n" + strings.Repeat("--x\r\nContent-Type: text/plain\r\n\r\na\r\n", maxParts) + "--x--\r\n"
	if _, err := Inspect([]byte(raw)); err == nil {
		t.Fatal("part limit ignored")
	}
}
