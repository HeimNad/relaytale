package api

import "net/http"

const providerView = `SELECT p.id,p.name,p.enabled,p.host,p.port,p.security,p.username,p.priority,p.max_connections,p.timeout_seconds,p.from_domains,coalesce(p.hourly_limit,0) hourly_limit,coalesce(p.daily_limit,0) daily_limit,p.revision,p.created_at,
 h.status health_status,h.circuit_state,h.open_until,h.last_success_at,h.last_failure_at,
 coalesce((SELECT sum(q.units) FROM provider_quota q WHERE q.provider_id=p.id AND q.reserved_at>clock_timestamp()-interval '1 hour'),0) hourly_reserved,
 coalesce((SELECT sum(q.units) FROM provider_quota q WHERE q.provider_id=p.id AND q.reserved_at>clock_timestamp()-interval '24 hours'),0) daily_reserved
 FROM providers p LEFT JOIN provider_health h ON h.provider_id=p.id`

func (a Management) providers(w http.ResponseWriter, r *http.Request) {
	a.list(w, r, `SELECT v.id,to_jsonb(v) FROM (`+providerView+` WHERE p.id>$1 ORDER BY p.id LIMIT $2) v ORDER BY v.id`)
}
func (a Management) provider(w http.ResponseWriter, r *http.Request) {
	a.one(w, r, `SELECT to_jsonb(v) FROM (`+providerView+` WHERE p.id=$1) v`)
}

const messageView = `SELECT id,left(message_id,998) message_id,source_type,left(envelope_from,320) envelope_from,left(header_from,320) header_from,left(subject,2048) subject,created_at,completed_at,status,eml_size,archive_state,attempt_count,next_attempt_at FROM messages`

func (a Management) messages(w http.ResponseWriter, r *http.Request) {
	a.list(w, r, `SELECT v.id,to_jsonb(v) FROM (`+messageView+` WHERE id>$1 ORDER BY id LIMIT $2) v ORDER BY v.id`)
}
func (a Management) message(w http.ResponseWriter, r *http.Request) {
	a.one(w, r, `SELECT to_jsonb(v) FROM (`+messageView+` WHERE id=$1) v`)
}
func (a Management) recipients(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	a.list(w, r, `SELECT v.id,to_jsonb(v) FROM (SELECT r.id,r.address,r.status,r.provider_id,r.smtp_code,r.completed_at,r.retry_at,(SELECT a.id FROM delivery_attempts a JOIN attempt_recipients ar ON ar.attempt_id=a.id WHERE ar.recipient_id=r.id ORDER BY a.attempt_number DESC LIMIT 1) latest_attempt_id FROM recipients r WHERE r.message_id=$3 AND r.id>$1 ORDER BY r.id LIMIT $2) v ORDER BY v.id`, id)
}
func (a Management) attempts(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	a.list(w, r, `SELECT v.id,to_jsonb(v) FROM (SELECT id,provider_id,attempt_number,started_at,finished_at,result,smtp_code,error_class,bytes_sent,data_started_at,final_response_at,health_outcome FROM delivery_attempts WHERE message_id=$3 AND id>$1 ORDER BY id LIMIT $2) v ORDER BY v.id`, id)
}
func (a Management) events(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	a.list(w, r, `SELECT v.id,to_jsonb(v) FROM (SELECT id,recipient_id,attempt_id,event_type,event_time,source,attempt_sequence FROM events WHERE message_id=$3 AND ($1::uuid='00000000-0000-0000-0000-000000000000' OR (event_time,id)>(SELECT event_time,id FROM events WHERE id=$1 AND message_id=$3)) ORDER BY event_time,id LIMIT $2) v ORDER BY v.event_time,v.id`, id)
}
func (a Management) suppressions(w http.ResponseWriter, r *http.Request) {
	a.list(w, r, `SELECT v.id,to_jsonb(v) FROM (SELECT id,email,reason category,source,created_at,expires_at,released_at,created_by actor,note,released_at IS NULL AND (expires_at IS NULL OR expires_at>clock_timestamp()) active FROM suppression_entries WHERE id>$1 ORDER BY id LIMIT $2) v ORDER BY v.id`)
}
