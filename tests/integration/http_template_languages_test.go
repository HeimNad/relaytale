package integration

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"relaytale/internal/api"
	"relaytale/internal/testsmtp"
)

func TestTemplateLanguageSubmissionNoFallback(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, r := sendFixture(t, f)
	en := api.SendTemplate{ID: "notice", Language: "en", Text: "Hello {{name}}", Variables: []string{"name"}}
	en.Version = api.TemplateVersion(en)
	zh := api.SendTemplate{ID: "notice", Language: "zh-cn", Text: "你好 {{name}}", Variables: []string{"name"}}
	zh.Version = api.TemplateVersion(zh)
	h := templateHandler(t, key, r, []api.SendTemplate{en, zh})
	body := `{"date":"2026-10-08T12:00:00Z","template":{"id":"notice","version":"` + zh.Version + `","language":"zh-cn","variables":{"name":"customer"}}}`
	w := jsonSubmit(h, key, body, "language", nil)
	requireStatus(t, w, 202)
	var result struct {
		MessageID string `json:"message_id"`
		Replayed  bool   `json:"replayed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var path string
	if err := f.db.QueryRow("SELECT eml_path FROM messages WHERE id=$1", result.MessageID).Scan(&path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), "Content-Language: zh-cn\r\n") {
		t.Fatal(err)
	}
	requireStatus(t, jsonSubmit(h, key, strings.Replace(body, `,"language":"zh-cn"`, "", 1), "no-language", nil), 400)
	requireStatus(t, jsonSubmit(h, key, strings.Replace(body, `"language":"zh-cn"`, `"language":"zh"`, 1), "no-fallback", nil), 400)
	english := strings.Replace(strings.Replace(body, zh.Version, en.Version, 1), `"language":"zh-cn"`, `"language":"en"`, 1)
	requireStatus(t, jsonSubmit(h, key, english, "language", nil), 409)
	w = jsonSubmit(h, key, body, "language", nil)
	requireStatus(t, w, 202)
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || !result.Replayed {
		t.Fatal(err)
	}
	assertCount(t, f, 1, "SELECT count(*) FROM messages")
	runOne(t, f.worker)
	assertCount(t, f, 1, "SELECT count(*) FROM messages WHERE status='SMTP_ACCEPTED'")
}
