package integration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"relaytale/internal/api"
	"relaytale/internal/testsmtp"
)

func TestTemplateDraftOptimisticEditAndPinnedPublish(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, r := sendFixture(t, f)
	ctx := context.Background()
	raw := catalogArtifact(t)
	rev, replay, err := api.SaveTemplateDraft(ctx, f.db, key.ID, 0, raw, "operator", "draft create")
	if err != nil || rev != 1 || replay {
		t.Fatal(rev, replay, err)
	}
	rev, replay, err = api.SaveTemplateDraft(ctx, f.db, key.ID, 0, raw, "operator", "retry")
	if err != nil || rev != 1 || !replay {
		t.Fatal(rev, replay, err)
	}
	assertCount(t, f, 0, "SELECT count(*) FROM api_template_versions")
	old, err := api.GetTemplateDraft(ctx, f.db, key.ID, "notice", 1)
	if err != nil {
		t.Fatal(err)
	}
	key.UsePublishedTemplates = true
	config, _ := json.Marshal([]api.SendKey{key})
	h, err := api.NewSendHandler(ctx, string(config), r)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"date":"2026-10-08T12:00:00Z","template":{"id":"notice","version":"` + old.Version + `","variables":{"name":"customer"}}}`
	requireStatus(t, jsonSubmit(h, key, body, "draft-only", nil), 400)
	var wg sync.WaitGroup
	success, conflict := 0, 0
	var mu sync.Mutex
	for _, text := range []string{"first {{name}}", "second {{name}}"} {
		wg.Add(1)
		go func(text string) {
			defer wg.Done()
			v := old
			v.Text = text
			v.Version = ""
			b, _ := json.Marshal(v)
			_, _, e := api.SaveTemplateDraft(ctx, f.db, key.ID, 1, b, "operator", "edit")
			mu.Lock()
			defer mu.Unlock()
			if e == nil {
				success++
			} else if errors.Is(e, api.ErrTemplateDraftConflict) {
				conflict++
			} else {
				t.Error(e)
			}
		}(text)
	}
	wg.Wait()
	if success != 1 || conflict != 1 {
		t.Fatal(success, conflict)
	}
	if _, err = api.PublishTemplateDraft(ctx, f.db, key.ID, "notice", 1, old.Version, "operator", "stale review"); !errors.Is(err, api.ErrTemplateDraftConflict) {
		t.Fatal(err)
	}
	current, err := api.GetTemplateDraft(ctx, f.db, key.ID, "notice", 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = api.PublishTemplateDraft(ctx, f.db, key.ID, "notice", 2, old.Version, "operator", "wrong digest"); err == nil {
		t.Fatal("unreviewed content published")
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := api.PublishTemplateDraft(ctx, f.db, key.ID, "notice", 2, current.Version, "operator", "reviewed"); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	assertCount(t, f, 1, "SELECT count(*) FROM api_template_versions")
	assertCount(t, f, 1, "SELECT count(*) FROM maintenance_audit WHERE action='TEMPLATE_PUBLISHED'")
	assertCount(t, f, 1, "SELECT count(*) FROM maintenance_audit WHERE action='TEMPLATE_PUBLISHED' AND data->>'draft_revision'='2'")
	h, err = api.NewSendHandler(ctx, string(config), r)
	if err != nil {
		t.Fatal(err)
	}
	body = `{"date":"2026-10-08T12:00:00Z","template":{"id":"notice","version":"` + current.Version + `","variables":{"name":"customer"}}}`
	requireStatus(t, jsonSubmit(h, key, body, "published-draft", nil), 202)
	for _, q := range []string{"UPDATE api_template_drafts SET revision=3", "DELETE FROM api_template_drafts"} {
		if _, err = f.db.Exec(q); err == nil {
			t.Fatal("draft history mutable")
		}
	}
	_, other, _ := sendFixture(t, f)
	if _, err = api.GetTemplateDraft(ctx, f.db, other.ID, "notice", 2); err == nil {
		t.Fatal("cross identity source export")
	}
	if err = api.VerifyPublishedTemplates(ctx, f.db); err != nil {
		t.Fatal(err)
	}
}

func TestTemplateDraftAuditFailureAndBounds(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, _ := sendFixture(t, f)
	ctx := context.Background()
	raw := catalogArtifact(t)
	mustExec(t, f, `CREATE FUNCTION reject_draft_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='TEMPLATE_DRAFT_SAVED' OR NEW.action='TEMPLATE_PUBLISHED' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END $$`)
	mustExec(t, f, `CREATE TRIGGER reject_draft_audit BEFORE INSERT ON maintenance_audit FOR EACH ROW EXECUTE FUNCTION reject_draft_audit()`)
	if _, _, err := api.SaveTemplateDraft(ctx, f.db, key.ID, 0, raw, "operator", "create"); err == nil {
		t.Fatal("draft audit failure accepted")
	}
	assertCount(t, f, 0, "SELECT count(*) FROM api_template_drafts")
	mustExec(t, f, "DROP TRIGGER reject_draft_audit ON maintenance_audit")
	rev, _, err := api.SaveTemplateDraft(ctx, f.db, key.ID, 0, raw, "operator", "create")
	if err != nil {
		t.Fatal(err)
	}
	v, err := api.GetTemplateDraft(ctx, f.db, key.ID, "notice", rev)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, f, `CREATE TRIGGER reject_draft_audit BEFORE INSERT ON maintenance_audit FOR EACH ROW EXECUTE FUNCTION reject_draft_audit()`)
	if _, err = api.PublishTemplateDraft(ctx, f.db, key.ID, v.ID, rev, v.Version, "operator", "publish"); err == nil {
		t.Fatal("publish audit failure accepted")
	}
	assertCount(t, f, 0, "SELECT count(*) FROM api_template_versions")
	mustExec(t, f, "DROP TRIGGER reject_draft_audit ON maintenance_audit")
	for i := 1; i < 65; i++ {
		v.Text = "revision " + string(rune('a'+i)) + " {{name}}"
		v.Version = ""
		b, _ := json.Marshal(v)
		next, _, e := api.SaveTemplateDraft(ctx, f.db, key.ID, rev, b, "operator", "edit")
		if i == 64 {
			if e == nil {
				t.Fatal("unbounded draft history")
			}
			break
		}
		if e != nil {
			t.Fatal(i, e)
		}
		rev = next
	}
	if err = api.VerifyTemplateDrafts(ctx, f.db); err != nil {
		t.Fatal(err)
	}
	list, err := api.ListTemplateDrafts(ctx, f.db, key.ID)
	if err != nil || len(list) != 1 || list[0].Revision != 64 {
		t.Fatal(list, err)
	}
}

func TestTemplateDraftHeadCapacityAndForgedAudit(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, _ := sendFixture(t, f)
	ctx := context.Background()
	save := func(name string, rev int64, text, html string) error {
		v := api.SendTemplate{ID: name, Text: text, HTML: html}
		b, _ := json.Marshal(v)
		_, _, e := api.SaveTemplateDraft(ctx, f.db, key.ID, rev, b, "operator", "capacity")
		return e
	}
	if err := save("a", 0, strings.Repeat("x", 16000), strings.Repeat("y", 14000)); err != nil {
		t.Fatal(err)
	}
	if err := save("a", 1, "small", ""); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"b", "c"} {
		if err := save(name, 0, strings.Repeat("x", 16000), strings.Repeat("y", 14000)); err != nil {
			t.Fatal(err)
		}
	}
	if err := api.VerifyTemplateDrafts(ctx, f.db); err != nil {
		t.Fatal("historical larger revision counted as current", err)
	}
	if err := save("d", 0, strings.Repeat("x", 16000), strings.Repeat("y", 14000)); err == nil {
		t.Fatal("head bytes exceeded")
	}
	for _, name := range []string{"d", "e", "f", "g", "h"} {
		if err := save(name, 0, "small", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := save("i", 0, "small", ""); err == nil {
		t.Fatal("draft IDs exceeded")
	}
	// A second identity has a well-formed source but a startup audit, not a save audit.
	_, other, otherR := sendFixture(t, f)
	v := api.SendTemplate{ID: "forged", Text: "hello"}
	v.Version = api.TemplateVersion(v)
	raw, _ := json.Marshal(v)
	var audit string
	if err := f.db.QueryRow("SELECT id FROM maintenance_audit WHERE action='API_SEND_CONFIGURED' LIMIT 1").Scan(&audit); err != nil {
		t.Fatal(err)
	}
	mustExec(t, f, "INSERT INTO api_template_drafts(api_key_id,template_id,revision,version,definition,definition_sha256,audit_id) VALUES($1,$2,1,$3,$4,encode(sha256(convert_to($4,'UTF8')),'hex'),$5)", other.ID, v.ID, v.Version, string(raw), audit)
	if _, err := api.GetTemplateDraft(ctx, f.db, other.ID, v.ID, 1); err == nil {
		t.Fatal("forged source exported")
	}
	if _, err := api.PublishTemplateDraft(ctx, f.db, other.ID, v.ID, 1, v.Version, "operator", "publish"); err == nil {
		t.Fatal("forged source published")
	}
	if err := api.VerifyTemplateDrafts(ctx, f.db); err == nil {
		t.Fatal("forged draft restored")
	}

	var publicationAudit string
	if err := f.db.QueryRow(`INSERT INTO maintenance_audit(id,action,actor,data) SELECT gen_random_uuid(),'TEMPLATE_PUBLISHED','operator',jsonb_build_object('api_key_id',api_key_id::text,'id',template_id,'version',version,'definition_sha256',definition_sha256,'draft_revision',1) FROM api_template_drafts WHERE api_key_id=$1 RETURNING id`, other.ID).Scan(&publicationAudit); err != nil {
		t.Fatal(err)
	}
	mustExec(t, f, "INSERT INTO api_template_versions(api_key_id,template_id,version,definition,definition_sha256,audit_id) SELECT api_key_id,template_id,version,definition,definition_sha256,$2 FROM api_template_drafts WHERE api_key_id=$1", other.ID, publicationAudit)
	other.UsePublishedTemplates = true
	config, _ := json.Marshal([]api.SendKey{other})
	if _, err := api.NewSendHandler(ctx, string(config), otherR); err == nil {
		t.Fatal("publication linked to forged draft activated")
	}
}
