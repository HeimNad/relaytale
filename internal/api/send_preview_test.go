package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestTemplateLanguageVersionAndExactSelection(t *testing.T) {
	old := templateFixture(t)
	language := old
	language.Language = "zh-cn"
	language.Version = TemplateVersion(language)
	if language.Version == old.Version {
		t.Fatal("language not bound to version")
	}
	catalog := []SendTemplate{old, language}
	if err := prepareTemplates(catalog); err != nil {
		t.Fatal(err)
	}
	ref := &jsonTemplateReference{ID: language.ID, Version: language.Version, Language: "zh-cn", Variables: map[string]string{"name": "客户", "url": "https://example.test"}}
	v, err := resolveJSONTemplate(jsonMessage{Date: "2026-10-08T12:00:00Z", Template: ref}, catalog)
	if err != nil {
		t.Fatal(err)
	}
	eml, err := renderJSONMessage(v, "sender@example.test", []string{"one@example.test"}, "identity", "key")
	if err != nil || !bytes.Contains(eml, []byte("Content-Language: zh-cn\r\n")) || !bytes.Contains(eml, []byte("X-RelayTale-Builder: json-v4")) {
		t.Fatal(err)
	}
	golden := sha256.Sum256(eml)
	if hex.EncodeToString(golden[:]) != "e193fc5936d94ad9f9bbc724a6b737c1ce215a49c21e227c7f0ed82d8c23f66b" {
		t.Fatal("json-v4 bytes changed")
	}
	for _, wrong := range []string{"", "en", "zh"} {
		copy := *ref
		copy.Language = wrong
		if _, err := resolveJSONTemplate(jsonMessage{Template: &copy}, catalog); err == nil {
			t.Fatal("implicit language fallback", wrong)
		}
	}
	for _, tag := range []string{"en-US", "zh_cn", "-en", "en-", "en--us", "en\r\nX: y", strings.Repeat("a", 36)} {
		if templateLanguage(tag) {
			t.Fatal(tag)
		}
	}
	language.Version = old.Version
	if err := prepareTemplates([]SendTemplate{language}); err == nil {
		t.Fatal("language changed old version")
	}
}
func TestTemplatePreviewIsBoundedJSONSource(t *testing.T) {
	source := []byte(`{"id":"notice","language":"zh-cn","subject":"Hello {{name}}","text":"Hi {{name}}","html":"<p>{{name}}</p>","variables":["name"]}`)
	output, err := PreviewTemplateJSON(source, []byte(`{"name":"<script>alert(1)</script>"}`))
	if err != nil {
		t.Fatal(err)
	}
	if output["dry_run"] != true || output["language"] != "zh-cn" || strings.Contains(output["html"].(string), "<script>") || !strings.Contains(output["html"].(string), "&lt;script&gt;") {
		t.Fatal(output)
	}
	encoded, err := json.Marshal(output)
	if err != nil || bytes.Contains(encoded, []byte("<script>")) {
		t.Fatal("unsafe serialized preview", err)
	}
	for _, variables := range []string{`{"name":"a","name":"b"}`, `{"name":null}`, `{"name":"a"} {}`, `{"other":"a"}`, `{"name":"bad\nheader"}`, `{"name":"` + strings.Repeat("x", 1025) + `"}`, strings.Repeat("x", (16<<10)+1)} {
		if _, err = PreviewTemplateJSON(source, []byte(variables)); err == nil {
			t.Fatal("invalid preview variables")
		}
	}
	if _, err = PreviewTemplateJSON([]byte(`{"id":"notice","version":"wrong","text":"hello"}`), []byte(`{}`)); err == nil {
		t.Fatal("wrong version preview")
	}
}
