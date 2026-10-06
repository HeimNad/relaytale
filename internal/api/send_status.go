package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/google/uuid"
	"relaytale/internal/message"
)

// These DTOs deliberately exclude free-form SMTP replies, event JSON, provider
// identity, subjects and MIME. The management API is not a website API.
type MessageStatus struct {
	Simulation           bool              `json:"simulation,omitempty"`
	Environment          string            `json:"environment,omitempty"`
	Business             *message.Business `json:"business,omitempty"`
	SchemaVersion        int               `json:"schema_version"`
	MessageID            string            `json:"message_id"`
	Status               string            `json:"status"`
	Description          string            `json:"description"`
	CreatedAt            time.Time         `json:"created_at"`
	SnapshotAt           time.Time         `json:"snapshot_at"`
	FeedbackAvailability string            `json:"feedback_availability"`
	Recipients           []RecipientStatus `json:"recipients"`
	Events               []StatusEvent     `json:"events"`
	HasMore              bool              `json:"has_more_events"`
	NextBefore           string            `json:"next_before,omitempty"`
}

type RecipientStatus struct {
	ID                     string         `json:"recipient_id"`
	Address                string         `json:"address"`
	Status                 string         `json:"status"`
	Description            string         `json:"description"`
	DecisionAction         string         `json:"decision_action,omitempty"`
	AttemptCount           int            `json:"attempt_count"`
	RetryAt                *time.Time     `json:"retry_at,omitempty"`
	LatestCompletedAttempt *StatusAttempt `json:"latest_completed_attempt,omitempty"`
}

type StatusAttempt struct {
	ID           string    `json:"attempt_id"`
	Number       int       `json:"attempt_number"`
	Status       string    `json:"recipient_status"`
	Stage        string    `json:"stage"`
	SMTPCode     *int      `json:"smtp_code,omitempty"`
	EnhancedCode string    `json:"smtp_enhanced_code,omitempty"`
	FinishedAt   time.Time `json:"finished_at"`
}

type StatusEvent struct {
	ID          string    `json:"event_id"`
	Type        string    `json:"type"`
	At          time.Time `json:"at"`
	Source      string    `json:"source"`
	RecipientID *string   `json:"recipient_id,omitempty"`
	AttemptID   *string   `json:"attempt_id,omitempty"`
}

var publicStatusEvents = []string{
	"SANDBOX_SIMULATED_ACCEPTED", "SANDBOX_SIMULATED_TEMP_FAILED", "SANDBOX_SIMULATED_PERM_FAILED", "SANDBOX_SIMULATED_UNKNOWN", "SANDBOX_SIMULATED_BOUNCE", "SANDBOX_STORED", "RECIPIENT_EXPIRED", "EXPIRY_GATE_BLOCKED", "MESSAGE_RECEIVED", "MESSAGE_ARCHIVED", "MESSAGE_QUEUED", "MESSAGE_SUPPRESSED",
	"SMTP_ACCEPTED", "ATTEMPT_STARTED", "ATTEMPT_FINISHED", "ATTEMPT_FAILED", "DELIVERY_DECIDED",
	"DELIVERY_UNKNOWN", "UNKNOWN_RESOLVED", "RETRY_EXHAUSTED", "RETRY_QUEUED",
	"RECIPIENT_SUPPRESSED", "SUPPRESSION_GATE_BLOCKED", "PROVIDER_FAILOVER", "LEASE_RECOVERED",
}
var enhancedStatus = regexp.MustCompile(`^[245]\.[0-9]{1,3}\.[0-9]{1,3}$`)

func canonicalUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id != uuid.Nil && id.String() == s
}

func statusPage(r *http.Request, id string) (string, int, bool) {
	if !canonicalUUID(id) || r.URL.RawPath != "" || len(r.URL.RawQuery) > 256 || r.ContentLength != 0 || len(r.TransferEncoding) > 0 || len(r.Header.Values("Content-Encoding")) > 0 {
		return "", 0, false
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", 0, false
	}
	for k, v := range q {
		if (k != "before" && k != "limit" && k != "schema_version") || len(v) != 1 {
			return "", 0, false
		}
	}
	if v, ok := q["schema_version"]; ok && v[0] != "1" && v[0] != "2" {
		return "", 0, false
	}
	before := q.Get("before")
	if _, ok := q["before"]; ok && !canonicalUUID(before) {
		return "", 0, false
	}
	limit := 50
	if v, ok := q["limit"]; ok {
		n, err := strconv.Atoi(v[0])
		if err != nil || n < 1 || n > 100 {
			return "", 0, false
		}
		limit = n
	}
	return before, limit, true
}

