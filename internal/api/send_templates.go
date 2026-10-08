package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

// VersionTemplateJSON prepares a local configuration artifact, not a database
// publication. A supplied version must match; omission calculates a new version.
func VersionTemplateJSON(raw []byte) (SendTemplate, error) {
	var t SendTemplate
	if len(raw) > 32<<10 || !utf8.Valid(raw) || uniqueJSON(json.NewDecoder(bytes.NewReader(raw))) != nil {
		return t, errJSONMessage
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&t); err != nil {
		return t, errJSONMessage
	}
	if t.Version == "" {
		t.Version = TemplateVersion(t)
	}
	if err := prepareTemplates([]SendTemplate{t}); err != nil {
		return t, err
	}
	return t, nil
}

// Templates are explicit immutable, content-addressed startup configuration.
type SendTemplate struct {
	ID        string   `json:"id"`
	Version   string   `json:"version"`
	Subject   string   `json:"subject"`
	Text      string   `json:"text"`
	HTML      string   `json:"html"`
	Variables []string `json:"variables"`
	compiled  *template.Template
}

type jsonTemplateReference struct {
	ID        string            `json:"id"`
	Version   string            `json:"version"`
	Variables map[string]string `json:"variables"`
}

func templateName(s string) bool {
	if len(s) < 1 || len(s) > 32 {
		return false
	}
	for i, c := range s {
		if c >= 'a' && c <= 'z' || i > 0 && (c >= '0' && c <= '9' || c == '_') {
			continue
		}
		return false
	}
	return true
}

// TemplateVersion hashes length-prefixed UTF-8 fields with a fixed namespace;
// Variables are a sorted set. It does not include the supplied Version field.
func TemplateVersion(t SendTemplate) string {
	h := sha256.New()
	h.Write([]byte("relaytale.template.v1\x00"))
	names := append([]string(nil), t.Variables...)
	sort.Strings(names)
	for _, s := range append([]string{t.ID, t.Subject, t.Text, t.HTML}, names...) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(s)))
		h.Write(length[:])
		h.Write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Only literal {{name}} substitutions are allowed, never Go control flow,
// function calls, imports or recursive template definitions.
func templateSource(source string, names map[string]bool, used map[string]bool, html bool) (string, error) {
	var b strings.Builder
	for {
		before, after, found := strings.Cut(source, "{{")
		if strings.Contains(before, "}}") {
			return "", errJSONMessage
		}
		b.WriteString(before)
		if !found {
			break
		}
		name, rest, closed := strings.Cut(after, "}}")
		if !closed || !names[name] {
			return "", errJSONMessage
		}
		used[name] = true
		if html {
			b.WriteString("{{index . \"" + name + "\"}}")
		} else {
			b.WriteString("{{" + name + "}}")
		}
		source = rest
	}
	return b.String(), nil
}

func prepareTemplates(catalog []SendTemplate) error {
	if len(catalog) > 8 {
		return errJSONMessage
	}
	seen := map[string]bool{}
	for i := range catalog {
		t := &catalog[i]
		if !templateName(t.ID) || t.Version != TemplateVersion(*t) || seen[t.ID+":"+t.Version] || !safeJSONHeader(t.Subject, 256) || len(t.Text) > 16<<10 || len(t.HTML) > 16<<10 || !utf8.ValidString(t.Text) || !utf8.ValidString(t.HTML) || t.Text == "" && t.HTML == "" || len(t.Variables) > 16 {
			return errJSONMessage
		}
		seen[t.ID+":"+t.Version] = true
		names, used := map[string]bool{}, map[string]bool{}
		for _, name := range t.Variables {
			if !templateName(name) || names[name] {
				return errJSONMessage
			}
			names[name] = true
		}
		if _, err := templateSource(t.Subject, names, used, false); err != nil {
			return err
		}
		if _, err := templateSource(t.Text, names, used, false); err != nil {
			return err
		}
		source, err := templateSource(t.HTML, names, used, true)
		if err != nil {
			return err
		}
		if len(used) != len(names) {
			return errJSONMessage
		}
		t.compiled, err = template.New("body").Option("missingkey=error").Parse(source)
		if err != nil {
			return errJSONMessage
		}
		// Execute at publication time too: reject invalid HTML contexts eagerly.
		dummy := map[string]string{}
		for name := range names {
			dummy[name] = "value"
		}
		if err = t.compiled.Execute(io.Discard, dummy); err != nil {
			return errJSONMessage
		}
	}
	return nil
}

