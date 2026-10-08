package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/mail"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"relaytale/internal/auth"
	"relaytale/internal/storage"
)

const jsonMessageLimit = 32 << 10

var errJSONMessage = errors.New("invalid JSON message")

type jsonMessage struct {
	Date        string           `json:"date"`
	Subject     string           `json:"subject"`
	FromName    string           `json:"from_name"`
	ReplyTo     string           `json:"reply_to"`
	Text        string           `json:"text"`
	HTML        string           `json:"html"`
	Attachments []jsonAttachment `json:"attachments,omitempty"`
}

// Lazy construction runs only when APIReceiver has acquired its resource budget.
// It adds no outer reservation which could deadlock a small memory budget.
type jsonMessageReader struct {
	body          io.Reader
	from          string
	recipients    []string
	identity, key string
	rendered      *bytes.Reader
	initialized   bool
}

func (r *jsonMessageReader) Read(p []byte) (int, error) {
	if !r.initialized {
		r.initialized = true
		raw, err := io.ReadAll(io.LimitReader(r.body, jsonMessageLimit+1))
		if len(raw) > jsonMessageLimit {
			return 0, storage.ErrTooLarge
		}
		if err != nil {
			return 0, err
		}
		value, err := decodeJSONMessage(raw)
		if err != nil {
			return 0, err
		}
		eml, err := renderJSONMessage(value, r.from, r.recipients, r.identity, r.key)
		if err != nil {
			return 0, err
		}
		r.rendered = bytes.NewReader(eml)
	}
	if r.rendered == nil {
		return 0, errJSONMessage
	}
	return r.rendered.Read(p)
}
func decodeJSONMessage(raw []byte) (jsonMessage, error) {
	var out jsonMessage
	if !utf8.Valid(raw) {
		return out, errJSONMessage
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return out, errJSONMessage
	}
	seen := map[string]bool{}
	for d.More() {
		k, e := d.Token()
		key, ok := k.(string)
		if e != nil || !ok || seen[key] {
			return out, errJSONMessage
		}
		seen[key] = true
		if key == "attachments" {
			out.Attachments, err = decodeJSONAttachments(d)
			if err != nil {
				return out, err
			}
			continue
		}
		v, e := d.Token()
		value, ok := v.(string)
		if e != nil || !ok {
			return out, errJSONMessage
		}
		switch key {
		case "date":
			out.Date = value
		case "subject":
			out.Subject = value
		case "from_name":
			out.FromName = value
		case "reply_to":
			out.ReplyTo = value
		case "text":
			out.Text = value
		case "html":
			out.HTML = value
		default:
			return out, errJSONMessage
		}
	}
	token, err = d.Token()
	if err != nil || token != json.Delim('}') {
		return out, errJSONMessage
	}
	if _, err = d.Token(); err != io.EOF {
		return out, errJSONMessage
	}
	return out, nil
}
func safeJSONHeader(s string, limit int) bool {
	if !utf8.ValidString(s) || len(s) > limit {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}
func renderJSONMessage(v jsonMessage, from string, to []string, identity, key string) ([]byte, error) {
	date, err := time.Parse(time.RFC3339, v.Date)
	date = date.UTC()
	if err != nil || date.Year() < 1970 || date.Year() > 9999 || date.Nanosecond() != 0 || !safeJSONHeader(v.Subject, 256) || !safeJSONHeader(v.FromName, 128) || !safeJSONHeader(v.ReplyTo, 254) || len(v.Text) > 16<<10 || len(v.HTML) > 16<<10 || (v.Text == "" && v.HTML == "") {
		return nil, errJSONMessage
	}
	v.Date = date.UTC().Format(time.RFC3339)
	if v.ReplyTo != "" {
		v.ReplyTo, err = auth.CanonicalAddress(v.ReplyTo)
		if err != nil {
			return nil, errJSONMessage
		}
	}
	version := 1
	if len(v.Attachments) > 0 {
		version = 2
		if err := normalizeJSONAttachments(v.Attachments); err != nil {
			return nil, err
		}
	}
	canonical, _ := json.Marshal(struct {
		Version       int
		Message       jsonMessage
		From          string
		To            []string
		Identity, Key string
	}{version, v, from, to, identity, key})
	digest := sha256.Sum256(canonical)
	tag := hex.EncodeToString(digest[:])
	var b strings.Builder
	b.WriteString("Date: " + date.UTC().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("From: " + (&mail.Address{Name: v.FromName, Address: from}).String() + "\r\n")
	b.WriteString("To: ")
	for i, address := range to {
		if i > 0 {
			b.WriteString(",\r\n ")
		}
		b.WriteString((&mail.Address{Address: address}).String())
	}
	b.WriteString("\r\nSubject: " + mime.QEncoding.Encode("utf-8", v.Subject) + "\r\n")
	builder := "json-v1"
	if version == 2 {
		builder = "json-v2"
	}
	b.WriteString("Message-ID: <" + tag + "@relaytale.invalid>\r\nX-RelayTale-Builder: " + builder + "\r\nMIME-Version: 1.0\r\n")
	if v.ReplyTo != "" {
		b.WriteString("Reply-To: " + (&mail.Address{Address: v.ReplyTo}).String() + "\r\n")
	}
	part := func(kind, content string) {
		b.WriteString("Content-Type: text/" + kind + "; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n")
		encoded := base64.StdEncoding.EncodeToString([]byte(content))
		for len(encoded) > 76 {
			b.WriteString(encoded[:76] + "\r\n")
			encoded = encoded[76:]
		}
		b.WriteString(encoded + "\r\n")
	}
	mixed := "relaytale-json-v2-mixed-" + tag[:40]
	if version == 2 {
		b.WriteString("Content-Type: multipart/mixed;\r\n boundary=\"" + mixed + "\"\r\n\r\n--" + mixed + "\r\n")
	}
	if v.Text != "" && v.HTML != "" {
		boundary := "relaytale-json-v1-" + tag[:40]
		b.WriteString("Content-Type: multipart/alternative;\r\n boundary=\"" + boundary + "\"\r\n\r\n--" + boundary + "\r\n")
		part("plain", v.Text)
		b.WriteString("--" + boundary + "\r\n")
		part("html", v.HTML)
		b.WriteString("--" + boundary + "--\r\n")
	} else if v.Text != "" {
		part("plain", v.Text)
	} else {
		part("html", v.HTML)
	}
	if version == 2 {
		for _, a := range v.Attachments {
			b.WriteString("--" + mixed + "\r\nContent-Type: " + a.ContentType + "\r\n")
			b.WriteString("Content-Disposition: " + mime.FormatMediaType("attachment", map[string]string{"filename": a.Filename}) + "\r\nContent-Transfer-Encoding: base64\r\n\r\n")
			encoded := a.ContentBase64
			for len(encoded) > 76 {
				b.WriteString(encoded[:76] + "\r\n")
				encoded = encoded[76:]
			}
			b.WriteString(encoded + "\r\n")
		}
		b.WriteString("--" + mixed + "--\r\n")
	}
	return []byte(b.String()), nil
}
