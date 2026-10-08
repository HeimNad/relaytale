package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"

	"relaytale/internal/smtpclient"
)

func TestJSONBuilderDeterminismAndMIME(t *testing.T) {
	a, err := decodeJSONMessage([]byte(`{"date":"2026-10-07T12:00:00Z","subject":"中文主题","from_name":"网站通知","text":"你好\n正文","html":"<p>你好</p>"}`))
	if err != nil {
		t.Fatal(err)
	}
	first, err := renderJSONMessage(a, "sender@example.test", []string{"one@example.test", "two@example.test"}, "identity", "key")
	if err != nil {
		t.Fatal(err)
	}
	a.Date = "2026-10-07T14:00:00+02:00"
	second, _ := renderJSONMessage(a, "sender@example.test", []string{"one@example.test", "two@example.test"}, "identity", "key")
	if !bytes.Equal(first, second) {
		t.Fatal("normalized date changed MIME")
	}
	golden := sha256.Sum256(first)
	if hex.EncodeToString(golden[:]) != "998df581a92eb799c89e0f4192f98688576166b5f46f2b554b1a701044b6dc3f" {
		t.Fatalf("json-v1 byte compatibility fixture: %x", golden)
	}
	if err = smtpclient.ValidateCanonical(context.Background(), bytes.NewReader(first)); err != nil {
		t.Fatal(err)
	}
	m, err := mail.ReadMessage(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject"))
	if err != nil || subject != "中文主题" {
		t.Fatal(subject, err)
	}
	from, err := mail.ParseAddress(m.Header.Get("From"))
	if err != nil || from.Name != "网站通知" {
		t.Fatal(from, err)
	}
	_, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil || len(params["boundary"]) > 70 {
		t.Fatal(params, err)
	}
	reader := multipart.NewReader(m.Body, params["boundary"])
	for _, want := range []string{"你好\n正文", "<p>你好</p>"} {
		part, e := reader.NextPart()
		if e != nil {
			t.Fatal(e)
		}
		body, e := io.ReadAll(base64.NewDecoder(base64.StdEncoding, part))
		if e != nil || string(body) != want {
			t.Fatal(string(body), e)
		}
	}
	if _, err = reader.NextPart(); err != io.EOF {
		t.Fatal(err)
	}
	a.Text = "different"
	changed, _ := renderJSONMessage(a, "sender@example.test", []string{"one@example.test", "two@example.test"}, "identity", "key")
	if bytes.Equal(first, changed) {
		t.Fatal("content change omitted")
	}
}
func TestJSONBuilderClosedInput(t *testing.T) {
	for _, raw := range []string{`{"date":"a","date":"b"}`, `{"text":null}`, `{"text":[]}`, `{"text":{}}`, `{"unknown":"x"}`, `{} {}`, `{"text":true}`, string([]byte{'{', '"', 't', 'e', 'x', 't', '"', ':', '"', 255, '"', '}'})} {
		if _, err := decodeJSONMessage([]byte(raw)); err == nil {
			t.Fatal(raw)
		}
	}
	valid := jsonMessage{Date: "2026-10-07T12:00:00Z", Text: "hello"}
	for _, mutate := range []func(*jsonMessage){func(v *jsonMessage) { v.Subject = "injection\r\nBcc: x" }, func(v *jsonMessage) { v.FromName = "bad\nname" }, func(v *jsonMessage) { v.ReplyTo = "a@example.test\r\nX: x" }, func(v *jsonMessage) { v.Text = "" }, func(v *jsonMessage) { v.Date = "2026-10-07T12:00:00.1Z" }, func(v *jsonMessage) { v.HTML = strings.Repeat("x", (16<<10)+1) }} {
		v := valid
		mutate(&v)
		if _, err := renderJSONMessage(v, "sender@example.test", []string{"one@example.test"}, "identity", "key"); err == nil {
			t.Fatal(v)
		}
	}
}
