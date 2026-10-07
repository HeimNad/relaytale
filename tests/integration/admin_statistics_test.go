package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"relaytale/internal/api"
	"relaytale/internal/testsmtp"
)

func capacityRequest(h http.Handler, query, token string, edit func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/admin/v1/statistics?"+query, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	if edit != nil {
		edit(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestManagementStatisticsScopeAndBounds(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	h := management(t, f)
	web, k, _ := sendFixture(t, f)
	id := submitStatusFixture(t, web, k, f.raw(), nil)
	other, key, _ := sendFixture(t, f)
	submitStatusFixture(t, other, key, f.raw(), nil)
	sandbox, sk, _ := sandboxFixture(t, f)
	submitStatusFixture(t, sandbox, sk, f.raw(), nil)
	f.seed(t)
	from := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	until := time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
	query := "from=" + from.Format(time.RFC3339Nano) + "&until=" + until.Format(time.RFC3339Nano)
	requireStatus(t, capacityRequest(h, query, strings.Repeat("v", 32), nil), 400)
	until = time.Now().UTC().Truncate(time.Microsecond)
	query = "from=" + from.Format(time.RFC3339Nano) + "&until=" + until.Format(time.RFC3339Nano)
	var before int
	f.db.QueryRow("SELECT count(*) FROM maintenance_audit").Scan(&before)
	for _, role := range []string{"v", "o", "a"} {
		w := capacityRequest(h, query, strings.Repeat(role, 32), nil)
		requireStatus(t, w, 200)
		var out api.CapacityStatistics
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.MessageCount != 4 || len(out.Groups) != 4 {
			t.Fatal(out)
		}
		found := map[string]bool{}
		for _, g := range out.Groups {
			found[g.Source+"/"+g.Environment+"/"+g.APIKeyID] = true
			if g.MessageCount != 1 || g.LogicalRawBytesByArchiveState["AVAILABLE"] == 0 {
				t.Fatal(g)
			}
		}
		if !found["api/production/"+k.ID] || !found["api/production/"+key.ID] || !found["api/sandbox/"+sk.ID] || !found["smtp/production/"] {
			t.Fatal(found)
		}
		for _, private := range []string{id, k.Token, "one@example.test", f.root, "sender@"} {
			if strings.Contains(w.Body.String(), private) {
				t.Fatal("private field leaked")
			}
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("missing privacy header")
		}
	}
	assertCount(t, f, before, "SELECT count(*) FROM maintenance_audit")
	assertCount(t, f, 0, "SELECT count(*) FROM delivery_attempts")
	requireStatus(t, capacityRequest(h, query, "", nil), 401)
	requireStatus(t, capacityRequest(h, query, k.Token, nil), 401)
	requireStatus(t, capacityRequest(h, query, strings.Repeat("v", 32), func(r *http.Request) { r.Header.Set("Origin", "https://other.test") }), 403)
	requireStatus(t, capacityRequest(h, query, strings.Repeat("v", 32), func(r *http.Request) { r.Method = "HEAD" }), 405)
	for _, bad := range []string{query + "&limit=1", query + "&from=x", "from=x&until=y"} {
		requireStatus(t, capacityRequest(h, bad, strings.Repeat("v", 32), nil), 400)
	}
	// Exact boundary fits; one additional row must reject the whole result.
	mustExec(t, f, `INSERT INTO messages(id,source_type,status,envelope_from,subject,eml_path,eml_size,eml_sha256,created_at)
 SELECT gen_random_uuid(),'smtp','QUEUED','sender@example.test','',gen_random_uuid()::text,1,repeat('a',64),$1 FROM generate_series(1,9996)`, from)
	w := capacityRequest(h, query, strings.Repeat("v", 32), nil)
	requireStatus(t, w, 200)
	var out api.CapacityStatistics
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.MessageCount != 10000 {
		t.Fatal(out.MessageCount)
	}
	mustExec(t, f, `INSERT INTO messages(id,source_type,status,envelope_from,subject,eml_path,eml_size,eml_sha256,created_at) VALUES(gen_random_uuid(),'smtp','QUEUED','sender@example.test','',gen_random_uuid()::text,1,repeat('a',64),$1)`, from)
	w = capacityRequest(h, query, strings.Repeat("v", 32), nil)
	requireStatus(t, w, 422)
	if strings.Contains(w.Body.String(), "groups") {
		t.Fatal("partial report returned")
	}
}

func TestManagementStatisticsGroupBudgetAndEmptyWindow(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	h := management(t, f)
	until := time.Now().UTC().Truncate(time.Microsecond)
	from := until.Add(-time.Hour)
	query := "from=" + from.Format(time.RFC3339Nano) + "&until=" + until.Format(time.RFC3339Nano)
	w := capacityRequest(h, query, strings.Repeat("v", 32), nil)
	requireStatus(t, w, 200)
	var out api.CapacityStatistics
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.MessageCount != 0 || out.Groups == nil || len(out.Groups) != 0 {
		t.Fatal(out)
	}
	mustExec(t, f, `WITH identities AS (
 INSERT INTO api_keys(id,name,key_hash,prefix,enabled)
 SELECT gen_random_uuid(),'capacity-only',gen_random_uuid()::text,'rts_',false FROM generate_series(1,256) RETURNING id)
 INSERT INTO messages(id,source_type,api_key_id,status,envelope_from,subject,eml_path,eml_size,eml_sha256,created_at)
 SELECT gen_random_uuid(),'api',id,'QUEUED','sender@example.test','',gen_random_uuid()::text,1,repeat('a',64),$1 FROM identities`, from)
	w = capacityRequest(h, query, strings.Repeat("v", 32), nil)
	requireStatus(t, w, 200)
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.MessageCount != 256 || len(out.Groups) != 256 {
		t.Fatal(out)
	}
	mustExec(t, f, `INSERT INTO messages(id,source_type,status,envelope_from,subject,eml_path,eml_size,eml_sha256,created_at) VALUES(gen_random_uuid(),'internal','QUEUED','sender@example.test','',gen_random_uuid()::text,1,repeat('a',64),$1)`, from)
	w = capacityRequest(h, query, strings.Repeat("v", 32), nil)
	requireStatus(t, w, 422)
	if strings.Contains(w.Body.String(), "groups") {
		t.Fatal("partial groups returned")
	}
	// Remove the extra group from the cohort, then reject overflow within a group.
	mustExec(t, f, `UPDATE messages SET created_at=$1 WHERE source_type='internal'`, until)
	mustExec(t, f, `UPDATE messages SET eml_size=9223372036854775807 WHERE id=(SELECT id FROM messages WHERE source_type='api' LIMIT 1)`)
	mustExec(t, f, `INSERT INTO messages(id,source_type,api_key_id,status,envelope_from,subject,eml_path,eml_size,eml_sha256,created_at)
 SELECT gen_random_uuid(),'api',api_key_id,'QUEUED','sender@example.test','',gen_random_uuid()::text,1,repeat('a',64),$1 FROM messages WHERE eml_size=9223372036854775807 LIMIT 1`, from)
	requireStatus(t, capacityRequest(h, query, strings.Repeat("v", 32), nil), 503)
}
