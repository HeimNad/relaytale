package delivery

import (
	"testing"
	"time"
)

func TestDecisionMatrix(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	budget := Budget{Attempts: 1, StartedAt: now, Now: now, Seed: "stable"}
	cases := []struct {
		name          string
		e             Evidence
		action        Action
		scope         string
		may, failover bool
	}{
		{"DNS", Evidence{Stage: "DNS_ERROR", ErrorClass: "DNS_ERROR", Status: "TEMP_FAILED"}, Retry, "provider", false, true},
		{"refused", Evidence{Stage: "CONNECT_ERROR", ErrorClass: "CONNECT_REFUSED", Status: "TEMP_FAILED"}, Retry, "provider", false, true},
		{"TLS", Evidence{Stage: "TLS_ERROR", ErrorClass: "TLS_ERROR", Status: "TEMP_FAILED"}, Retry, "provider", false, true},
		{"auth", Evidence{Stage: "AUTH_ERROR", ErrorClass: "AUTH_ERROR", Status: "PERM_FAILED", Code: 535}, Manual, "configuration", false, false},
		{"auth temporary", Evidence{Stage: "AUTH_ERROR", ErrorClass: "AUTH_ERROR", Status: "TEMP_FAILED", Code: 454}, Retry, "provider", false, false},
		{"auth disconnect", Evidence{Stage: "AUTH_ERROR", ErrorClass: "AUTH_CONNECTION_ERROR", Status: "TEMP_FAILED"}, Retry, "provider", false, false},
		{"greeting 554", Evidence{Stage: "SMTP_GREETING_ERROR", ErrorClass: "SMTP_GREETING_ERROR", Status: "PERM_FAILED", Code: 554}, Manual, "provider", false, false},
		{"local", Evidence{ErrorClass: "LOCAL_STORAGE_ERROR", Status: "TEMP_FAILED"}, Manual, "local", false, false},
		{"RCPT 450 despite other recipients sent", Evidence{Stage: "RCPT_REJECTED", Status: "TEMP_FAILED", Code: 450, BodyStarted: true, FinalResponse: true, FinalCode: 250}, Retry, "recipient", false, false},
		{"RCPT 550", Evidence{Stage: "RCPT_REJECTED", Status: "PERM_FAILED", Code: 550}, Permanent, "recipient", false, false},
		{"DATA command 451", Evidence{Stage: "DATA_REJECTED", ErrorClass: "DATA_REJECTED", Status: "TEMP_FAILED", Code: 451}, Retry, "provider", false, true},
		{"final 451", Evidence{Stage: "FINAL_RESPONSE_ERROR", ErrorClass: "FINAL_RESPONSE_ERROR", Status: "TEMP_FAILED", Code: 451, BodyStarted: true, FinalResponse: true, FinalCode: 451}, Retry, "provider", false, false},
		{"final timeout", Evidence{Stage: "FINAL_RESPONSE_ERROR", ErrorClass: "FINAL_RESPONSE_TIMEOUT", Status: "DELIVERY_UNKNOWN", BodyStarted: true}, Unknown, "unknown", true, false},
		{"final 250", Evidence{Stage: "FINAL_RESPONSE", Status: "SMTP_ACCEPTED", FinalResponse: true, FinalCode: 250, BodyStarted: true}, Accept, "none", true, false},
		{"contradictory final acceptance", Evidence{Stage: "CONNECT_ERROR", ErrorClass: "CONNECT_ERROR", Status: "TEMP_FAILED", FinalResponse: true, FinalCode: 250}, Unknown, "unknown", true, false},
		{"DATA connection reset", Evidence{Stage: "DATA_REJECTED", ErrorClass: "CONNECTION_RESET", Status: "TEMP_FAILED"}, Retry, "provider", false, false},
		{"unclassified failure", Evidence{Status: "TEMP_FAILED", ErrorClass: "SOMETHING_NEW"}, Manual, "unknown", false, false},
		{"contradictory temporary after body", Evidence{Status: "TEMP_FAILED", BodyStarted: true}, Unknown, "unknown", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(tc.e, budget)
			if d.Action != tc.action || d.Scope != tc.scope || d.MayHaveDelivered != tc.may || d.FailoverAllowed != tc.failover {
				t.Fatalf("%+v", d)
			}
			if d.Action == Retry && (d.RetryAfter < 48*time.Second || d.RetryAfter > 72*time.Second) {
				t.Fatal("bad jitter")
			}
		})
	}
}
func TestRetryBudget(t *testing.T) {
	now := time.Now().UTC()
	e := Evidence{Stage: "RCPT_REJECTED", Code: 450, Status: "TEMP_FAILED"}
	for _, b := range []Budget{{Attempts: 7, StartedAt: now, Now: now}, {Attempts: 1, StartedAt: now.Add(-Window), Now: now}, {Attempts: 1, StartedAt: now.Add(-Window + time.Second), Now: now}, {Attempts: 0, StartedAt: now, Now: now}} {
		if d := Decide(e, b); d.Action != Manual || d.Terminal {
			t.Fatal(d)
		}
	}
	b := Budget{Attempts: 3, StartedAt: now, Now: now, Seed: "persisted-attempt"}
	if Decide(e, b) != Decide(e, b) {
		t.Fatal("unstable policy")
	}
}
