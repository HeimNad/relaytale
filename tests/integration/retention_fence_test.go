package integration

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"relaytale/internal/operations"
	"relaytale/internal/testsmtp"
)

// Simulate a pre-v24 purged backup/sandbox fixture. Only the synthetic test
// administrator bypasses triggers, in this transaction; runtime roles cannot.
func legacyPurgedFixture(t *testing.T, f *deliveryFixture, id string) {
	t.Helper()
	tx, err := f.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("SET LOCAL session_replication_role=replica"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("UPDATE messages SET archive_state='PURGED',eml_purged_at=now() WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatedCleanupDependencyFences(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	id := f.seed(t)
	runOne(t, f.worker)
	mustExec(t, f, "UPDATE messages SET completed_at=now()-interval '200 days' WHERE id=$1", id)
	p := operations.Retention{EMLDays: 180, Batch: 10, Apply: true}
	ctx := context.Background()
	// Intent commits before any later feedback. A failed removal leaves a durable
	// intent; later feedback cannot pretend it can recover an already removed file.
	if r, err := operations.Cleanup(ctx, f.db, t.TempDir(), p); err == nil || len(r.Failures) != 1 {
		t.Fatal(r, err)
	}
	for _, q := range []string{
		"UPDATE messages SET archive_state='AVAILABLE' WHERE id=$1",
		"UPDATE messages SET status='QUEUED' WHERE id=$1",
		"UPDATE recipients SET status='DELIVERY_UNKNOWN' WHERE message_id=$1",
		"UPDATE delivery_attempts SET result='IN_PROGRESS' WHERE message_id=$1",
	} {
		if _, err := f.db.Exec(q, id); err == nil {
			t.Fatal("retired archive reactivated", q)
		}
	}
	key := testAPIKey(t, f)
	if _, err := f.db.Exec("INSERT INTO api_submission_ledger(api_key_id,key_sha256,request_sha256,message_id) VALUES($1,$2,$2,$3)", key, strings.Repeat("a", 64), id); err == nil {
		t.Fatal("late ledger admitted")
	}
	endpoint := uuid.NewString()
	mustExec(t, f, "INSERT INTO webhook_endpoints(id,api_key_id,url,config_hash) VALUES($1,$2,'https://example.test/hook',$3)", endpoint, key, strings.Repeat("f", 64))
	if _, err := f.db.Exec("INSERT INTO webhook_outbox(endpoint_id,event_id,message_id,payload) SELECT $1,id,message_id,'{}' FROM events WHERE message_id=$2 LIMIT 1", endpoint, id); err == nil {
		t.Fatal("late webhook dependency admitted")
	}
	feedback := uuid.NewString()
	mustExec(t, f, "INSERT INTO feedback_reports(id,source,external_id,sha256,raw,parsed,parse_state,actor,reason) VALUES($1,'test','late',$2,$3,'{}','UNSUPPORTED','test','late evidence')", feedback, strings.Repeat("b", 64), []byte("synthetic"))
	if r, err := operations.Cleanup(ctx, f.db, f.root, p); err != nil || r.Purged != 1 {
		t.Fatal("committed intent did not resume", r, err)
	}
	fresh := f.seed(t)
	runOne(t, f.worker)
	mustExec(t, f, "UPDATE messages SET completed_at=now()-interval '200 days' WHERE id=$1", fresh)
	if r, err := operations.Cleanup(ctx, f.db, f.root, p); err != nil || r.Purged != 0 {
		t.Fatal("unreviewed feedback failed to hold new intent", r, err)
	}
	mustExec(t, f, "INSERT INTO feedback_reviews(feedback_id,action,actor,reason) VALUES($1,'DISMISS','test','reviewed')", feedback)
	if r, err := operations.Cleanup(ctx, f.db, f.root, p); err != nil || r.Purged != 1 {
		t.Fatal("reviewed feedback did not release hold", r, err)
	}
}

// Wait for an actual PostgreSQL lock barrier, not a guessed sleep interval.
func awaitRetentionWait(t *testing.T, db *sql.DB, pattern string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE $1)", pattern).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("retention barrier timeout")
		case <-ticker.C:
		}
	}
}

func TestCleanupWaitRechecksCommittedFeedback(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	id := f.seed(t)
	runOne(t, f.worker)
	mustExec(t, f, "UPDATE messages SET completed_at=now()-interval '200 days' WHERE id=$1", id)
	var path string
	if err := f.db.QueryRow("SELECT eml_path FROM messages WHERE id=$1", id).Scan(&path); err != nil {
		t.Fatal(err)
	}
	tx, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO feedback_reports(id,source,external_id,sha256,raw,parsed,parse_state,actor,reason) VALUES($1,'test','barrier',$2,$3,'{}','UNSUPPORTED','test','uncommitted evidence')", uuid.NewString(), strings.Repeat("c", 64), []byte("synthetic")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := operations.Cleanup(ctx, f.db, f.root, operations.Retention{EMLDays: 180, Batch: 10, Apply: true})
		done <- err
	}()
	awaitRetentionWait(t, f.db, "%pg_advisory_xact_lock(78320420)%")
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err == nil {
		t.Fatal("stale cleanup candidate not rejected")
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("protected EML removed", err)
	}
	assertCount(t, f, 1, "SELECT count(*) FROM messages WHERE id=$1 AND archive_state='AVAILABLE'", id)
	assertCount(t, f, 0, "SELECT count(*) FROM maintenance_audit WHERE action='ARCHIVE_PURGE_PENDING' AND data->>'message_id'=$1", id)
}

