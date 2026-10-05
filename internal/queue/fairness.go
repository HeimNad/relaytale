package queue

import (
	"context"
	"database/sql"
)

// FairRecipientQuantum bounds one identity's opportunity; it is not a rate limit.
const FairRecipientQuantum = 16

func (r Repository) selectFair(ctx context.Context, tx *sql.Tx, j *Job) (string, int, error) {
	eligible := `m.status='QUEUED' AND m.archive_state='AVAILABLE' AND m.next_attempt_at<=now()
 AND EXISTS(SELECT 1 FROM recipients rc WHERE rc.message_id=m.id AND ` + r.claimRecipient("rc") + `)
 AND EXISTS(SELECT 1 FROM providers p WHERE ` + r.eligibleProvider() + `)`
	var owner string
	var cursor int
	err := tx.QueryRowContext(ctx, `SELECT o.owner_key,o.stream_cursor FROM queue_fair_owners o WHERE EXISTS(SELECT 1 FROM messages m WHERE queue_owner(m.source_type,m.api_key_id,m.smtp_account_id)=o.owner_key AND `+eligible+`)
 ORDER BY o.last_ticket,o.owner_key LIMIT 1 FOR UPDATE OF o SKIP LOCKED`).Scan(&owner, &cursor)
	if err == sql.ErrNoRows {
		return "", 0, ErrNoJob
	}
	if err != nil {
		return "", 0, err
	}
	var next int
	err = tx.QueryRowContext(ctx, `WITH weights AS (
 SELECT names.name,coalesce(p.weight,1) AS weight FROM (VALUES('normal'),('otp'),('receipt'),('security')) names(name)
 LEFT JOIN api_message_streams p ON p.api_key_id=CASE WHEN split_part($1,':',1)='api' THEN NULLIF(split_part($1,':',2),'legacy')::uuid ELSE NULL END AND p.name=names.name
 ), lanes AS (
 SELECT name,weight,coalesce(sum(weight) OVER(ORDER BY name ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING),0)::int AS first,sum(weight) OVER()::int AS total FROM weights
 ), turns AS (
 SELECT name,total,CASE WHEN ($2::int%total)>=first AND ($2::int%total)<first+weight THEN 0 ELSE (first-($2::int%total)+total)%total END AS distance FROM lanes
 ) SELECT m.id,m.envelope_from,m.eml_path,m.eml_size,m.eml_sha256,m.message_stream,($2::int+t.distance+1)%t.total
 FROM messages m JOIN turns t ON t.name=m.message_stream
 WHERE queue_owner(m.source_type,m.api_key_id,m.smtp_account_id)=$1 AND `+eligible+`
 ORDER BY t.distance,m.created_at,m.id LIMIT 1 FOR UPDATE OF m SKIP LOCKED`, owner, cursor).Scan(&j.ID, &j.From, &j.Path, &j.Size, &j.SHA256, &j.Stream, &next)
	if err == sql.ErrNoRows {
		return "", 0, ErrNoJob
	}
	return owner, next, err
}
