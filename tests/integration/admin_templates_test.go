package integration

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"relaytale/internal/api"
	"relaytale/internal/testsmtp"
)

func TestTemplateManagementRolesPublicationAndIsolation(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	_, key, _ := sendFixture(t, f)
	h := management(t, f)
	base := "/templates/" + key.ID
	raw := catalogArtifact(t)
	var v api.SendTemplate
	json.Unmarshal(raw, &v)
	payload := map[string]any{"expected_revision": 0, "template": v, "reason": "create draft"}
	requireStatus(t, adminRequest(h, "POST", base+"/drafts", "v", payload), 403)
	requireStatus(t, adminRequest(h, "POST", base+"/drafts", "o", payload), 200)
	list := adminRequest(h, "GET", base+"/drafts", "v", nil)
	requireStatus(t, list, 200)
	if strings.Contains(list.Body.String(), "Hello") || strings.Contains(list.Body.String(), "definition") {
		t.Fatal("source leaked in metadata")
	}
	sourcePath := base + "/drafts/notice/revisions/1"
	requireStatus(t, adminRequest(h, "GET", sourcePath, "v", nil), 403)
	requireStatus(t, adminRequest(h, "GET", sourcePath, "o", nil), 200)
	publish := map[string]any{"revision": 1, "version": v.Version, "reason": "reviewed"}
	requireStatus(t, adminRequest(h, "POST", base+"/drafts/notice/publish", "o", publish), 403)
	requireStatus(t, adminRequest(h, "POST", base+"/drafts/notice/publish", "a", publish), 200)
	var actor, revision string
	if err := f.db.QueryRow("SELECT actor,data->>'draft_revision' FROM maintenance_audit WHERE action='TEMPLATE_PUBLISHED'").Scan(&actor, &revision); err != nil || actor != "test-admin" || revision != "1" {
		t.Fatal(actor, revision, err)
	}
	v.Text = "changed {{name}}"
	v.Version = ""
	payload["template"] = v
	payload["expected_revision"] = 1
	requireStatus(t, adminRequest(h, "POST", base+"/drafts", "o", payload), 200)
	requireStatus(t, adminRequest(h, "POST", base+"/drafts", "o", payload), 200)
	v.Text = "different {{name}}"
	payload["template"] = v
	requireStatus(t, adminRequest(h, "POST", base+"/drafts", "o", payload), 409)
	requireStatus(t, adminRequest(h, "POST", base+"/drafts/notice/publish", "a", publish), 409)
	_, other, _ := sendFixture(t, f)
	requireStatus(t, adminRequest(h, "GET", "/templates/"+other.ID+"/drafts/notice/revisions/1", "o", nil), 404)
	requireStatus(t, adminRequest(h, "GET", "/templates/"+uuid.NewString()+"/versions", "v", nil), 404)
	identities := adminRequest(h, "GET", "/template-identities", "v", nil)
	requireStatus(t, identities, 200)
	for _, secret := range []string{"key_hash", "token", "password"} {
		if strings.Contains(identities.Body.String(), secret) {
			t.Fatal("credential metadata leaked")
		}
	}
	versions := adminRequest(h, "GET", base+"/versions", "v", nil)
	requireStatus(t, versions, 200)
	var items struct {
		Items []api.PublishedTemplate `json:"items"`
	}
	json.Unmarshal(versions.Body.Bytes(), &items)
	retire := base + "/versions/notice/" + items.Items[0].Version + "/retire"
	requireStatus(t, adminRequest(h, "POST", retire, "a", map[string]any{"reason": "end"}), 400)
	requireStatus(t, adminRequest(h, "POST", retire, "a", map[string]any{"reason": "end", "acknowledge_retry_window": true}), 200)
}
