package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"relaytale/internal/api"
	"relaytale/internal/provider"
	"relaytale/internal/testsmtp"
)

func management(t *testing.T, f *deliveryFixture) http.Handler {
	t.Helper()
	raw, _ := json.Marshal([]api.Principal{{ID: "test-admin", Role: "admin", Token: strings.Repeat("a", 32)}, {ID: "test-reader", Role: "viewer", Token: strings.Repeat("v", 32)}, {ID: "test-operator", Role: "operator", Token: strings.Repeat("o", 32)}})
	h, err := (api.Management{DB: f.db, Box: f.box}).Handler(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func adminRequest(h http.Handler, method, path, role string, v any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(v)
	r := httptest.NewRequest(method, "/admin/v1"+path, bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer "+strings.Repeat(role, 32))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func requireStatus(t *testing.T, w *httptest.ResponseRecorder, n int) {
	t.Helper()
	if w.Code != n {
		t.Fatalf("want %d got %d: %s", n, w.Code, w.Body)
	}
}
func TestManagementProviderConcurrencyRotationAndAuditRollback(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h := management(t, f)
	id := uuid.NewString()
	settings := provider.Settings{Name: id, Host: "smtp.example.test", Port: 587, Security: "starttls", Username: "configuration-user", MaxConnections: 1, TimeoutSeconds: 30, FromDomains: []string{f.domain}}
	create := map[string]any{"id": id, "settings": settings, "password": "private-provider-credential", "reason": "initial setup"}
	requireStatus(t, adminRequest(h, "POST", "/providers", "o", create), 403)
	requireStatus(t, adminRequest(h, "POST", "/providers", "a", create), 201)
	requireStatus(t, adminRequest(h, "POST", "/providers", "a", create), 409)
	get := adminRequest(h, "GET", "/providers/"+id, "v", nil)
	requireStatus(t, get, 200)
	if strings.Contains(get.Body.String(), "password") || strings.Contains(get.Body.String(), "nonce") || strings.Contains(get.Body.String(), "private-provider-credential") {
		t.Fatal("credential leaked")
	}
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- adminRequest(h, "PUT", "/providers/"+id, "a", map[string]any{"expected_revision": 1, "settings": settings, "reason": "concurrent edit"}).Code
		}()
	}
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for c := range codes {
		counts[c]++
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Fatal(counts)
	}
	requireStatus(t, adminRequest(h, "POST", "/providers/"+id+"/credentials", "a", map[string]any{"expected_revision": 2, "password": "rotated-private-password", "reason": "rotation"}), 200)
	var cipher, nonce []byte
	var rev int64
	if err := f.db.QueryRow(`SELECT password_ciphertext,nonce,revision FROM providers WHERE id=$1`, id).Scan(&cipher, &nonce, &rev); err != nil {
		t.Fatal(err)
	}
	pass, err := f.box.Open(id, cipher, nonce)
	if err != nil || pass != "rotated-private-password" || rev != 3 {
		t.Fatal("rotation failed", err)
	}
	var audit string
	var count int
	if err = f.db.QueryRow(`SELECT count(*),coalesce(string_agg(data::text,''),'') FROM maintenance_audit WHERE data->>'provider_id'=$1`, id).Scan(&count, &audit); err != nil {
		t.Fatal(err)
	}
	if count != 3 || strings.Contains(audit, "private-password") || strings.Contains(audit, "private-provider-credential") || strings.Contains(audit, "ciphertext") {
		t.Fatal("incorrect audit or secret disclosure")
	}
	mustExec(t, f, `CREATE FUNCTION fail_admin_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action LIKE 'PROVIDER_%' THEN RAISE EXCEPTION 'injected secret failure'; END IF; RETURN NEW; END; $$; CREATE TRIGGER fail_admin_audit BEFORE INSERT ON maintenance_audit FOR EACH ROW EXECUTE FUNCTION fail_admin_audit();`)
	defer f.db.Exec(`DROP TRIGGER fail_admin_audit ON maintenance_audit; DROP FUNCTION fail_admin_audit();`)
	failed := adminRequest(h, "POST", "/providers/"+id+"/credentials", "a", map[string]any{"expected_revision": 3, "password": "must-not-commit", "reason": "rollback"})
	requireStatus(t, failed, 503)
	if strings.Contains(failed.Body.String(), "injected") {
		t.Fatal("database error leaked")
	}
	if err = f.db.QueryRow(`SELECT password_ciphertext,nonce,revision FROM providers WHERE id=$1`, id).Scan(&cipher, &nonce, &rev); err != nil {
		t.Fatal(err)
	}
	pass, err = f.box.Open(id, cipher, nonce)
	if err != nil || rev != 3 || pass != "rotated-private-password" {
		t.Fatal("audit failure did not roll back")
	}
}
func TestManagementSuppressionAndUnknownReuseService(t *testing.T) {
	f := delivery(t, testsmtp.Options{DropFinal: true}, 1)
	h := management(t, f)
	mid := f.seed(t)
	runOne(t, f.worker)
	var rid, attempt, address string
	if err := f.db.QueryRow(`SELECT r.id,a.id,r.address FROM recipients r JOIN attempt_recipients ar ON ar.recipient_id=r.id JOIN delivery_attempts a ON a.id=ar.attempt_id WHERE r.message_id=$1 ORDER BY r.id LIMIT 1`, mid).Scan(&rid, &attempt, &address); err != nil {
		t.Fatal(err)
	}
	request := map[string]any{"expected_attempt": attempt, "action": "retry", "reason": "reviewed ambiguity"}
	requireStatus(t, adminRequest(h, "POST", "/recipients/"+rid+"/resolve-unknown", "o", request), 400)
	add := map[string]any{"email": address, "category": "manual", "reason": "block test"}
	w := adminRequest(h, "POST", "/suppressions", "o", add)
	requireStatus(t, w, 201)
	var result map[string]string
	json.Unmarshal(w.Body.Bytes(), &result)
	sid := result["id"]
	requireStatus(t, adminRequest(h, "POST", "/suppressions", "o", add), 409)
	request["acknowledge_duplicate_risk"] = true
	requireStatus(t, adminRequest(h, "POST", "/recipients/"+rid+"/resolve-unknown", "o", request), 409)
	release := map[string]string{"reason": "reviewed removal"}
	requireStatus(t, adminRequest(h, "POST", "/suppressions/"+sid+"/release", "o", release), 200)
	requireStatus(t, adminRequest(h, "POST", "/suppressions/"+sid+"/release", "o", release), 409)
	request["action"] = "mark-failed"
	requireStatus(t, adminRequest(h, "POST", "/recipients/"+rid+"/resolve-unknown", "o", request), 200)
	requireStatus(t, adminRequest(h, "POST", "/recipients/"+rid+"/resolve-unknown", "o", request), 409)
	var who string
	if err := f.db.QueryRow(`SELECT actor FROM maintenance_audit WHERE action='UNKNOWN_RESOLVED' AND data->>'recipient_id'=$1`, rid).Scan(&who); err != nil || who != "test-operator" {
		t.Fatal("actor not bound to key", err)
	}
	// Existing raw content is deliberately outside the management response schema.
	mustExec(t, f, `UPDATE delivery_attempts SET raw_debug_log='private-raw-debug' WHERE id=$1`, attempt)
	for _, path := range []string{"/messages/" + mid, "/messages/" + mid + "/recipients", "/messages/" + mid + "/attempts", "/messages/" + mid + "/events", "/providers", "/suppressions"} {
		w := adminRequest(h, "GET", path, "v", nil)
		requireStatus(t, w, 200)
		for _, secret := range []string{"private-raw-debug", "eml_path", "password_ciphertext", "nonce", "claim_token"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("%s leaked %s", path, secret)
			}
		}
	}
}

