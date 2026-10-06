// Package mimeinspect describes bounded MIME without rendering or extracting bodies.
package mimeinspect

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strconv"
	"strings"
	"unicode/utf8"
)

const MaxBytes = 2 << 20
const MemoryBudget = 32 << 20
const maxHeader = 64 << 10
const maxParts = 64
const maxDepth = 8

var ErrFormat = errors.New("malformed MIME or inspection complexity limit")

type Part struct {
	Path             string `json:"path"`
	MediaType        string `json:"media_type"`
	Disposition      string `json:"disposition,omitempty"`
	Filename         string `json:"filename,omitempty"`
	TransferEncoding string `json:"transfer_encoding"`
	EncodedBytes     int64  `json:"encoded_bytes"`
	DecodedBytes     *int64 `json:"decoded_bytes,omitempty"`
	Opaque           bool   `json:"opaque,omitempty"`
}
type Report struct {
	Parts []Part `json:"parts"`
}

func headerOK(raw []byte) bool {
	end := bytes.Index(raw, []byte("\r\n\r\n"))
	if end < 0 || end > maxHeader {
		return false
	}
	for _, line := range bytes.Split(raw[:end], []byte("\r\n")) {
		if len(line) > 8192 {
			return false
		}
	}
	return true
}
func Inspect(raw []byte) (Report, error) {
	out := Report{Parts: []Part{}}
	if len(raw) == 0 || len(raw) > MaxBytes || !headerOK(raw) {
		return out, ErrFormat
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return out, ErrFormat
	}
	err = walk(textproto.MIMEHeader(msg.Header), msg.Body, "1", 0, &out)
	if err != nil {
		return Report{}, ErrFormat
	}
	return out, nil
}
func walk(h textproto.MIMEHeader, r io.Reader, path string, depth int, out *Report) error {
	if depth > maxDepth || len(out.Parts) >= maxParts {
		return ErrFormat
	}
	for _, name := range []string{"Content-Type", "Content-Disposition", "Content-Transfer-Encoding"} {
		if len(h.Values(name)) > 1 {
			return ErrFormat
		}
	}
	media, params := "text/plain", map[string]string{}
	var err error
	if v := h.Get("Content-Type"); v != "" {
		media, params, err = mime.ParseMediaType(v)
		if err != nil {
			return err
		}
	}
	disposition, filename := "", ""
	if v := h.Get("Content-Disposition"); v != "" {
		var p map[string]string
		disposition, p, err = mime.ParseMediaType(v)
		if err != nil {
			return err
		}
		filename = p["filename"]
	}
	if filename == "" {
		filename = params["name"]
	}
	if len(filename) > 512 || !utf8.ValidString(filename) || strings.ContainsAny(filename, "\x00\r\n") {
		return ErrFormat
	}
	encoding := strings.ToLower(strings.TrimSpace(h.Get("Content-Transfer-Encoding")))
	if encoding == "" {
		encoding = "7bit"
	}
	body, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil || len(body) > MaxBytes {
		return ErrFormat
	}
	part := Part{Path: path, MediaType: media, Disposition: disposition, Filename: filename, TransferEncoding: encoding, EncodedBytes: int64(len(body))}
	index := len(out.Parts)
	out.Parts = append(out.Parts, part)
	if strings.HasPrefix(media, "multipart/") {
		if encoding != "7bit" && encoding != "8bit" && encoding != "binary" {
			return ErrFormat
		}
		boundary := params["boundary"]
		if boundary == "" || len(boundary) > 70 {
			return ErrFormat
		}
		mr := multipart.NewReader(bytes.NewReader(body), boundary)
		count := 0
		for {
			p, err := mr.NextRawPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			// The complete input is capped; additionally bound every part header and count.
			size := 0
			for k, values := range p.Header {
				size += len(k)
				for _, v := range values {
					size += len(v)
					if len(v) > 8192 {
						return ErrFormat
					}
				}
			}
			if size > maxHeader {
				return ErrFormat
			}
			count++
			if err = walk(p.Header, p, path+"."+strconv.Itoa(count), depth+1, out); err != nil {
				return err
			}
		}
		if count == 0 {
			return ErrFormat
		}
		return nil
	}
	var decoded io.Reader = bytes.NewReader(body)
	switch encoding {
	case "base64":
		decoded = base64.NewDecoder(base64.StdEncoding, decoded)
	case "quoted-printable":
		decoded = quotedprintable.NewReader(decoded)
	case "7bit", "8bit", "binary":
	default:
		out.Parts[index].Opaque = true
		return nil
	}
	n, err := io.Copy(io.Discard, io.LimitReader(decoded, MaxBytes+1))
	if err != nil || n > MaxBytes {
		return ErrFormat
	}
	out.Parts[index].DecodedBytes = &n
	// Embedded messages, archives and HTML remain opaque: no recursive extraction,
	// decompression, external fetching or body content is included in the report.
	if media == "message/rfc822" {
		out.Parts[index].Opaque = true
	}
	return nil
}