func templateVersions(catalog []SendTemplate) []map[string]string {
	out := make([]map[string]string, 0, len(catalog))
	for _, t := range catalog {
		out = append(out, map[string]string{"id": t.ID, "version": t.Version})
	}
	return out
}

func decodeJSONTemplate(d *json.Decoder) (*jsonTemplateReference, error) {
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return nil, errJSONMessage
	}
	out := &jsonTemplateReference{}
	seen := map[string]bool{}
	for d.More() {
		t, err = d.Token()
		key, ok := t.(string)
		if err != nil || !ok || seen[key] {
			return nil, errJSONMessage
		}
		seen[key] = true
		if key == "variables" {
			out.Variables, err = decodeTemplateVariables(d)
			if err != nil {
				return nil, err
			}
			continue
		}
		t, err = d.Token()
		value, ok := t.(string)
		if err != nil || !ok {
			return nil, errJSONMessage
		}
		switch key {
		case "id":
			out.ID = value
		case "version":
			out.Version = value
		default:
			return nil, errJSONMessage
		}
	}
	t, err = d.Token()
	if err != nil || t != json.Delim('}') || len(seen) != 3 {
		return nil, errJSONMessage
	}
	return out, nil
}

func decodeTemplateVariables(d *json.Decoder) (map[string]string, error) {
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return nil, errJSONMessage
	}
	out := map[string]string{}
	total := 0
	for d.More() {
		t, err = d.Token()
		key, ok := t.(string)
		if err != nil || !ok || !templateName(key) || len(out) >= 16 {
			return nil, errJSONMessage
		}
		if _, exists := out[key]; exists {
			return nil, errJSONMessage
		}
		t, err = d.Token()
		value, ok := t.(string)
		if err != nil || !ok || len(value) > 1024 {
			return nil, errJSONMessage
		}
		total += len(value)
		if total > 8192 {
			return nil, errJSONMessage
		}
		out[key] = value
	}
	t, err = d.Token()
	if err != nil || t != json.Delim('}') {
		return nil, errJSONMessage
	}
	return out, nil
}

type templateOutput struct{ strings.Builder }

func (b *templateOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 16<<10 {
		return 0, errJSONMessage
	}
	return b.Builder.Write(p)
}

func resolveJSONTemplate(v jsonMessage, catalog []SendTemplate) (jsonMessage, error) {
	if v.Template == nil {
		return v, nil
	}
	ref := v.Template
	for _, t := range catalog {
		if t.ID != ref.ID || t.Version != ref.Version {
			continue
		}
		if len(ref.Variables) != len(t.Variables) || t.compiled == nil {
			return v, errJSONMessage
		}
		for _, name := range t.Variables {
			if _, ok := ref.Variables[name]; !ok {
				return v, errJSONMessage
			}
		}
		// A single pass prevents inserted values from becoming template syntax.
		expand := func(source string) (string, error) {
			var b templateOutput
			for {
				before, after, found := strings.Cut(source, "{{")
				if _, err := b.Write([]byte(before)); err != nil {
					return "", err
				}
				if !found {
					break
				}
				name, rest, _ := strings.Cut(after, "}}")
				if _, err := b.Write([]byte(ref.Variables[name])); err != nil {
					return "", err
				}
				source = rest
			}
			return b.String(), nil
		}
		var err error
		v.Subject, err = expand(t.Subject)
		if err != nil {
			return v, err
		}
		v.Text, err = expand(t.Text)
		if err != nil {
			return v, err
		}
		var html templateOutput
		if err = t.compiled.Execute(&html, ref.Variables); err != nil {
			return v, errJSONMessage
		}
		v.HTML = html.String()
		if strings.Contains(v.HTML, "#ZgotmplZ") {
			return v, errJSONMessage
		}
		return v, nil
	}
	return v, errJSONMessage
}