func TestCleanupWaitRechecksCommittedMessageDependencies(t *testing.T) {
	for _, mode := range []string{"ledger", "webhook", "recipient", "attempt"} {
		t.Run(mode, func(t *testing.T) {
			_, url := recoveryDB(t)
			t.Setenv("TEST_DATABASE_URL", url)
			f := delivery(t, testsmtp.Options{}, 1)
			id := f.seed(t)
			runOne(t, f.worker)
			mustExec(t, f, "UPDATE messages SET completed_at=now()-interval '200 days' WHERE id=$1", id)
			key := testAPIKey(t, f)
			endpoint := uuid.NewString()
			mustExec(t, f, "INSERT INTO webhook_endpoints(id,api_key_id,url,config_hash) VALUES($1,$2,'https://example.test/hook',$3)", endpoint, key, strings.Repeat("e", 64))
			mustExec(t, f, "INSERT INTO webhook_outbox(endpoint_id,event_id,message_id,payload,state) SELECT $1,id,message_id,'{}','DELIVERED' FROM events WHERE message_id=$2 LIMIT 1", endpoint, id)
			mutate := func(tx *sql.Tx) error {
				var err error
				switch mode {
				case "ledger":
					_, err = tx.Exec("INSERT INTO api_submission_ledger(api_key_id,key_sha256,request_sha256,message_id) VALUES($1,$2,$2,$3)", key, strings.Repeat("d", 64), id)
				case "webhook":
					_, err = tx.Exec("UPDATE webhook_outbox SET state='PENDING' WHERE message_id=$1", id)
				case "recipient":
					_, err = tx.Exec("UPDATE recipients SET status='DELIVERY_UNKNOWN' WHERE message_id=$1", id)
				case "attempt":
					_, err = tx.Exec("UPDATE delivery_attempts SET result='IN_PROGRESS' WHERE message_id=$1", id)
				}
				return err
			}
			tx, err := f.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err = mutate(tx); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := operations.Cleanup(ctx, f.db, f.root, operations.Retention{EMLDays: 180, Batch: 10, Apply: true})
				done <- err
			}()
			awaitRetentionWait(t, f.db, "%SELECT id FROM messages WHERE id=%FOR UPDATE%")
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if err = <-done; err == nil {
				t.Fatal("post-wait dependency ignored")
			}
			assertCount(t, f, 1, "SELECT count(*) FROM messages WHERE id=$1 AND archive_state='AVAILABLE'", id)
			// Restore only mutable delivery states, preserving ledger history. A direct
			// attempted intent must independently be rejected by the schema gate.
			if mode == "recipient" {
				mustExec(t, f, "UPDATE recipients SET status='SMTP_ACCEPTED' WHERE message_id=$1", id)
			}
			if mode == "attempt" {
				mustExec(t, f, "UPDATE delivery_attempts SET result='SMTP_ACCEPTED' WHERE message_id=$1", id)
			}
			if mode == "ledger" || mode == "webhook" {
				if _, err = f.db.Exec("UPDATE messages SET archive_state='PURGE_PENDING' WHERE id=$1", id); err == nil {
					t.Fatal("schema allowed protected intent")
				}
			} else {
				mustExec(t, f, "UPDATE messages SET archive_state='PURGE_PENDING' WHERE id=$1", id)
			}
			if mode == "webhook" {
				mustExec(t, f, "UPDATE webhook_outbox SET state='DELIVERED' WHERE message_id=$1", id)
			}
			// Reverse ordering: once intent wins, a later protected write fails.
			if mode == "ledger" || mode == "webhook" {
				return
			}
			late, err := f.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer late.Rollback()
			if err = mutate(late); err == nil {
				t.Fatal("late dependency admitted")
			}
		})
	}
}

func TestDirectRetentionIntentWaitsForDependency(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	id := f.seed(t)
	runOne(t, f.worker)
	key := testAPIKey(t, f)
	tx, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO api_submission_ledger(api_key_id,key_sha256,request_sha256,message_id) VALUES($1,$2,$2,$3)", key, strings.Repeat("9", 64), id); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := f.db.ExecContext(ctx, "UPDATE messages SET archive_state='PURGE_PENDING' WHERE id=$1", id)
		done <- err
	}()
	awaitRetentionWait(t, f.db, "%UPDATE messages SET archive_state='PURGE_PENDING'%")
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err == nil {
		t.Fatal("direct intent ignored concurrent ledger")
	}
	assertCount(t, f, 1, "SELECT count(*) FROM messages WHERE id=$1 AND archive_state='AVAILABLE'", id)
}
