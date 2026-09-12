package message

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"relaytale/internal/suppression"
)

type Postgres struct{ DB *sql.DB }

func (p Postgres) Enqueue(ctx context.Context, s Submission) error {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Never acknowledge an asynchronous PostgreSQL commit, even if the server
	// default was relaxed by an operator.
	if _, err = tx.ExecContext(ctx, `SET LOCAL synchronous_commit = on`); err != nil {
		return err
	}
	queued := time.Now().UTC()
	_, err = tx.ExecContext(ctx, `INSERT INTO messages
 (id,message_id,source_type,smtp_account_id,envelope_from,header_from,subject,created_at,queued_at,status,eml_path,eml_size,eml_sha256,next_attempt_at)
 VALUES($1,$2,'smtp',$3,$4,$5,$6,$7,$8,'QUEUED',$9,$10,$11,$8)`, s.ID, s.MessageID, s.Envelope.Account.ID, s.Envelope.From, s.HeaderFrom, s.Subject, s.ReceivedAt, queued, s.Archive.Path, s.Archive.Size, s.Archive.SHA256)
	if err != nil {
		return err
	}
	for _, address := range s.Envelope.Recipients {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO recipients(id,message_id,address,recipient_type,status,created_at) VALUES($1,$2,$3,'envelope','QUEUED',$4)`, id.String(), s.ID, address, s.ReceivedAt); err != nil {
			return err
		}
	}
	if err = suppression.Lock(ctx, tx, false); err != nil {
		return err
	}
	if _, err = suppression.Apply(ctx, tx, s.ID, "", false); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE messages SET status='SUPPRESSED',completed_at=now(),next_attempt_at=NULL WHERE id=$1 AND NOT EXISTS(SELECT 1 FROM recipients WHERE message_id=$1 AND status<>'SUPPRESSED')`, s.ID); err != nil {
		return err
	}
	queueEvent := "MESSAGE_QUEUED"
	var messageStatus string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM messages WHERE id=$1`, s.ID).Scan(&messageStatus); err != nil {
		return err
	}
	if messageStatus == "SUPPRESSED" {
		queueEvent = "MESSAGE_SUPPRESSED"
	}
	for _, event := range []struct {
		kind string
		at   time.Time
	}{{"MESSAGE_RECEIVED", s.ReceivedAt}, {"MESSAGE_ARCHIVED", s.ArchivedAt}, {queueEvent, queued}} {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		data, err := json.Marshal(map[string]any{"source": "smtp"})
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO events(id,message_id,event_type,event_time,source,data) VALUES($1,$2,$3,$4,'smtp',$5)`, id.String(), s.ID, event.kind, event.at, string(data)); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `SELECT pg_notify('queue_new_message',$1) FROM messages WHERE id=$1::uuid AND status='QUEUED'`, s.ID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
