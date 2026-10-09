package integration

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"relaytale/internal/api"
	"relaytale/internal/testsmtp"
)

func TestTemplateRetirementSnapshotAndCapacity(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, r := sendFixture(t, f)
	raw := catalogArtifact(t)
	var v api.SendTemplate
	json.Unmarshal(raw, &v)
	if _, err := api.PublishTemplate(context.Background(), f.db, key.ID, raw, "operator", "reviewed"); err != nil {
		t.Fatal(err)
	}
	key.UsePublishedTemplates = true
	config, _ := json.Marshal([]api.SendKey{key})
	old, err := api.NewSendHandler(context.Background(), string(config), r)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"date":"2026-10-08T12:00:00Z","template":{"id":"notice","version":"` + v.Version + `","variables":{"name":"customer"}}}`
	requireStatus(t, jsonSubmit(old, key, body, "before-retirement", nil), 202)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := api.RetireTemplate(context.Background(), f.db, key.ID, v.ID, v.Version, "operator", "obsolete"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	assertCount(t, f, 1, "SELECT count(*) FROM api_template_retirements")
	assertCount(t, f, 1, "SELECT count(*) FROM maintenance_audit WHERE action='TEMPLATE_RETIRED'")
	requireStatus(t, jsonSubmit(old, key, body, "old-snapshot", nil), 202)
	fresh, err := api.NewSendHandler(context.Background(), string(config), r)
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, jsonSubmit(fresh, key, body, "retired", nil), 400)
	// Retire only after the caller retry window: a removed renderer rejects even
	// an old request, without creating another message. Status queries remain usable.
	requireStatus(t, jsonSubmit(fresh, key, body, "before-retirement", nil), 400)
	list, err := api.ListPublishedTemplates(context.Background(), f.db, key.ID)
	if err != nil || len(list) != 1 || list[0].RetiredAt == nil {
		t.Fatal(list, err)
	}
	if replay, err := api.PublishTemplate(context.Background(), f.db, key.ID, raw, "operator", "same artifact"); err != nil || !replay {
		t.Fatal(replay, err)
	}
	// Retired history remains; eight new active versions fit, the ninth cannot.
	for i := 0; i < 9; i++ {
		v.Text = strings.Repeat("x", i+1) + " {{name}}"
		v.Version = api.TemplateVersion(v)
		b, _ := json.Marshal(v)
		_, err := api.PublishTemplate(context.Background(), f.db, key.ID, b, "operator", "replacement")
		if (i < 8) != (err == nil) {
			t.Fatalf("version %d: %v", i, err)
		}
	}
	if err := api.VerifyPublishedTemplates(context.Background(), f.db); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"UPDATE api_template_retirements SET retired_at=now()", "DELETE FROM api_template_retirements"} {
		if _, err := f.db.Exec(q); err == nil {
			t.Fatal("retirement history mutable")
		}
	}
	key.Templates = []api.SendTemplate{func() api.SendTemplate {
		var original api.SendTemplate
		json.Unmarshal(raw, &original)
		return original
	}()}
	config, _ = json.Marshal([]api.SendKey{key})
	if _, err := api.NewSendHandler(context.Background(), string(config), r); err == nil {
		t.Fatal("retired static version bypass")
	}
	runOne(t, f.worker)
	runOne(t, f.worker)
	assertCount(t, f, 2, "SELECT count(*) FROM messages WHERE status='SMTP_ACCEPTED'")
}

func TestTemplateRetirementAuditRollbackAndForgery(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, r := sendFixture(t, f)
	raw := catalogArtifact(t)
	var v api.SendTemplate
	json.Unmarshal(raw, &v)
	if _, err := api.PublishTemplate(context.Background(), f.db, key.ID, raw, "operator", "reviewed"); err != nil {
		t.Fatal(err)
	}
	mustExec(t, f, `CREATE FUNCTION reject_retirement_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='TEMPLATE_RETIRED' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END $$`)
	mustExec(t, f, `CREATE TRIGGER reject_retirement_audit BEFORE INSERT ON maintenance_audit FOR EACH ROW EXECUTE FUNCTION reject_retirement_audit()`)
	if _, err := api.RetireTemplate(context.Background(), f.db, key.ID, v.ID, v.Version, "operator", "retire"); err == nil {
		t.Fatal("audit failure accepted")
	}
	assertCount(t, f, 0, "SELECT count(*) FROM api_template_retirements")
	mustExec(t, f, "DROP TRIGGER reject_retirement_audit ON maintenance_audit")
	var audit string
	f.db.QueryRow("SELECT id FROM maintenance_audit WHERE action='TEMPLATE_PUBLISHED' LIMIT 1").Scan(&audit)
	mustExec(t, f, "INSERT INTO api_template_retirements(api_key_id,template_id,version,audit_id) VALUES($1,$2,$3,$4)", key.ID, v.ID, v.Version, audit)
	if _, err := api.RetireTemplate(context.Background(), f.db, key.ID, v.ID, v.Version, "operator", "retry"); err == nil {
		t.Fatal("forged retirement replay")
	}
	if err := api.VerifyPublishedTemplates(context.Background(), f.db); err == nil {
		t.Fatal("forged retirement recovery")
	}
	key.UsePublishedTemplates = true
	config, _ := json.Marshal([]api.SendKey{key})
	if _, err := api.NewSendHandler(context.Background(), string(config), r); err == nil {
		t.Fatal("forged retirement activation")
	}
}

func TestTemplateRetirementRetainedHistoryBound(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, _ := sendFixture(t, f)
	v := api.SendTemplate{ID: "notice", Text: "version"}
	for i := 0; i < 65; i++ {
		v.Text = strings.Repeat("x", i+1)
		v.Version = api.TemplateVersion(v)
		raw, _ := json.Marshal(v)
		_, err := api.PublishTemplate(context.Background(), f.db, key.ID, raw, "operator", "history test")
		if i == 64 {
			if err == nil {
				t.Fatal("unbounded retained history")
			}
			break
		}
		if err != nil {
			t.Fatal(i, err)
		}
		if _, err = api.RetireTemplate(context.Background(), f.db, key.ID, v.ID, v.Version, "operator", "history test"); err != nil {
			t.Fatal(i, err)
		}
	}
	list, err := api.ListPublishedTemplates(context.Background(), f.db, key.ID)
	if err != nil || len(list) != 64 {
		t.Fatal(len(list), err)
	}
	if err = api.VerifyPublishedTemplates(context.Background(), f.db); err != nil {
		t.Fatal(err)
	}
}