func TestManagementPaginationAndTimelineOrdering(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h := management(t, f)
	mid := f.seed(t)
	// UUID order deliberately disagrees with event time to catch a fake timeline.
	late := uuid.NewString()
	early := uuid.NewString()
	mustExec(t, f, `INSERT INTO events(id,message_id,event_type,event_time,source,data) VALUES($1,$3,'EARLY','2000-01-01','test','{"password":"never-return-event-payload"}'),($2,$3,'LATE','2001-01-01','test','{}')`, early, late, mid)
	w := adminRequest(h, "GET", "/messages/"+mid+"/events?limit=1", "v", nil)
	requireStatus(t, w, 200)
	var page struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
		After string `json:"next_after"`
		More  bool   `json:"has_more"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Items) != 1 || page.Items[0].ID != early || !page.More {
		t.Fatal("bad first timeline page", err, w.Body)
	}
	w = adminRequest(h, "GET", "/messages/"+mid+"/events?limit=1&after="+page.After, "v", nil)
	requireStatus(t, w, 200)
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Items) != 1 || page.Items[0].ID != late {
		t.Fatal("bad timeline continuation", err, w.Body)
	}
	// Receiver UUIDs and all collections expose a bounded cursor response.
	for _, p := range []string{"/messages", "/providers", "/suppressions", "/messages/" + mid + "/recipients", "/messages/" + mid + "/attempts"} {
		w = adminRequest(h, "GET", p+"?limit=1", "v", nil)
		requireStatus(t, w, 200)
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Items) > 1 {
			t.Fatal("unbounded collection", p, err)
		}
	}
}

func TestManagementPolicyAndUnknownAuditFailuresAreAtomic(t *testing.T) {
	f := delivery(t, testsmtp.Options{DropFinal: true}, 1)
	h := management(t, f)
	mid := f.seed(t)
	runOne(t, f.worker)
	var rid, attempt string
	if err := f.db.QueryRow(`SELECT ar.recipient_id,ar.attempt_id FROM attempt_recipients ar JOIN delivery_attempts a ON a.id=ar.attempt_id WHERE a.message_id=$1 LIMIT 1`, mid).Scan(&rid, &attempt); err != nil {
		t.Fatal(err)
	}
	email := uuid.NewString() + "@example.test"
	w := adminRequest(h, "POST", "/suppressions", "o", map[string]string{"email": email, "category": "manual", "reason": "initial"})
	requireStatus(t, w, 201)
	var added map[string]string
	json.Unmarshal(w.Body.Bytes(), &added)
	mustExec(t, f, `CREATE FUNCTION fail_admin_policy_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action IN ('UNKNOWN_RESOLVED','SUPPRESSION_RELEASED','SUPPRESSION_ADDED') THEN RAISE EXCEPTION 'injected'; END IF; RETURN NEW; END; $$; CREATE TRIGGER fail_admin_policy_audit BEFORE INSERT ON maintenance_audit FOR EACH ROW EXECUTE FUNCTION fail_admin_policy_audit();`)
	defer f.db.Exec(`DROP TRIGGER fail_admin_policy_audit ON maintenance_audit; DROP FUNCTION fail_admin_policy_audit();`)
	requireStatus(t, adminRequest(h, "POST", "/suppressions/"+added["id"]+"/release", "o", map[string]string{"reason": "should roll back"}), 503)
	second := uuid.NewString() + "@example.test"
	requireStatus(t, adminRequest(h, "POST", "/suppressions", "o", map[string]string{"email": second, "category": "manual", "reason": "should roll back"}), 503)
	requireStatus(t, adminRequest(h, "POST", "/recipients/"+rid+"/resolve-unknown", "o", map[string]string{"expected_attempt": attempt, "action": "mark-failed", "reason": "should roll back"}), 503)
	var state string
	var released bool
	var count int
	if err := f.db.QueryRow(`SELECT status FROM recipients WHERE id=$1`, rid).Scan(&state); err != nil || state != "DELIVERY_UNKNOWN" {
		t.Fatal("UNKNOWN changed", err, state)
	}
	if err := f.db.QueryRow(`SELECT released_at IS NOT NULL FROM suppression_entries WHERE id=$1`, added["id"]).Scan(&released); err != nil || released {
		t.Fatal("suppression released", err)
	}
	if err := f.db.QueryRow(`SELECT count(*) FROM suppression_entries WHERE email=$1`, second).Scan(&count); err != nil || count != 0 {
		t.Fatal("suppression creation committed", err)
	}
	if err := f.db.QueryRow(`SELECT count(*) FROM events WHERE recipient_id=$1 AND event_type='UNKNOWN_RESOLVED'`, rid).Scan(&count); err != nil || count != 0 {
		t.Fatal("orphan resolution event", err)
	}
}
