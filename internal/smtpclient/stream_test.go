package smtpclient

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type shortReader struct{ io.Reader }

func (r shortReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.Reader.Read(p)
}

type failedReader struct{}

func (failedReader) Read([]byte) (int, error) { return 0, errors.New("source failed") }
func TestStreamingDotsAtBufferBoundaries(t *testing.T) {
	raw := ".\r\n..line\r\n" + strings.Repeat("x", 32755) + "\r\n.dot\r\n"
	var out bytes.Buffer
	n, err := streamDATA(context.Background(), bufio.NewWriterSize(&out, 17), shortReader{strings.NewReader(raw)})
	want := "." + strings.ReplaceAll(raw, "\n.", "\n..") + ".\r\n"
	if err != nil || n != int64(len(raw)) || out.String() != want {
		t.Fatal("bad dot-stuff boundary", n, err)
	}
}
func TestValidationAndReadFailureNeverAddsTerminator(t *testing.T) {
	for _, raw := range []string{"", "bare\n", "bare\rCR\r\n", "no terminator", "dangling\r"} {
		if err := validatePayload(context.Background(), strings.NewReader(raw)); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	raw := strings.Repeat("x", 32767) + "\r\n.body\r\n"
	r := strings.NewReader(raw)
	if err := validatePayload(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if pos, _ := r.Seek(0, io.SeekCurrent); pos != 0 {
		t.Fatal("not rewound")
	}
	var out bytes.Buffer
	w := bufio.NewWriter(&out)
	_, err := streamDATA(context.Background(), w, io.MultiReader(strings.NewReader("prefix\r\n"), failedReader{}))
	_ = w.Flush()
	if err == nil || strings.HasSuffix(out.String(), "\r\n.\r\n") {
		t.Fatal("failed source terminated DATA")
	}
}
