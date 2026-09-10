package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"mailgateway/internal/provider"
	"mailgateway/internal/smtpclient"
)

var ErrNoJob = errors.New("no eligible queued message")
var ErrLeaseLost = errors.New("delivery lease lost")

type Job struct {
	ID, Token, AttemptID, From, Path, SHA256 string
	Size                                     int64
	Provider                                 provider.Provider
	Recipients                               []smtpclient.Recipient
}
type Repository struct{ DB *sql.DB }

func (r Repository) begin(ctx context.Context) (*sql.Tx, error) {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}
func event(ctx context.Context, tx *sql.Tx, j Job, kind, rid string, at time.Time, data any) error {
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	var recipient any
	if rid != "" {
		recipient = rid
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO events(id,message_id,attempt_id,recipient_id,event_type,event_time,source,data) VALUES($1,$2,$3,$4,$5,$6,'worker',$7)`, id.String(), j.ID, j.AttemptID, recipient, kind, at, string(raw))
	return err
}

const eligibleProvider = `p.enabled AND p.security IN ('starttls','implicit_tls') AND p.timeout_seconds BETWEEN 1 AND 300 AND p.max_connections BETWEEN 1 AND 32
 AND p.hourly_limit IS NULL AND p.daily_limit IS NULL
 AND lower(split_part(m.envelope_from,'@',2))=ANY(p.from_domains)
 AND (SELECT count(*) FROM delivery_attempts a JOIN messages active ON active.id=a.message_id WHERE a.provider_id=p.id AND a.result='IN_PROGRESS' AND active.lease_expires_at>clock_timestamp())<p.max_connections`

func (r Repository) Claim(ctx context.Context) (Job, error) {
	tx, err := r.begin(ctx)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	j := Job{Token: uuid.NewString(), AttemptID: uuid.Must(uuid.NewV7()).String()}
	err = tx.QueryRowContext(ctx, `SELECT m.id,m.envelope_from,m.eml_path,m.eml_size,m.eml_sha256 FROM messages m
 WHERE m.status='QUEUED' AND m.next_attempt_at<=now()
 AND EXISTS(SELECT 1 FROM recipients rc WHERE rc.message_id=m.id AND rc.status='QUEUED')
 AND EXISTS(SELECT 1 FROM providers p WHERE `+eligibleProvider+`)
 ORDER BY m.priority DESC,m.created_at,m.id LIMIT 1 FOR UPDATE OF m SKIP LOCKED`).Scan(&j.ID, &j.From, &j.Path, &j.Size, &j.SHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNoJob
	}
	if err != nil {
		return Job{}, err
	}
	var seconds int
	err = tx.QueryRowContext(ctx, `SELECT p.id,p.name,p.host,p.port,p.security,p.username,p.password_ciphertext,p.nonce,p.priority,p.max_connections,p.timeout_seconds
 FROM providers p JOIN messages m ON m.id=$1 WHERE `+eligibleProvider+` ORDER BY p.priority,p.id LIMIT 1 FOR UPDATE OF p SKIP LOCKED`, j.ID).Scan(&j.Provider.ID, &j.Provider.Name, &j.Provider.Host, &j.Provider.Port, &j.Provider.Security, &j.Provider.Username, &j.Provider.Ciphertext, &j.Provider.Nonce, &j.Provider.Priority, &j.Provider.MaxConnections, &seconds)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNoJob
	}
	if err != nil {
		return Job{}, err
	}
	j.Provider.Timeout = time.Duration(seconds) * time.Second
	// Recheck capacity under the provider lock using a fresh READ COMMITTED snapshot.
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM delivery_attempts a JOIN messages m ON m.id=a.message_id WHERE a.provider_id=$1 AND a.result='IN_PROGRESS' AND m.lease_expires_at>clock_timestamp()`, j.Provider.ID).Scan(&active); err != nil {
		return Job{}, err
	}
	if active >= j.Provider.MaxConnections {
		return Job{}, ErrNoJob
	}
	var attemptNumber int
	err = tx.QueryRowContext(ctx, `UPDATE messages SET status='SENDING',locked_at=now(),locked_by=$2,lease_expires_at=now()+($3 * interval '1 second'),attempt_count=attempt_count+1 WHERE id=$1 RETURNING attempt_count`, j.ID, j.Token, seconds+30).Scan(&attemptNumber)
	if err != nil {
		return Job{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO delivery_attempts(id,message_id,provider_id,attempt_number,result,claim_token,remote_host) VALUES($1,$2,$3,$4,'IN_PROGRESS',$5,$6)`, j.AttemptID, j.ID, j.Provider.ID, attemptNumber, j.Token, j.Provider.Host)
	if err != nil {
		return Job{}, err
	}
	rows, err := tx.QueryContext(ctx, `UPDATE recipients SET status='SENDING',provider_id=$2 WHERE message_id=$1 AND status='QUEUED' RETURNING id,address`, j.ID, j.Provider.ID)
	if err != nil {
		return Job{}, err
	}
	for rows.Next() {
		var rc smtpclient.Recipient
		if err = rows.Scan(&rc.ID, &rc.Address); err != nil {
			rows.Close()
			return Job{}, err
		}
		j.Recipients = append(j.Recipients, rc)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return Job{}, err
	}
	rows.Close()
	for _, rc := range j.Recipients {
		if _, err = tx.ExecContext(ctx, `INSERT INTO attempt_recipients(attempt_id,recipient_id,status) VALUES($1,$2,'SENDING')`, j.AttemptID, rc.ID); err != nil {
			return Job{}, err
		}
	}
	if err = event(ctx, tx, j, "ATTEMPT_STARTED", "", time.Now().UTC(), map[string]any{"provider_id": j.Provider.ID, "attempt_number": attemptNumber}); err != nil {
		return Job{}, err
	}
	if err = tx.Commit(); err != nil {
		return Job{}, err
	}
	return j, nil
}
func fence(ctx context.Context, tx *sql.Tx, j Job, requireLive bool) error {
	query := `SELECT id FROM messages WHERE id=$1 AND status='SENDING' AND locked_by=$2`
	if requireLive {
		query += ` AND lease_expires_at>clock_timestamp()`
	}
	query += ` FOR UPDATE`
	var id string
	err := tx.QueryRowContext(ctx, query, j.ID, j.Token).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseLost
	}
	return err
}
func (r Repository) ArmData(ctx context.Context, j Job) error {
	tx, err := r.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fence(ctx, tx, j, true); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE delivery_attempts SET data_armed_at=now() WHERE id=$1 AND claim_token=$2 AND result='IN_PROGRESS' AND data_armed_at IS NULL`, j.AttemptID, j.Token)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrLeaseLost
	}
	if err = event(ctx, tx, j, "DATA_ARMED", "", time.Now().UTC(), map[string]string{"reason": "durable permission to transmit DATA"}); err != nil {
		return err
	}
	return tx.Commit()
}
func nullable(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
func duration(start, end time.Time) any {
	if start.IsZero() || end.IsZero() {
		return nil
	}
	return end.Sub(start).Milliseconds()
}
func (r Repository) Finish(ctx context.Context, j Job, out smtpclient.Result) error {
	tx, err := r.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fence(ctx, tx, j, false); err != nil {
		return err
	}
	var ip any
	if out.RemoteIP != "" {
		ip = out.RemoteIP
	}
	_, err = tx.ExecContext(ctx, `UPDATE delivery_attempts SET result=$2,finished_at=$3,connected_at=$4,tls_at=$5,authenticated_at=$6,mail_from_at=$7,rcpt_at=$8,data_started_at=$9,data_completed_at=$10,final_response_at=$11,
 smtp_code=$12,smtp_enhanced_code=$13,smtp_response=$14,error_class=$15,error_message=$16,bytes_sent=$17,remote_ip=$18,total_duration_ms=$19,connection_duration_ms=$20,tls_duration_ms=$21,auth_duration_ms=$22,data_duration_ms=$23 WHERE id=$1`,
		j.AttemptID, out.Status, out.FinishedAt, nullable(out.ConnectedAt), nullable(out.TLSAt), nullable(out.AuthenticatedAt), nullable(out.MailFromAt), nullable(out.RcptAt), nullable(out.DataStartedAt), nullable(out.DataCompletedAt), nullable(out.FinalResponseAt), out.Code, out.Enhanced, out.Response, out.ErrorClass, out.ErrorMessage, out.BytesSent, ip, duration(out.StartedAt, out.FinishedAt), duration(out.StartedAt, out.ConnectedAt), duration(out.ConnectedAt, out.TLSAt), duration(out.TLSAt, out.AuthenticatedAt), duration(out.DataStartedAt, out.DataCompletedAt))
	if err != nil {
		return err
	}
	if len(out.Recipients) != len(j.Recipients) {
		return errors.New("incomplete recipient results")
	}
	expected := map[string]bool{}
	for _, rc := range j.Recipients {
		expected[rc.ID] = true
	}
	for _, rc := range out.Recipients {
		if !expected[rc.ID] {
			return errors.New("unexpected recipient result")
		}
		delete(expected, rc.ID)
		if rc.Status != smtpclient.Accepted && rc.Status != smtpclient.Temporary && rc.Status != smtpclient.Permanent && rc.Status != smtpclient.Unknown {
			return errors.New("invalid recipient result")
		}
		var completed any
		if rc.Status == smtpclient.Accepted || rc.Status == smtpclient.Permanent {
			completed = out.FinishedAt
		}
		if _, err = tx.ExecContext(ctx, `UPDATE recipients SET status=$2,smtp_code=$3,smtp_response=$4,completed_at=$5 WHERE id=$1 AND message_id=$6`, rc.ID, rc.Status, rc.Code, rc.Response, completed, j.ID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE attempt_recipients SET status=$3,smtp_code=$4,smtp_enhanced_code=$5,smtp_response=$6 WHERE attempt_id=$1 AND recipient_id=$2`, j.AttemptID, rc.ID, rc.Status, rc.Code, rc.Enhanced, rc.Response); err != nil {
			return err
		}
	}
	for _, e := range out.Events {
		if err = event(ctx, tx, j, e.Type, e.RecipientID, e.At, map[string]any{"smtp_code": e.Code, "response": e.Response}); err != nil {
			return err
		}
	}
	kind := "ATTEMPT_FINISHED"
	if out.Status == smtpclient.Unknown {
		kind = "DELIVERY_UNKNOWN"
	} else if out.Status == smtpclient.Temporary || out.Status == smtpclient.Permanent {
		kind = "ATTEMPT_FAILED"
	}
	if err = event(ctx, tx, j, kind, "", out.FinishedAt, map[string]any{"status": out.Status, "error_class": out.ErrorClass}); err != nil {
		return err
	}
	var completed any
	if out.Status == smtpclient.Accepted || out.Status == smtpclient.Permanent {
		completed = out.FinishedAt
	}
	_, err = tx.ExecContext(ctx, `UPDATE messages SET status=$2,completed_at=$3,last_error=$4,next_attempt_at=NULL,locked_at=NULL,locked_by=NULL,lease_expires_at=NULL WHERE id=$1`, j.ID, out.Status, completed, out.ErrorClass)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (r Repository) Recover(ctx context.Context) error {
	tx, err := r.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT m.id,m.locked_by,a.id,a.data_armed_at IS NOT NULL FROM messages m JOIN delivery_attempts a ON a.message_id=m.id AND a.result='IN_PROGRESS' WHERE m.status='SENDING' AND m.lease_expires_at<=clock_timestamp() ORDER BY m.lease_expires_at LIMIT 20 FOR UPDATE OF m SKIP LOCKED`)
	if err != nil {
		return err
	}
	type expired struct {
		j     Job
		armed bool
	}
	var jobs []expired
	for rows.Next() {
		var e expired
		if err = rows.Scan(&e.j.ID, &e.j.Token, &e.j.AttemptID, &e.armed); err != nil {
			rows.Close()
			return err
		}
		jobs = append(jobs, e)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, e := range jobs {
		status, attemptStatus, kind := "QUEUED", smtpclient.Temporary, "LEASE_RECOVERED"
		var next any = time.Now().UTC()
		if e.armed {
			status = smtpclient.Unknown
			attemptStatus = smtpclient.Unknown
			kind = "DELIVERY_UNKNOWN"
			next = nil
		}
		if _, err = tx.ExecContext(ctx, `UPDATE delivery_attempts SET result=$2,finished_at=now(),error_class='WORKER_LEASE_EXPIRED' WHERE id=$1`, e.j.AttemptID, attemptStatus); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE attempt_recipients SET status=$2 WHERE attempt_id=$1`, e.j.AttemptID, attemptStatus); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE recipients SET status=$2 WHERE message_id=$1 AND status='SENDING'`, e.j.ID, status); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE messages SET status=$2,last_error='WORKER_LEASE_EXPIRED',next_attempt_at=$3,locked_at=NULL,locked_by=NULL,lease_expires_at=NULL WHERE id=$1`, e.j.ID, status, next); err != nil {
			return err
		}
		if err = event(ctx, tx, e.j, kind, "", time.Now().UTC(), map[string]bool{"data_may_have_started": e.armed}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Keep SQL errors out of application logs: they may contain SMTP metadata.
func ErrorClass(err error) string {
	if errors.Is(err, ErrLeaseLost) {
		return "LEASE_LOST"
	}
	if strings.Contains(fmt.Sprintf("%T", err), "PgError") {
		return "DATABASE_ERROR"
	}
	return "WORKER_ERROR"
}
