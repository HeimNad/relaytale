package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"relaytale/internal/api"
	"relaytale/internal/message"
	"relaytale/internal/testsmtp"
)

func templateHandler(t *testing.T, key api.SendKey, r message.APIReceiver, catalog []api.SendTemplate) http.Handler {
	t.Helper()
	key.Templates = catalog
	raw, _ := json.Marshal([]api.SendKey{key})
	h, err := api.NewSendHandler(context.Background(), string(raw), r)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func TestTemplateSubmissionImmutableVersionsAndScope(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, r := sendFixture(t, f)
	old := api.SendTemplate{ID: "order", Subject: "Order {{number}}", Text: "Receipt {{number}}", HTML: "<p>{{number}}</p>", Variables: []string{"number"}}
	old.Version = api.TemplateVersion(old)
	h := templateHandler(t, key, r, []api.SendTemplate{old})
	body := `{"date":"2026-10-08T12:00:00Z","template":{"id":"order","version":"` + old.Version + `","variables":{"number":"<42>"}},"attachments":[{"filename":"receipt.txt","content_type":"text/plain","content_base64":"eA=="}]}`
	w := jsonSubmit(h, key, body, "template-replay", nil)
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
	archived, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(archived), old.Version) || !strings.Contains(string(archived), "json-v3") {
		t.Fatal("version evidence", err)
	}
	newer := old
	newer.Text = "New receipt {{number}}"
	newer.Version = api.TemplateVersion(newer)
	h = templateHandler(t, key, r, []api.SendTemplate{old, newer})
	w = jsonSubmit(h, key, body, "template-replay", nil)
	requireStatus(t, w, 202)
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || !result.Replayed {
		t.Fatal(err)
	}
	requireStatus(t, jsonSubmit(h, key, strings.Replace(body, old.Version, newer.Version, 1), "template-replay", nil), 409)
	// Removing a version does not change an already queued archive.
	h = templateHandler(t, key, r, []api.SendTemplate{newer})
	requireStatus(t, jsonSubmit(h, key, body, "removed-version", nil), 400)
	runOne(t, f.worker)
	assertCount(t, f, 1, "SELECT count(*) FROM messages WHERE status='SMTP_ACCEPTED'")
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(archived) {
		t.Fatal("template publication rewrote queue", err)
	}
	other, otherKey, _ := sendFixture(t, f)
	requireStatus(t, jsonSubmit(other, otherKey, body, "other-site", nil), 400)
	// Sandbox uses the same builder but never produces a delivery attempt.
	_, sandboxKey, sandboxR := sandboxFixture(t, f)
	sandbox := templateHandler(t, sandboxKey, sandboxR, []api.SendTemplate{old})
	requireStatus(t, jsonSubmit(sandbox, sandboxKey, body, "template-sandbox", nil), 202)
	assertCount(t, f, 1, "SELECT count(*) FROM messages WHERE status='SANDBOX_STORED'")
	assertCount(t, f, 1, "SELECT count(*) FROM delivery_attempts")
	var audit string
	if err := f.db.QueryRow("SELECT data::text FROM maintenance_audit WHERE action='API_SEND_CONFIGURED' AND data->>'id'=$1 ORDER BY created_at DESC LIMIT 1", sandboxKey.ID).Scan(&audit); err != nil || !strings.Contains(audit, old.Version) || strings.Contains(audit, "Receipt") || strings.Contains(audit, "<42>") {
		t.Fatal("audit content disclosure", err)
	}
}
