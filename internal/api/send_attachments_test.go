package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"

	"relaytale/internal/smtpclient"
)

func TestJSONAttachmentsMIMEAndCompatibility(t *testing.T) {
	raw := `{"date":"2026-10-07T12:00:00Z","text":"hello","html":"<p>hello</p>","attachments":[{"filename":"订单.txt","content_type":"TEXT/PLAIN","content_base64":"AP8NCg=="}]}`
	v, err := decodeJSONMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	eml, err := renderJSONMessage(v, "sender@example.test", []string{"one@example.test"}, "identity", "key")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(eml)) != "bc29fbcfb27c3305d84df06452ad7a6770dab1b39d373b137c70cf216addaf25" {
		t.Fatal("json-v2 byte compatibility changed")
	}
	if err = smtpclient.ValidateCanonical(context.Background(), bytes.NewReader(eml)); err != nil {
		t.Fatal(err)
	}
	m, err := mail.ReadMessage(bytes.NewReader(eml))
	if err != nil {
		t.Fatal(err)
	}
	if m.Header.Get("X-RelayTale-Builder") != "json-v2" {
		t.Fatal(m.Header)
	}
	kind, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil || kind != "multipart/mixed" {
		t.Fatal(kind, err)
	}
	r := multipart.NewReader(m.Body, params["boundary"])
	body, err := r.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	kind, params, err = mime.ParseMediaType(body.Header.Get("Content-Type"))
	if err != nil || kind != "multipart/alternative" {
		t.Fatal(kind, err)
	}
	alternative := multipart.NewReader(body, params["boundary"])
	for _, want := range []string{"hello", "<p>hello</p>"} {
		p, e := alternative.NextPart()
		if e != nil {
			t.Fatal(e)
		}
		b, e := io.ReadAll(base64.NewDecoder(base64.StdEncoding, p))
		if e != nil || string(b) != want {
			t.Fatal(string(b), e)
		}
	}
	a, err := r.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	disposition, p, err := mime.ParseMediaType(a.Header.Get("Content-Disposition"))
	if err != nil || disposition != "attachment" || p["filename"] != "订单.txt" {
		t.Fatal(p, err)
	}
	b, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, a))
	if err != nil || !bytes.Equal(b, []byte{0, 255, 13, 10}) {
		t.Fatal(b, err)
	}
	if _, err = r.NextPart(); err != io.EOF {
		t.Fatal(err)
	}
	v.Attachments[0].ContentType = "text/plain"
	again, _ := renderJSONMessage(v, "sender@example.test", []string{"one@example.test"}, "identity", "key")
	if !bytes.Equal(eml, again) {
		t.Fatal("canonical type changed MIME")
	}
	v.Attachments = nil
	old, _ := renderJSONMessage(v, "sender@example.test", []string{"one@example.test"}, "identity", "key")
	v.Attachments = []jsonAttachment{}
	empty, _ := renderJSONMessage(v, "sender@example.test", []string{"one@example.test"}, "identity", "key")
	if !bytes.Equal(old, empty) {
		t.Fatal("empty array changed v1 MIME")
	}
}

func TestJSONAttachmentsRejectUnsafeInput(t *testing.T) {
	for _, a := range []string{`null`, `{}`, `[null]`, `[{"filename":"x","filename":"y"}]`, `[{"filename":"x","content_type":"text/plain","content_base64":"","url":"https://example.test"}]`, `[{"filename":"x","content_type":[],"content_base64":""}]`} {
		if _, err := decodeJSONMessage([]byte(`{"attachments":` + a + `}`)); err == nil {
			t.Fatal(a)
		}
	}
	valid := jsonAttachment{Filename: "invoice.pdf", ContentType: "application/pdf", ContentBase64: "eA=="}
	for _, mutate := range []func(*jsonAttachment){
		func(a *jsonAttachment) { a.Filename = "../invoice.pdf" }, func(a *jsonAttachment) { a.Filename = "C:\\invoice.pdf" }, func(a *jsonAttachment) { a.Filename = "bad\r\nX: y" }, func(a *jsonAttachment) { a.Filename = "" }, func(a *jsonAttachment) { a.ContentType = "multipart/mixed" }, func(a *jsonAttachment) { a.ContentType = "message/rfc822" }, func(a *jsonAttachment) { a.ContentType = "text/plain; charset=utf-8" }, func(a *jsonAttachment) { a.ContentBase64 = "eB==" }, func(a *jsonAttachment) { a.ContentBase64 = "eA==\n" }, func(a *jsonAttachment) {
			a.ContentBase64 = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", (16<<10)+1)))
		},
	} {
		a := valid
		mutate(&a)
		if err := normalizeJSONAttachments([]jsonAttachment{a}); err == nil {
			t.Fatal(a)
		}
	}
	if err := normalizeJSONAttachments([]jsonAttachment{valid, valid, valid, valid, valid}); err == nil {
		t.Fatal("count")
	}
	a := valid
	a.ContentBase64 = base64.StdEncoding.EncodeToString(make([]byte, 9<<10))
	if err := normalizeJSONAttachments([]jsonAttachment{a, a}); err == nil {
		t.Fatal("aggregate")
	}
}

func TestJSONAttachmentsLimitsAndBodyVariants(t *testing.T) {
	a := jsonAttachment{Filename: "empty.bin", ContentType: "application/octet-stream"}
	if err := normalizeJSONAttachments([]jsonAttachment{a, a, a, a}); err != nil {
		t.Fatal(err)
	}
	a.ContentBase64 = base64.StdEncoding.EncodeToString(make([]byte, 16<<10))
	if err := normalizeJSONAttachments([]jsonAttachment{a}); err != nil {
		t.Fatal(err)
	}
	for _, html := range []bool{false, true} {
		v := jsonMessage{Date: "2026-10-07T12:00:00Z", Attachments: []jsonAttachment{a}}
		if html {
			v.HTML = "<p>hello</p>"
		} else {
			v.Text = "hello"
		}
		eml, err := renderJSONMessage(v, "sender@example.test", []string{"one@example.test"}, "identity", "key")
		if err != nil {
			t.Fatal(err)
		}
		if err = smtpclient.ValidateCanonical(context.Background(), bytes.NewReader(eml)); err != nil {
			t.Fatal(err)
		}
		m, err := mail.ReadMessage(bytes.NewReader(eml))
		if err != nil {
			t.Fatal(err)
		}
		_, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
		if err != nil {
			t.Fatal(err)
		}
		r := multipart.NewReader(m.Body, params["boundary"])
		if _, err = r.NextPart(); err != nil {
			t.Fatal(err)
		}
		part, err := r.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, part))
		if err != nil || len(data) != 16<<10 {
			t.Fatal(len(data), err)
		}
	}
}
