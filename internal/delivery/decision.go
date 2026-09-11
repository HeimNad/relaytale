// Package delivery decides policy from immutable SMTP evidence; it performs no I/O.
package delivery

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"time"
)

type Action string

const (
	Accept      Action = "ACCEPT"
	Retry       Action = "RETRY_SAME_PROVIDER"
	Permanent   Action = "PERMANENT_FAILURE"
	Unknown     Action = "DELIVERY_UNKNOWN"
	Manual      Action = "MANUAL_INTERVENTION"
	MaxAttempts        = 7
	Window             = 24 * time.Hour
)

type Evidence struct {
	Stage         string `json:"stage"`
	ErrorClass    string `json:"error_class"`
	Status        string `json:"status"`
	Code          int    `json:"smtp_code"`
	FinalCode     int    `json:"final_code"`
	FinalResponse bool   `json:"final_response"`
	BodyStarted   bool   `json:"body_started"`
}
type Decision struct {
	Version          int           `json:"version"`
	Action           Action        `json:"action"`
	Reason           string        `json:"reason"`
	Scope            string        `json:"scope"`
	RetryAfter       time.Duration `json:"retry_after_ns"`
	MayHaveDelivered bool          `json:"may_have_delivered"`
	FailoverAllowed  bool          `json:"failover_allowed"`
	Terminal         bool          `json:"terminal"`
}
type Budget struct {
	Attempts       int
	StartedAt, Now time.Time
	Seed           string
}

func Decide(e Evidence, b Budget) Decision {
	d := Decision{Version: 1, Action: Manual, Reason: "UNCLASSIFIED_EVIDENCE", Scope: "unknown"}
	rcRejected := e.Stage == "RCPT_REJECTED" && e.Code >= 400 && e.Code < 600
	switch {
	case e.Status == "SMTP_ACCEPTED" && e.FinalResponse && e.FinalCode >= 200 && e.FinalCode < 300:
		d.Action = Accept
		d.Reason = "FINAL_2XX"
		d.Terminal = true
		d.MayHaveDelivered = true
		d.Scope = "none"
		return d
	case e.Status == "DELIVERY_UNKNOWN" || (!rcRejected && e.BodyStarted && !(e.FinalResponse && e.FinalCode >= 400 && e.FinalCode < 600)):
		d.Action = Unknown
		d.Reason = "REMOTE_ACCEPTANCE_UNCERTAIN"
		d.MayHaveDelivered = true
		return d
	case rcRejected:
		d.Scope = "recipient"
		d.Reason = "RCPT_REJECTED"
		if e.Code >= 500 {
			d.Action = Permanent
			d.Terminal = true
			return d
		}
		d.Action = Retry
	case e.ErrorClass == "AUTH_ERROR" && e.Code >= 400 && e.Code < 500:
		d.Action = Retry
		d.Reason = "AUTH_TEMPORARY_FAILURE"
		d.Scope = "provider"
	case e.ErrorClass == "AUTH_ERROR" || e.ErrorClass == "CREDENTIAL_DECRYPTION_ERROR":
		d.Reason = "CONFIG_ERROR"
		d.Scope = "configuration"
		return d
	case e.ErrorClass == "LOCAL_STORAGE_ERROR" || e.ErrorClass == "DATABASE_ERROR" || e.ErrorClass == "RECORDER_ERROR":
		d.Reason = e.ErrorClass
		d.Scope = "local"
		return d
	case e.ErrorClass == "INVALID_EML" || e.ErrorClass == "INVALID_ENVELOPE":
		d.Reason = e.ErrorClass
		d.Scope = "message"
		return d
	case e.Status == "PERM_FAILED" && (e.Stage == "SMTP_GREETING_ERROR" || e.Stage == "EHLO_ERROR" || e.Stage == "STARTTLS_ERROR"):
		d.Reason = "PROVIDER_SETUP_REJECTED"
		d.Scope = "provider"
		return d
	case e.Status == "PERM_FAILED" && ((e.Code >= 500 && e.Code < 600) || (e.FinalResponse && e.FinalCode >= 500 && e.FinalCode < 600)):
		d.Action = Permanent
		d.Reason = "EXPLICIT_5XX"
		d.Scope = "message"
		d.Terminal = true
		return d
	case e.Status == "TEMP_FAILED" && safeTemporary(e):
		d.Action = Retry
		d.Reason = e.ErrorClass
		d.Scope = "provider"
		// Permission only. Phase 4A always pins the original Provider.
		d.FailoverAllowed = !e.BodyStarted && (e.Stage == "DNS_ERROR" || e.Stage == "CONNECT_ERROR" || e.Stage == "TLS_ERROR" || e.Stage == "DATA_REJECTED")
	default:
		return d
	}
	if b.Attempts < 1 || b.StartedAt.IsZero() || b.Now.Before(b.StartedAt) {
		d.Action = Manual
		d.FailoverAllowed = false
		d.Reason = "INVALID_RETRY_BUDGET"
		return d
	}
	if b.Attempts >= MaxAttempts || !b.Now.Before(b.StartedAt.Add(Window)) {
		d.Action = Manual
		d.FailoverAllowed = false
		d.Reason = "RETRY_BUDGET_EXHAUSTED"
		return d
	}
	base := time.Minute * time.Duration(1<<uint(2*(b.Attempts-1)))
	if base > 12*time.Hour {
		base = 12 * time.Hour
	}
	hash := sha256.Sum256([]byte(b.Seed))
	// Stable jitter is computed once and the resulting deadline is persisted.
	factor := 800 + binary.BigEndian.Uint16(hash[:2])%401
	d.RetryAfter = base * time.Duration(factor) / 1000
	if !b.Now.Add(d.RetryAfter).Before(b.StartedAt.Add(Window)) {
		d.Action = Manual
		d.FailoverAllowed = false
		d.Reason = "RETRY_WINDOW_EXHAUSTED"
		d.RetryAfter = 0
	}
	return d
}
func safeTemporary(e Evidence) bool {
	if e.Code >= 400 && e.Code < 500 {
		return true
	}
	if e.FinalResponse && e.FinalCode >= 400 && e.FinalCode < 500 {
		return true
	}
	for _, prefix := range []string{"DNS_", "CONNECT_", "TLS_", "STARTTLS_", "SMTP_GREETING_", "EHLO_", "AUTH_CONNECTION_"} {
		if strings.HasPrefix(e.ErrorClass, prefix) {
			return true
		}
	}
	return e.ErrorClass == "CONNECTION_RESET" && !e.BodyStarted
}
