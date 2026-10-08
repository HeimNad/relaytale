package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func templateFixture(t *testing.T) SendTemplate {
	t.Helper()
	v := SendTemplate{ID: "order", Subject: "Order {{name}}", Text: "Hello {{name}}", HTML: `<p>{{name}}</p><a href="{{url}}">Details</a>`, Variables: []string{"url", "name"}}
	v.Version = TemplateVersion(v)
	if err := prepareTemplates([]SendTemplate{v}); err != nil {
		t.Fatal(err)
	}
	catalog := []SendTemplate{v}
	if err := prepareTemplates(catalog); err != nil {
		t.Fatal(err)
	}
	return catalog[0]
}
func TestTemplateVersionsAndConfiguration(t *testing.T) {
	v := templateFixture(t)
	reordered := v
	reordered.Variables = []string{"name", "url"}
	if TemplateVersion(reordered) != v.Version {
		t.Fatal("variable declaration order changed version")
	}
	changed := v
	changed.Text = "changed"
	if TemplateVersion(changed) == v.Version {
		t.Fatal("content change preserved version")
	}
	key := SendKey{ID: uuid.NewString(), Token: "rts_" + strings.Repeat("a", 64), AllowedFrom: []string{"sender@example.test"}, Templates: []SendTemplate{v}}
	raw, _ := json.Marshal([]SendKey{key})
	if _, err := ParseSendKeys(string(raw)); err != nil {
		t.Fatal(err)
	}
	key.Templates[0].Text = "changed"
	raw, _ = json.Marshal([]SendKey{key})
	if _, err := ParseSendKeys(string(raw)); err == nil {
		t.Fatal("edited fixed version accepted")
	}
	for _, mutate := range []func(*SendTemplate){func(v *SendTemplate) { v.Text = "{{range .}}x{{end}}" }, func(v *SendTemplate) { v.ID = "../order" }, func(v *SendTemplate) { v.Variables = append(v.Variables, "unused") }, func(v *SendTemplate) { v.Variables = append(v.Variables, "name") }, func(v *SendTemplate) { v.HTML = `<a href="{{url}}` }, func(v *SendTemplate) { v.Subject = "bad\nheader" }} {
		bad := v
		bad.Variables = append([]string(nil), v.Variables...)
		mutate(&bad)
		bad.Version = TemplateVersion(bad)
		if err := prepareTemplates([]SendTemplate{bad}); err == nil {
			t.Fatal("invalid catalog accepted", bad.ID)
		}
	}
	if err := prepareTemplates([]SendTemplate{v, v}); err == nil {
		t.Fatal("duplicate fixed version")
	}
	local, err := VersionTemplateJSON([]byte(`{"id":"notice","subject":"Hi {{name}}","text":"Hello {{name}}","variables":["name"]}`))
	if err != nil || local.Version != TemplateVersion(local) {
		t.Fatal(err)
	}
	for _, bad := range []string{`{"id":"one","id":"two","text":"hello","variables":[]}`, `{"id":"notice","text":"hello","variables":[],"url":"https://example.test"}`, `{} {}`} {
		if _, err = VersionTemplateJSON([]byte(bad)); err == nil {
			t.Fatal(bad)
		}
	}
}
func TestTemplateRenderingEscapesAndIsDeterministic(t *testing.T) {
	catalog := []SendTemplate{templateFixture(t)}
	raw := `{"date":"2026-10-07T12:00:00Z","template":{"id":"order","version":"` + catalog[0].Version + `","variables":{"name":"<b>{{name}}</b>","url":"https://example.test/?a=1&b=2"}}}`
	v, err := decodeJSONMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	v, err = resolveJSONTemplate(v, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v.HTML, "&lt;b&gt;{{name}}&lt;/b&gt;") || !strings.Contains(v.HTML, "&amp;b=2") || v.Text != "Hello <b>{{name}}</b>" {
		t.Fatal(v.HTML, v.Text)
	}
	a, err := renderJSONMessage(v, "sender@example.test", []string{"one@example.test"}, "identity", "key")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(a)) != "c84dd405d82a0dea0586ab373d6bdc5de5efbd02254560224474355b01c1e925" {
		t.Fatal("json-v3 byte compatibility changed")
	}
	if !bytes.Contains(a, []byte("X-RelayTale-Builder: json-v3")) || !bytes.Contains(a, []byte(catalog[0].Version)) {
		t.Fatal("missing version evidence")
	}
	again, err := decodeJSONMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	again, err = resolveJSONTemplate(again, catalog)
	if err != nil {
		t.Fatal(err)
	}
	b, err := renderJSONMessage(again, "sender@example.test", []string{"one@example.test"}, "identity", "key")
	if err != nil || !bytes.Equal(a, b) {
		t.Fatal("unstable MIME", err)
	}
	v.Template.Variables["url"] = "javascript:alert(1)"
	if _, err = resolveJSONTemplate(v, catalog); err == nil {
		t.Fatal("unsafe URL accepted")
	}
}
func TestTemplateRequestsFailClosedAndBoundExpansion(t *testing.T) {
	v := templateFixture(t)
	for _, raw := range []string{`{"template":null}`, `{"template":{"id":"x","id":"y","version":"z","variables":{}}}`, `{"template":{"id":"x","version":"z","variables":{"name":null}}}`, `{"template":{"id":"x","version":"z","variables":{"name":"a","name":"b"}}}`, `{"template":{"id":"x","version":"z","variables":{},"text":"x"}}`, `{"subject":"","template":{"id":"x","version":"z","variables":{}}}`} {
		if _, err := decodeJSONMessage([]byte(raw)); err == nil {
			t.Fatal(raw)
		}
	}
	for _, ref := range []*jsonTemplateReference{{ID: v.ID, Version: v.Version, Variables: map[string]string{"name": "x"}}, {ID: v.ID, Version: v.Version, Variables: map[string]string{"name": "x", "url": "https://example.test", "extra": "x"}}, {ID: v.ID, Version: strings.Repeat("0", 64), Variables: map[string]string{"name": "x", "url": "https://example.test"}}} {
		if _, err := resolveJSONTemplate(jsonMessage{Template: ref}, []SendTemplate{v}); err == nil {
			t.Fatal("invalid reference")
		}
	}
	if _, err := resolveJSONTemplate(jsonMessage{Template: &jsonTemplateReference{ID: v.ID, Version: v.Version, Variables: map[string]string{}}}, nil); err == nil {
		t.Fatal("identity catalog bypass")
	}
	bomb := SendTemplate{ID: "large", Text: strings.Repeat("{{name}}", 100), Variables: []string{"name"}}
	bomb.Version = TemplateVersion(bomb)
	catalog := []SendTemplate{bomb}
	if err := prepareTemplates(catalog); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveJSONTemplate(jsonMessage{Template: &jsonTemplateReference{ID: bomb.ID, Version: bomb.Version, Variables: map[string]string{"name": strings.Repeat("x", 1024)}}}, catalog); err == nil {
		t.Fatal("unbounded expansion")
	}
}

func TestTemplateConcurrentRenderingAndVariableBounds(t *testing.T) {
	catalog := []SendTemplate{templateFixture(t)}
	raw := `{"date":"2026-10-08T12:00:00Z","template":{"id":"order","version":"` + catalog[0].Version + `","variables":{"name":"customer","url":"https://example.test"}}}`
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := decodeJSONMessage([]byte(raw))
			if err != nil {
				t.Error(err)
				return
			}
			v, err = resolveJSONTemplate(v, catalog)
			if err != nil || v.Text != "Hello customer" {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	huge := strings.Replace(raw, "customer", strings.Repeat("x", 1025), 1)
	if _, err := decodeJSONMessage([]byte(huge)); err == nil {
		t.Fatal("oversize variable")
	}
	values := map[string]string{}
	for i := 0; i < 9; i++ {
		values[fmt.Sprintf("v%d", i)] = strings.Repeat("x", 1024)
	}
	b, _ := json.Marshal(values)
	huge = `{"template":{"id":"x","version":"x","variables":` + string(b) + `}}`
	if _, err := decodeJSONMessage([]byte(huge)); err == nil {
		t.Fatal("aggregate variable budget")
	}
}
