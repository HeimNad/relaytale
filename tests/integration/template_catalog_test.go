package integration

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"relaytale/internal/api"
	"relaytale/internal/testsmtp"
)

func catalogArtifact(t *testing.T) []byte {
	t.Helper()
	v := api.SendTemplate{ID: "notice", Text: "Hello {{name}}", Variables: []string{"name"}}
	v.Version = api.TemplateVersion(v)
	raw, _ := json.Marshal(v)
	return raw
}
func TestPublishedTemplateCatalogConcurrencyActivationAndIsolation(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	old, key, r := sendFixture(t, f)
	raw := catalogArtifact(t)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := api.PublishTemplate(context.Background(), f.db, key.ID, raw, "operator", "reviewed template"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	assertCount(t, f, 1, "SELECT count(*) FROM api_template_versions")
	assertCount(t, f, 1, "SELECT count(*) FROM maintenance_audit WHERE action='TEMPLATE_PUBLISHED'")
	list, err := api.ListPublishedTemplates(context.Background(), f.db, key.ID)
	if err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	body := `{"date":"2026-10-08T12:00:00Z","template":{"id":"notice","version":"` + list[0].Version + `","variables":{"name":"customer"}}}`
	requireStatus(t, jsonSubmit(old, key, body, "not-active", nil), 400)
	key.UsePublishedTemplates = true
	config, _ := json.Marshal([]api.SendKey{key})
	h, err := api.NewSendHandler(context.Background(), string(config), r)
	if err != nil {
		t.Fatal(err)
	}
	w := jsonSubmit(h, key, body, "published", nil)
	requireStatus(t, w, 202)
	var result struct {
		MessageID string `json:"message_id"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var path string
	if err = f.db.QueryRow("SELECT eml_path FROM messages WHERE id=$1", result.MessageID).Scan(&path); err != nil {
		t.Fatal(err)
	}
	archived, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	other, otherKey, otherR := sendFixture(t, f)
	otherKey.UsePublishedTemplates = true
	config, _ = json.Marshal([]api.SendKey{otherKey})
	other, err = api.NewSendHandler(context.Background(), string(config), otherR)
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, jsonSubmit(other, otherKey, body, "other", nil), 400)
	if err = api.VerifyPublishedTemplates(context.Background(), f.db); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{"UPDATE api_template_versions SET language='en'", "DELETE FROM api_template_versions"} {
		if _, err = f.db.Exec(sql); err == nil {
			t.Fatal("published content mutable")
		}
	}
	runOne(t, f.worker)
	assertCount(t, f, 1, "SELECT count(*) FROM messages WHERE status='SMTP_ACCEPTED'")
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(archived) {
		t.Fatal("archive changed", err)
	}
}
func TestPublishedTemplateAtomicAuditAndBounds(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, _ := sendFixture(t, f)
	raw := catalogArtifact(t)
	mustExec(t, f, `CREATE FUNCTION reject_template_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='TEMPLATE_PUBLISHED' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END $$`)
	mustExec(t, f, `CREATE TRIGGER reject_template_audit BEFORE INSERT ON maintenance_audit FOR EACH ROW EXECUTE FUNCTION reject_template_audit()`)
	if _, err := api.PublishTemplate(context.Background(), f.db, key.ID, raw, "operator", "test"); err == nil {
		t.Fatal("audit failure accepted")
	}
	assertCount(t, f, 0, "SELECT count(*) FROM api_template_versions")
	mustExec(t, f, "DROP TRIGGER reject_template_audit ON maintenance_audit")
	for i := 0; i < 8; i++ {
		var v api.SendTemplate
		json.Unmarshal(raw, &v)
		v.Text = strings.Repeat("x", i+1) + " {{name}}"
		v.Version = api.TemplateVersion(v)
		b, _ := json.Marshal(v)
		if _, err := api.PublishTemplate(context.Background(), f.db, key.ID, b, "operator", "reviewed"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := api.PublishTemplate(context.Background(), f.db, key.ID, raw, "operator", "ninth"); err == nil {
		t.Fatal("catalog count exceeded")
	}
	assertCount(t, f, 8, "SELECT count(*) FROM api_template_versions")
	assertCount(t, f, 8, "SELECT count(*) FROM maintenance_audit WHERE action='TEMPLATE_PUBLISHED'")
	if _, err := api.PublishTemplate(context.Background(), f.db, key.ID, raw, "", "reason"); err == nil {
		t.Fatal("missing actor")
	}
	mustExec(t, f, "UPDATE api_keys SET enabled=false WHERE id=$1", key.ID)
	if _, err := api.PublishTemplate(context.Background(), f.db, key.ID, raw, "operator", "disabled"); err == nil {
		t.Fatal("disabled identity")
	}
}

func TestPublishedTemplateForgedAuditBlocksActivation(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, r := sendFixture(t, f)
	raw := catalogArtifact(t)
	var v api.SendTemplate
	json.Unmarshal(raw, &v)
	// SQL metadata constraints do not grant a startup configuration audit permission
	// to publish a template. Both activation and offline verification must reject it.
	var auditID string
	if err := f.db.QueryRow("SELECT id FROM maintenance_audit WHERE action='API_SEND_CONFIGURED' LIMIT 1").Scan(&auditID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, f, "INSERT INTO api_template_versions(api_key_id,template_id,version,language,definition,definition_sha256,audit_id) VALUES($1,$2,$3,'',$4,encode(sha256(convert_to($4,'UTF8')),'hex'),$5)", key.ID, v.ID, v.Version, string(raw), auditID)
	if _, err := api.PublishTemplate(context.Background(), f.db, key.ID, raw, "operator", "retry"); err == nil {
		t.Fatal("forged record reported as publication replay")
	}
	if err := api.VerifyPublishedTemplates(context.Background(), f.db); err == nil {
		t.Fatal("forged publication audit accepted")
	}
	key.UsePublishedTemplates = true
	config, _ := json.Marshal([]api.SendKey{key})
	if _, err := api.NewSendHandler(context.Background(), string(config), r); err == nil {
		t.Fatal("forged publication activated")
	}
}
