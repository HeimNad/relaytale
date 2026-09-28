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
	"relaytale/internal/message"
	"relaytale/internal/queue"
	"relaytale/internal/testsmtp"
)

func statusRequest(h http.Handler, key api.SendKey, id, query string, edit func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/v1/messages/"+id+query, nil)
	r.Header.Set("Authorization", "Bearer "+key.Token)
	if edit != nil {
		edit(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func decodeStatus(t *testing.T, w *httptest.ResponseRecorder) api.MessageStatus {
	t.Helper()
	requireStatus(t, w, 200)
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing privacy headers")
	}
	var out api.MessageStatus
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func submitStatusFixture(t *testing.T, h http.Handler, key api.SendKey, raw string, edit func(*http.Request)) string {
	t.Helper()
	w := sendRequest(h, key, uuid.NewString(), raw, edit)
	requireStatus(t, w, 202)
	var result message.EnqueueResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result.MessageID
}

func TestHTTPStatusOwnershipRotationAndReadOnly(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, receiver := sendFixture(t, f)
	id := submitStatusFixture(t, h, key, f.raw(), nil)
	var beforeAudit int
	if err := f.db.QueryRow(`SELECT count(*) FROM maintenance_audit`).Scan(&beforeAudit); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(api.Handler(f.db, func() error { return nil }, nil, nil, h))
	defer server.Close()
	req, _ := http.NewRequest("GET", server.URL+"/v1/messages/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+key.Token)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out api.MessageStatus
	err = json.NewDecoder(response.Body).Decode(&out)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || out.MessageID != id || out.Status != "QUEUED" || len(out.Recipients) != 1 || len(out.Events) != 3 || out.SchemaVersion != 1 || out.FeedbackAvailability != "NOT_EXPOSED" {
		t.Fatal(out, err, response.StatusCode)
	}
	var afterAudit int
	if err = f.db.QueryRow(`SELECT count(*) FROM maintenance_audit`).Scan(&afterAudit); err != nil || beforeAudit != afterAudit {
		t.Fatal("GET changed audit", err)
	}
	assertCount(t, f, 0, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1`, id)
	assertCount(t, f, 1, `SELECT count(*) FROM api_submission_ledger WHERE message_id=$1`, id)
	if emlCount(t, f.root) != 1 {
		t.Fatal("GET modified archives")
	}
	other, otherKey, _ := sendFixture(t, f)
	hidden := statusRequest(other, otherKey, id, "", nil)
	absent := statusRequest(other, otherKey, uuid.NewString(), "", nil)
	requireStatus(t, hidden, 404)
	requireStatus(t, absent, 404)
	if hidden.Body.String() != absent.Body.String() {
		t.Fatal("foreign ownership leaked")
	}
	smtpID := f.seed(t)
	requireStatus(t, statusRequest(h, key, smtpID, "", nil), 404)
	mustExec(t, f, `UPDATE api_keys SET enabled=false WHERE id=$1`, key.ID)
	requireStatus(t, statusRequest(h, key, id, "", nil), 401)
	old := key
	key.Token = "rts_" + strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")
	config, _ := json.Marshal([]api.SendKey{key})
	rotated, err := api.NewSendHandler(context.Background(), string(config), receiver)
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, statusRequest(rotated, old, id, "", nil), 401)
	requireStatus(t, statusRequest(h, old, id, "", nil), 401) // stale handler also checks persisted hash
	got := decodeStatus(t, statusRequest(rotated, key, id, "", nil))
	if got.MessageID != id {
		t.Fatal(got)
	}
	disabled, err := api.NewSendHandler(context.Background(), "", receiver)
	if err != nil || disabled != nil {
		t.Fatal("default enabled", err)
	}
	mux := api.Handler(f.db, func() error { return nil }, nil, nil, disabled)
	requireStatus(t, statusRequest(mux, key, id, "", nil), 404)
}

func TestHTTPStatusRecipientOutcomesAndManualAssertion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options testsmtp.Options
		want    string
		code    int
	}{
		{"accepted", testsmtp.Options{}, "SMTP_ACCEPTED", 250},
		{"partial", testsmtp.Options{RejectRecipients: map[string]int{"two@example.test": 550}}, "PARTIAL_ACCEPTED", 550},
		{"temporary", testsmtp.Options{FinalCode: 451}, "TEMP_FAILED", 451},
		{"permanent", testsmtp.Options{FinalCode: 550}, "PERM_FAILED", 550},
		{"unknown", testsmtp.Options{DropFinal: true}, "DELIVERY_UNKNOWN", 0},
		{"auth", testsmtp.Options{RejectAuth: true}, "TEMP_FAILED", 535},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := delivery(t, tc.options, 1)
			h, key, _ := sendFixture(t, f)
			id := submitStatusFixture(t, h, key, f.raw(), func(r *http.Request) { r.Header.Add("X-RelayTale-To", "two@example.test") })
			runOne(t, f.worker)
			got := decodeStatus(t, statusRequest(h, key, id, "", nil))
			if got.Status != tc.want || len(got.Recipients) != 2 {
				t.Fatal(got)
			}
			foundCode := false
			for _, rc := range got.Recipients {
				if rc.AttemptCount != 1 || rc.LatestCompletedAttempt == nil || rc.LatestCompletedAttempt.Stage == "UNCLASSIFIED" {
					t.Fatal(rc)
				}
				a := rc.LatestCompletedAttempt
				if a.SMTPCode != nil && *a.SMTPCode == tc.code {
					foundCode = true
				}
				if tc.name == "partial" && rc.Address == "two@example.test" && (rc.Status != "PERM_FAILED" || a.Stage != "RCPT_REJECTED") {
					t.Fatal(rc)
				}
				if tc.name == "accepted" && a.Stage != "FINAL_RESPONSE" {
					t.Fatal(rc)
				}
				if tc.name == "auth" && rc.DecisionAction != "MANUAL_INTERVENTION" {
					t.Fatal(rc)
				}
			}
			if tc.code > 0 && !foundCode {
				t.Fatal("SMTP numeric outcome lost", got)
			}
			if tc.name == "unknown" {
				for _, rc := range got.Recipients {
					_, err := (queue.Repository{DB: f.db}).ResolveUnknown(context.Background(), queue.Resolution{RecipientID: rc.ID, ExpectedAttempt: rc.LatestCompletedAttempt.ID, Action: "mark-delivered", Actor: "status-test", Reason: "synthetic operator assertion"})
					if err != nil {
						t.Fatal(err)
					}
				}
				resolved := decodeStatus(t, statusRequest(h, key, id, "", nil))
				if resolved.Status != "DELIVERED" || !strings.Contains(resolved.Description, "not independent inbox verification") {
					t.Fatal(resolved)
				}
				for _, rc := range resolved.Recipients {
					if rc.DecisionAction != "MANUAL_RESOLUTION" || rc.LatestCompletedAttempt.Status != "DELIVERY_UNKNOWN" {
						t.Fatal("manual action rewrote SMTP evidence", rc)
					}
				}
			}
			assertCount(t, f, 1, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1`, id)
		})
	}
}

func TestHTTPStatusPaginationAndRedaction(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sendFixture(t, f)
	id := submitStatusFixture(t, h, key, f.raw(), nil)
	otherID := f.seed(t)
	var otherRecipient string
	if err := f.db.QueryRow(`SELECT id FROM recipients WHERE message_id=$1 LIMIT 1`, otherID).Scan(&otherRecipient); err != nil {
		t.Fatal(err)
	}
	// Same timestamp forces the ID tie-breaker. Free-form source/data must not leak.
	at := time.Now().UTC().Add(time.Minute)
	for i := 0; i < 7; i++ {
		mustExec(t, f, `INSERT INTO events(id,message_id,recipient_id,event_type,event_time,source,data) VALUES($1,$2,$3,'ATTEMPT_FAILED',$4,'sensitive-provider-name','{"secret":"do-not-expose"}')`, uuid.NewString(), id, otherRecipient, at)
	}
	privateEvent := uuid.NewString()
	mustExec(t, f, `INSERT INTO events(id,message_id,event_type,source,data) VALUES($1,$2,'PRIVATE-secret-event','worker','{}')`, privateEvent, id)
	seen := map[string]bool{}
	before := ""
	for page := 0; page < 5; page++ {
		q := "?limit=3"
		if before != "" {
			q += "&before=" + before
		}
		w := statusRequest(h, key, id, q, nil)
		got := decodeStatus(t, w)
		if strings.Contains(w.Body.String(), "sensitive") || strings.Contains(w.Body.String(), "do-not-expose") || strings.Contains(w.Body.String(), "PRIVATE") || strings.Contains(w.Body.String(), otherRecipient) {
			t.Fatal("private data leaked")
		}
		for _, event := range got.Events {
			if seen[event.ID] {
				t.Fatal("duplicate page event")
			}
			seen[event.ID] = true
		}
		if !got.HasMore {
			if got.NextBefore != "" {
				t.Fatal("unexpected cursor")
			}
			break
		}
		if got.NextBefore == "" || got.NextBefore == before {
			t.Fatal("cursor stalled")
		}
		before = got.NextBefore
	}
	if len(seen) != 10 {
		t.Fatal("pagination lost events", len(seen))
	}
	var foreignCursor string
	if err := f.db.QueryRow(`SELECT id FROM events WHERE message_id=$1 LIMIT 1`, otherID).Scan(&foreignCursor); err != nil {
		t.Fatal(err)
	}
	w1 := statusRequest(h, key, id, "?before="+foreignCursor, nil)
	w2 := statusRequest(h, key, id, "?before="+uuid.NewString(), nil)
	requireStatus(t, w1, 400)
	requireStatus(t, w2, 400)
	if w1.Body.String() != w2.Body.String() {
		t.Fatal("cursor existence leaked")
	}
	requireStatus(t, statusRequest(h, key, id, "?before="+privateEvent, nil), 400)
}

func TestHTTPStatusRateLimitIndependentFromSubmission(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sendFixture(t, f)
	id := submitStatusFixture(t, h, key, f.raw(), nil)
	limited := false
	for i := 0; i < 30; i++ {
		w := statusRequest(h, key, id, "", nil)
		if w.Code == 429 {
			if w.Header().Get("Retry-After") == "" {
				t.Fatal("missing backoff")
			}
			limited = true
			break
		}
		requireStatus(t, w, 200)
	}
	if !limited {
		t.Fatal("polling unbounded")
	}
	requireStatus(t, sendRequest(h, key, "still-can-send", f.raw(), nil), 202)
}

func TestHTTPStatusPrivateSMTPTextAndDatabaseFailure(t *testing.T) {
	f := delivery(t, testsmtp.Options{FinalCode: 550}, 1)
	h, key, _ := sendFixture(t, f)
	id := submitStatusFixture(t, h, key, f.raw(), nil)
	runOne(t, f.worker)
	mustExec(t, f, `UPDATE messages SET last_error='PRIVATE-sentinel',subject='PRIVATE-sentinel',metadata='{"secret":"PRIVATE-sentinel"}' WHERE id=$1`, id)
	mustExec(t, f, `UPDATE delivery_attempts SET smtp_response='PRIVATE-sentinel',error_message='PRIVATE-sentinel',raw_debug_log='PRIVATE-sentinel' WHERE message_id=$1`, id)
	mustExec(t, f, `UPDATE attempt_recipients SET smtp_response='PRIVATE-sentinel',smtp_enhanced_code='5.1.1 PRIVATE',protocol_stage='PRIVATE-stage' WHERE attempt_id IN (SELECT id FROM delivery_attempts WHERE message_id=$1)`, id)
	w := statusRequest(h, key, id, "", nil)
	got := decodeStatus(t, w)
	if strings.Contains(w.Body.String(), "PRIVATE") || got.Recipients[0].LatestCompletedAttempt.EnhancedCode != "" || got.Recipients[0].LatestCompletedAttempt.Stage != "UNCLASSIFIED" {
		t.Fatal("free text leaked")
	}
	// A canceled read must not turn a database failure into a false empty result.
	canceled := statusRequest(h, key, id, "", func(r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		cancel()
		*r = *r.WithContext(ctx)
	})
	requireStatus(t, canceled, 503)
}

func TestHTTPStatusRequestGuards(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sendFixture(t, f)
	id := submitStatusFixture(t, h, key, f.raw(), nil)
	for _, tc := range []struct {
		name string
		code int
		edit func(*http.Request)
	}{
		{"missing", 401, func(r *http.Request) { r.Header.Del("Authorization") }},
		{"duplicate", 401, func(r *http.Request) { r.Header.Add("Authorization", "Bearer "+key.Token) }},
		{"wrong", 401, func(r *http.Request) { r.Header.Set("Authorization", "Bearer rts_"+strings.Repeat("0", 64)) }},
		{"browser", 403, func(r *http.Request) { r.Header.Set("Origin", "https://website.example") }},
		{"method", 405, func(r *http.Request) { r.Method = "POST" }},
		{"body", 400, func(r *http.Request) { r.ContentLength = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) { requireStatus(t, statusRequest(h, key, id, "", tc.edit), tc.code) })
	}
	decodeStatus(t, statusRequest(h, key, id, "", nil))
}
