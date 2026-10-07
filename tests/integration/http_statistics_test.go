package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"relaytale/internal/api"
	"relaytale/internal/testsmtp"
)

func statisticsRequest(h http.Handler, key api.SendKey, query string, edit func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/v1/statistics?"+query, nil)
	r.Header.Set("Authorization", "Bearer "+key.Token)
	if edit != nil {
		edit(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func decodeStatistics(t *testing.T, w *httptest.ResponseRecorder) api.WebsiteStatistics {
	t.Helper()
	requireStatus(t, w, 200)
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing privacy headers")
	}
	var out api.WebsiteStatistics
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWebsiteStatisticsOwnershipWindowAndReadOnly(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, receiver := sendFixture(t, f)
	from := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	until := from.Add(time.Hour)
	query := "from=" + from.Format(time.RFC3339Nano) + "&until=" + until.Format(time.RFC3339Nano)
	ids := []string{}
	for i := 0; i < 4; i++ {
		ids = append(ids, submitStatusFixture(t, h, key, f.raw(), nil))
	}
	for i, at := range []time.Time{from, from.Add(time.Minute), until, from.Add(-time.Microsecond)} {
		mustExec(t, f, "UPDATE messages SET created_at=$2 WHERE id=$1", ids[i], at)
	}
	mustExec(t, f, "UPDATE messages SET status='SMTP_ACCEPTED' WHERE id=$1", ids[0])
	mustExec(t, f, "UPDATE recipients SET status='SMTP_ACCEPTED' WHERE message_id=$1", ids[0])
	mustExec(t, f, "UPDATE messages SET status='DELIVERY_UNKNOWN' WHERE id=$1", ids[1])
	other, otherKey, _ := sendFixture(t, f)
	foreign := submitStatusFixture(t, other, otherKey, f.raw(), nil)
	mustExec(t, f, "UPDATE messages SET created_at=$2 WHERE id=$1", foreign, from)
	smtp := f.seed(t)
	mustExec(t, f, "UPDATE messages SET created_at=$2 WHERE id=$1", smtp, from)
	var size int64
	var totalAudit int
	if err := f.db.QueryRow("SELECT eml_size FROM messages WHERE id=$1", ids[0]).Scan(&size); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow("SELECT count(*) FROM maintenance_audit").Scan(&totalAudit); err != nil {
		t.Fatal(err)
	}
	// Exercise the mounted /v1 route over a real local TLS HTTP server.
	server := httptest.NewTLSServer(api.Handler(f.db, func() error { return nil }, nil, nil, h))
	defer server.Close()
	req, _ := http.NewRequest("GET", server.URL+"/v1/statistics?"+query, nil)
	req.Header.Set("Authorization", "Bearer "+key.Token)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out api.WebsiteStatistics
	err = json.NewDecoder(response.Body).Decode(&out)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || out.MessageCount != 2 || out.MessagesByStatus["SMTP_ACCEPTED"] != 1 || out.MessagesByStatus["DELIVERY_UNKNOWN"] != 1 || out.ArchivesByState["AVAILABLE"] != 2 || out.LogicalRawBytesByArchiveState["AVAILABLE"] != 2*size || out.Simulation || out.Environment != "production" || out.FeedbackAvailability != "NOT_EXPOSED" {
		t.Fatal(out, err, response.StatusCode)
	}
	data, _ := json.Marshal(out)
	for _, private := range []string{ids[0], foreign, "one@example.test", f.root, "sender@"} {
		if strings.Contains(string(data), private) {
			t.Fatal("private field leaked", private)
		}
	}
	if got := decodeStatistics(t, statisticsRequest(other, otherKey, query, nil)); got.MessageCount != 1 {
		t.Fatal("foreign identity aggregation", got)
	}
	assertCount(t, f, totalAudit, "SELECT count(*) FROM maintenance_audit")
	assertCount(t, f, 0, "SELECT count(*) FROM delivery_attempts")
	if emlCount(t, f.root) != 6 || f.fake.Connections.Load() != 0 {
		t.Fatal("statistics changed files or sent mail")
	}
	requireStatus(t, statisticsRequest(h, key, query, func(r *http.Request) { r.Method = "POST" }), 405)
	requireStatus(t, statisticsRequest(h, key, query, func(r *http.Request) { r.Header.Set("Origin", "https://example.test") }), 403)
	requireStatus(t, statisticsRequest(h, key, query, func(r *http.Request) { r.Header.Del("Authorization") }), 401)
	mustExec(t, f, "UPDATE api_keys SET enabled=false WHERE id=$1", key.ID)
	requireStatus(t, statisticsRequest(h, key, query, nil), 401)
	old := key
	key.Token = "rts_" + strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")
	config, _ := json.Marshal([]api.SendKey{key})
	rotated, err := api.NewSendHandler(context.Background(), string(config), receiver)
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, statisticsRequest(h, old, query, nil), 401)
	if got := decodeStatistics(t, statisticsRequest(rotated, key, query, nil)); got.MessageCount != 2 {
		t.Fatal("rotation lost identity", got)
	}
}

func TestWebsiteStatisticsSandboxAndWorkLimit(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sandboxFixture(t, f)
	submitStatusFixture(t, h, key, f.raw(), nil)
	until := time.Now().UTC().Truncate(time.Microsecond)
	from := until.Add(-time.Hour)
	query := "from=" + from.Format(time.RFC3339Nano) + "&until=" + until.Format(time.RFC3339Nano)
	out := decodeStatistics(t, statisticsRequest(h, key, query, nil))
	if !out.Simulation || out.Environment != "sandbox" || out.MessagesByStatus["SANDBOX_STORED"] != 1 {
		t.Fatal(out)
	}
	emptyQuery := "from=" + from.Add(-time.Hour).Format(time.RFC3339Nano) + "&until=" + from.Format(time.RFC3339Nano)
	if got := decodeStatistics(t, statisticsRequest(h, key, emptyQuery, nil)); got.MessageCount != 0 || len(got.MessagesByStatus) != 0 {
		t.Fatal(got)
	}
	futureQuery := "from=" + from.Format(time.RFC3339Nano) + "&until=" + until.Add(time.Hour).Format(time.RFC3339Nano)
	requireStatus(t, statisticsRequest(h, key, futureQuery, nil), 400)
	bulk := `INSERT INTO messages(id,source_type,api_key_id,environment,envelope_from,status,eml_path,eml_size,eml_sha256,created_at)
 SELECT gen_random_uuid(),'api',$1,'sandbox','synthetic@example.test','SANDBOX_STORED','/synthetic/statistics/'||n||'.eml',1,$2,$3 FROM generate_series($4::int,$5::int) n`
	mustExec(t, f, bulk, key.ID, strings.Repeat("a", 64), from, 1, 9999)
	if got := decodeStatistics(t, statisticsRequest(h, key, query, nil)); got.MessageCount != 10000 {
		t.Fatal("exact limit rejected", got)
	}
	mustExec(t, f, bulk, key.ID, strings.Repeat("a", 64), from, 10000, 10000)
	overloaded := statisticsRequest(h, key, query, nil)
	requireStatus(t, overloaded, 422)
	if strings.Contains(overloaded.Body.String(), "message_count") {
		t.Fatal("partial statistics published")
	}
	if f.fake.Connections.Load() != 0 {
		t.Fatal("sandbox statistics sent mail")
	}
}
