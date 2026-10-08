package api

import (
	"encoding/base64"
	"encoding/json"
	"mime"
	"strings"
)

type jsonAttachment struct {
	Filename      string `json:"filename"`
	ContentType   string `json:"content_type"`
	ContentBase64 string `json:"content_base64"`
}

// Explicit token parsing bounds nesting and rejects duplicate/unknown fields.
func decodeJSONAttachments(d *json.Decoder) ([]jsonAttachment, error) {
	t, err := d.Token()
	if err != nil || t != json.Delim('[') {
		return nil, errJSONMessage
	}
	var out []jsonAttachment
	for d.More() {
		if len(out) == 4 {
			return nil, errJSONMessage
		}
		t, err = d.Token()
		if err != nil || t != json.Delim('{') {
			return nil, errJSONMessage
		}
		a := jsonAttachment{}
		seen := map[string]bool{}
		for d.More() {
			t, err = d.Token()
			key, ok := t.(string)
			if err != nil || !ok || seen[key] {
				return nil, errJSONMessage
			}
			seen[key] = true
			t, err = d.Token()
			value, ok := t.(string)
			if err != nil || !ok {
				return nil, errJSONMessage
			}
			switch key {
			case "filename":
				a.Filename = value
			case "content_type":
				a.ContentType = value
			case "content_base64":
				a.ContentBase64 = value
			default:
				return nil, errJSONMessage
			}
		}
		t, err = d.Token()
		if err != nil || t != json.Delim('}') || len(seen) != 3 {
			return nil, errJSONMessage
		}
		out = append(out, a)
	}
	t, err = d.Token()
	if err != nil || t != json.Delim(']') {
		return nil, errJSONMessage
	}
	return out, nil
}

func normalizeJSONAttachments(attachments []jsonAttachment) error {
	if len(attachments) > 4 {
		return errJSONMessage
	}
	total := 0
	for i := range attachments {
		a := &attachments[i]
		if !safeJSONHeader(a.Filename, 120) || strings.TrimSpace(a.Filename) == "" || a.Filename == "." || a.Filename == ".." || strings.ContainsAny(a.Filename, "/\\") || !safeJSONHeader(a.ContentType, 100) {
			return errJSONMessage
		}
		kind, params, err := mime.ParseMediaType(a.ContentType)
		if err != nil || len(params) != 0 || !strings.Contains(kind, "/") || strings.Contains(kind, "*") || strings.HasPrefix(kind, "multipart/") || strings.HasPrefix(kind, "message/") {
			return errJSONMessage
		}
		a.ContentType = kind
		// Reject whitespace and noncanonical padding/trailing bits explicitly.
		if len(a.ContentBase64) > base64.StdEncoding.EncodedLen(16<<10) || strings.ContainsAny(a.ContentBase64, "\r\n") {
			return errJSONMessage
		}
		decoded, err := base64.StdEncoding.Strict().DecodeString(a.ContentBase64)
		if err != nil || base64.StdEncoding.EncodeToString(decoded) != a.ContentBase64 {
			return errJSONMessage
		}
		total += len(decoded)
		if total > 16<<10 {
			return errJSONMessage
		}
	}
	return nil
}