func readMessageStatus(ctx context.Context, w http.ResponseWriter, r *http.Request, db *sql.DB, who *sendIdentity, id string) {
	before, limit, ok := statusPage(r, id)
	if !ok {
		respond(w, 400, "invalid_query")
		return
	}
	tx, ok := beginWebsiteRead(ctx, w, db, who)
	if !ok {
		return
	}
	defer tx.Rollback()
	out := MessageStatus{SchemaVersion: 1, FeedbackAvailability: "NOT_EXPOSED", Recipients: []RecipientStatus{}, Events: []StatusEvent{}}
	var reference, stream string
	var expiresAt sql.NullTime
	var tags []byte
	err := tx.QueryRowContext(ctx, `SELECT id::text,status,created_at,transaction_timestamp(),client_reference,to_jsonb(business_tags),message_stream,expires_at FROM messages WHERE id=$1 AND api_key_id=$2 AND source_type='api'`, id, who.key.ID).Scan(&out.MessageID, &out.Status, &out.CreatedAt, &out.SnapshotAt, &reference, &tags, &stream, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		respond(w, 404, "not_found")
		return
	}
	if err != nil {
		respond(w, 503, "unavailable")
		return
	}
	if r.URL.Query().Get("schema_version") == "2" {
		out.SchemaVersion = 2
		out.Business = &message.Business{ClientReference: reference}
		if json.Unmarshal(tags, &out.Business.Tags) != nil {
			respond(w, 503, "unavailable")
			return
		}
	}
	if who.key.Environment == "sandbox" {
		out.Simulation = true
		out.Environment = "sandbox"
	}
	out.Description = statusDescription(out.Status)
	if err = readStatusRecipients(ctx, tx, id, &out); err != nil {
		respond(w, 503, "unavailable")
		return
	}
	var beforeTime time.Time
	if before != "" {
		err = tx.QueryRowContext(ctx, `SELECT event_time FROM events WHERE id=$1 AND message_id=$2 AND event_type=ANY($3::text[])`, before, id, publicStatusEvents).Scan(&beforeTime)
		if errors.Is(err, sql.ErrNoRows) {
			respond(w, 400, "invalid_cursor")
			return
		}
		if err != nil {
			respond(w, 503, "unavailable")
			return
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.id::text,e.event_type,e.event_time,left(e.source,32),r.id::text,a.id::text
 FROM events e
 LEFT JOIN recipients r ON r.id=e.recipient_id AND r.message_id=e.message_id
 LEFT JOIN delivery_attempts a ON a.id=e.attempt_id AND a.message_id=e.message_id
 WHERE e.message_id=$1 AND e.event_type=ANY($2::text[])
 AND ($3='' OR (e.event_time,e.id)<($4::timestamptz,NULLIF($3,'')::uuid))
 ORDER BY e.event_time DESC,e.id DESC LIMIT $5`, id, publicStatusEvents, before, beforeTime, limit+1)
	if err != nil {
		respond(w, 503, "unavailable")
		return
	}
	for rows.Next() {
		var event StatusEvent
		if err = rows.Scan(&event.ID, &event.Type, &event.At, &event.Source, &event.RecipientID, &event.AttemptID); err != nil {
			break
		}
		switch event.Source {
		case "api", "worker", "policy", "operator", "sandbox":
		default:
			event.Source = "internal"
		}
		out.Events = append(out.Events, event)
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil || rowErr != nil {
		respond(w, 503, "unavailable")
		return
	}
	if len(out.Events) > limit {
		out.Events = out.Events[:limit]
		out.HasMore = true
		out.NextBefore = out.Events[len(out.Events)-1].ID
	}
	if err = tx.Commit(); err != nil {
		respond(w, 503, "unavailable")
		return
	}
	w.Header().Set("X-RelayTale-Stream", stream)
	if expiresAt.Valid {
		w.Header().Set("X-RelayTale-Expires-At", expiresAt.Time.UTC().Format(time.RFC3339))
	}
	output(w, 200, out)
}

func readStatusRecipients(ctx context.Context, tx *sql.Tx, id string, out *MessageStatus) error {
	rows, err := tx.QueryContext(ctx, `SELECT r.id::text,left(r.address,254),r.status,r.attempt_count,r.retry_at,left(coalesce(r.decision->>'action',''),64),
 h.id,h.attempt_number,h.status,h.stage,h.smtp_code,h.enhanced,h.finished_at
 FROM recipients r
 LEFT JOIN LATERAL (
 SELECT a.id::text,a.attempt_number,left(ar.status,32) AS status,left(coalesce(ar.protocol_stage,''),64) AS stage,
 CASE WHEN ar.protocol_stage='FINAL_RESPONSE' THEN a.smtp_code ELSE ar.smtp_code END AS smtp_code,
 left(coalesce(CASE WHEN ar.protocol_stage='FINAL_RESPONSE' THEN a.smtp_enhanced_code ELSE ar.smtp_enhanced_code END,''),16) AS enhanced,a.finished_at
 FROM delivery_attempts a JOIN attempt_recipients ar ON ar.attempt_id=a.id AND ar.recipient_id=r.id
 WHERE a.message_id=r.message_id AND a.finished_at IS NOT NULL ORDER BY a.attempt_number DESC LIMIT 1
 ) h ON true WHERE r.message_id=$1 ORDER BY r.id LIMIT 101`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var rc RecipientStatus
		var aid, astatus, stage, enhanced sql.NullString
		var number, code sql.NullInt64
		var finished sql.NullTime
		if err = rows.Scan(&rc.ID, &rc.Address, &rc.Status, &rc.AttemptCount, &rc.RetryAt, &rc.DecisionAction, &aid, &number, &astatus, &stage, &code, &enhanced, &finished); err != nil {
			return err
		}
		rc.Description = statusDescription(rc.Status)
		rc.DecisionAction = publicAction(rc.DecisionAction)
		if aid.Valid {
			a := StatusAttempt{ID: aid.String, Number: int(number.Int64), Status: publicAttemptStatus(astatus.String), Stage: publicStage(stage.String), FinishedAt: finished.Time}
			if code.Valid && code.Int64 >= 200 && code.Int64 <= 599 {
				n := int(code.Int64)
				a.SMTPCode = &n
			}
			if enhancedStatus.MatchString(enhanced.String) {
				a.EnhancedCode = enhanced.String
			}
			rc.LatestCompletedAttempt = &a
		}
		out.Recipients = append(out.Recipients, rc)
	}
	if len(out.Recipients) > 100 {
		return errors.New("recipient projection exceeds API bound")
	}
	return rows.Err()
}

func publicAction(s string) string {
	switch s {
	case "EXPIRE", "ACCEPT", "RESUME_SAME_PROVIDER", "RETRY_SAME_PROVIDER", "PERMANENT_FAILURE", "DELIVERY_UNKNOWN", "MANUAL_INTERVENTION", "MANUAL_RESOLUTION":
		return s
	}
	return ""
}
func publicAttemptStatus(s string) string {
	switch s {
	case "SMTP_ACCEPTED", "TEMP_FAILED", "PERM_FAILED", "DELIVERY_UNKNOWN", "SUPPRESSED", "CANCELLED":
		return s
	}
	return "UNCLASSIFIED"
}
func publicStage(s string) string {
	switch s {
	case "DNS_ERROR", "CONNECT_ERROR", "TLS_ERROR", "SMTP_GREETING_ERROR", "EHLO_ERROR", "STARTTLS_ERROR", "AUTH_ERROR", "MAIL_FROM_REJECTED", "RCPT_REJECTED", "DATABASE_ERROR", "RECORDER_ERROR", "INVALID_EML", "INVALID_ENVELOPE", "SUPPRESSION_BLOCKED", "EXPIRY_BLOCKED", "DATA_REJECTED", "DATA_WRITE_ERROR", "FINAL_RESPONSE_ERROR", "FINAL_RESPONSE":
		return s
	}
	return "UNCLASSIFIED"
}
func statusDescription(s string) string {
	switch s {
	case "SANDBOX_STORED":
		return "Stored in the sandbox; no SMTP delivery is permitted or confirmed."
	case "RECEIVED", "ARCHIVED", "QUEUED":
		return "Accepted by the gateway; awaiting delivery."
	case "SENDING":
		return "A delivery attempt is in progress."
	case "SMTP_ACCEPTED":
		return "Accepted by the SMTP provider; inbox delivery is not confirmed."
	case "PARTIAL_ACCEPTED":
		return "Some recipients were accepted; inspect each recipient."
	case "TEMP_FAILED":
		return "Delivery is temporarily unsuccessful or paused; inspect the decision and retry time."
	case "PERM_FAILED":
		return "Delivery was rejected or marked failed; this does not prove the address is invalid."
	case "DELIVERY_UNKNOWN":
		return "Remote acceptance is uncertain; do not automatically resubmit."
	case "DELIVERED":
		return "Marked delivered in gateway records; this is not independent inbox verification."
	case "BOUNCED":
		return "Marked bounced in gateway records; authenticated feedback is not exposed by this endpoint."
	case "SUPPRESSED":
		return "Delivery blocked by recipient policy."
	case "EXPIRED":
		return "Delivery deadline elapsed; no further attempts are allowed. Previous SMTP evidence is retained."
	case "CANCELLED":
		return "Cancelled in gateway records; this does not retract a previously sent message."
	default:
		return "State is not classified by this API version."
	}
}
