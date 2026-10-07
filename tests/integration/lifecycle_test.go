package integration

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"relaytale/internal/operations"
	"relaytale/internal/testsmtp"
)

func TestLifecyclePreflightProtectionAndNoMutation(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	id := f.seed(t)
	runOne(t, f.worker)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := f.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE messages SET completed_at=now()-interval '200 days' WHERE id=$1", id)
	var path, before string
	if err := f.db.QueryRow("SELECT eml_path,to_jsonb(messages)::text FROM messages WHERE id=$1", id).Scan(&path, &before); err != nil {
		t.Fatal(err)
	}
	var auditBefore int
	if err := f.db.QueryRow("SELECT count(*) FROM maintenance_audit").Scan(&auditBefore); err != nil {
		t.Fatal(err)
	}
	p := operations.LifecycleOptions{EMLDays: 180, Limit: 10}
	check := func(reason string) operations.LifecycleReport {
		t.Helper()
		r, err := operations.LifecyclePreflight(ctx, f.db, p)
		if err != nil || len(r.Items) != 1 {
			t.Fatalf("report %+v %v", r, err)
		}
		item := r.Items[0]
		if !r.ReadOnly || r.DeletionAuthorized || r.MetadataDisposition != "RETAIN_ALL" {
			t.Fatal("unsafe report", r)
		}
		if reason == "" {
			if item.EMLDisposition != "REVIEW_CANDIDATE" {
				t.Fatal(item)
			}
		} else {
			found := false
			for _, v := range item.Reasons {
				if v == reason {
					found = true
				}
			}
			if !found || item.EMLDisposition != "RETAIN" {
				t.Fatalf("missing %s: %+v", reason, item)
			}
		}
		data, _ := json.Marshal(r)
		for _, private := range []string{"one@example.test", f.root, "sender@", "raw_debug_log"} {
			if strings.Contains(string(data), private) {
				t.Fatal("private data in report", private)
			}
		}
		return r
	}
	r := check("")
	p.AsOf = r.AsOf
	var after string
	var auditAfter int
	if err := f.db.QueryRow("SELECT to_jsonb(messages)::text FROM messages WHERE id=$1", id).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow("SELECT count(*) FROM maintenance_audit").Scan(&auditAfter); err != nil {
		t.Fatal(err)
	}
	if before != after || auditBefore != auditAfter {
		t.Fatal("preflight mutated state")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("preflight touched archive", err)
	}
	p.EMLDays = 0
	check("RETENTION_DISABLED")
	p.EMLDays = 180
	exec("UPDATE messages SET status='DELIVERY_UNKNOWN' WHERE id=$1", id)
	check("NON_TERMINAL_MESSAGE")
	exec("UPDATE messages SET status='SMTP_ACCEPTED' WHERE id=$1", id)
	exec("UPDATE recipients SET status='DELIVERY_UNKNOWN' WHERE message_id=$1", id)
	check("RECIPIENT_UNRESOLVED")
	exec("UPDATE recipients SET status='SMTP_ACCEPTED' WHERE message_id=$1", id)
	exec("UPDATE messages SET locked_by='synthetic',locked_at=now(),lease_expires_at=now()+interval '1 minute' WHERE id=$1", id)
	check("MESSAGE_LEASE")
	exec("UPDATE messages SET locked_by=NULL,locked_at=NULL,lease_expires_at=NULL WHERE id=$1", id)
	exec("UPDATE delivery_attempts SET result='IN_PROGRESS' WHERE message_id=$1", id)
	check("ATTEMPT_IN_PROGRESS")
	exec("UPDATE delivery_attempts SET result='SMTP_ACCEPTED' WHERE message_id=$1", id)
	exec("UPDATE messages SET completed_at=NULL WHERE id=$1", id)
	check("COMPLETION_UNRECORDED")
	exec("UPDATE messages SET completed_at=now() WHERE id=$1", id)
	check("RETENTION_WINDOW")
	exec("UPDATE messages SET completed_at=now()-interval '200 days' WHERE id=$1", id)
	key := testAPIKey(t, f)
	exec("INSERT INTO api_submission_ledger(api_key_id,key_sha256,request_sha256,message_id) VALUES($1,$2,$2,$3)", key, strings.Repeat("a", 64), id)
	check("IDEMPOTENCY_REFERENCE")
	endpoint := uuid.NewString()
	exec("INSERT INTO webhook_endpoints(id,api_key_id,url,config_hash) VALUES($1,$2,'https://example.test/hook',$3)", endpoint, key, strings.Repeat("b", 64))
	exec("INSERT INTO webhook_outbox(endpoint_id,event_id,message_id,payload) SELECT $1,id,message_id,'{}' FROM events WHERE message_id=$2 LIMIT 1", endpoint, id)
	check("WEBHOOK_REQUIRES_REVIEW")
	exec("UPDATE webhook_outbox SET state='DEAD' WHERE message_id=$1", id)
	check("WEBHOOK_REQUIRES_REVIEW")
	feedback := uuid.NewString()
	exec("INSERT INTO feedback_reports(id,source,external_id,sha256,raw,parsed,parse_state,actor,reason) VALUES($1,'test','one',$2,$3,'{}','UNSUPPORTED','test','synthetic')", feedback, strings.Repeat("c", 64), []byte("synthetic"))
	if !check("UNSCOPED_FEEDBACK_REVIEW").UnreviewedFeedback {
		t.Fatal("missing global hold")
	}
	exec("INSERT INTO feedback_reviews(feedback_id,action,actor,reason) VALUES($1,'DISMISS','test','manual review')", feedback)
	if check("IDEMPOTENCY_REFERENCE").UnreviewedFeedback {
		t.Fatal("reviewed evidence still unreviewed")
	}
	if _, err := f.db.Exec("UPDATE messages SET archive_state='PURGED' WHERE id=$1", id); err == nil {
		t.Fatal("protected archive allowed retirement")
	}
}

func TestLifecyclePaginationAndInvalidInput(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	ids := map[string]bool{f.seed(t): true, f.seed(t): true, f.seed(t): true}
	ctx := context.Background()
	p := operations.LifecycleOptions{Limit: 1}
	for i := 0; i < 3; i++ {
		r, err := operations.LifecyclePreflight(ctx, f.db, p)
		if err != nil || len(r.Items) != 1 || !ids[r.Items[0].MessageID] || r.HasMore != (i < 2) {
			t.Fatalf("page %+v %v", r, err)
		}
		delete(ids, r.Items[0].MessageID)
		p.After = r.NextAfter
		p.AsOf = r.AsOf
	}
	for _, bad := range []operations.LifecycleOptions{{Limit: 0}, {Limit: 1001}, {Limit: 1, After: "bad"}, {Limit: 1, EMLDays: -1}, {Limit: 1, EMLDays: 36501}, {Limit: 1, AsOf: time.Now().Add(time.Hour)}} {
		r, err := operations.LifecyclePreflight(ctx, f.db, bad)
		if err == nil || len(r.Items) != 0 || r.NextAfter != "" {
			t.Fatalf("invalid input published report %+v %v", r, err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if r, err := operations.LifecyclePreflight(cancelled, f.db, p); err == nil || r.NextAfter != "" {
		t.Fatal("cancel published cursor", r, err)
	}
}
