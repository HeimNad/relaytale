package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"relaytale/internal/api"
	"relaytale/internal/feedback"
	"relaytale/migrations"
)

// Capacity boundary tests own a schema; synthetic near-limit counters must never
// alter the shared integration database or require a gigabyte of test payloads.
func capacityDB(t *testing.T) *sql.DB {
	t.Helper()
	base := testDB(t)
	schema := "capacity_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := base.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { base.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	t.Setenv("TEST_DATABASE_URL", u.String())
	return testDB(t)
}
func capacityExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}
func capacityImport(s feedback.Service, source string) (feedback.ImportResult, error) {
	return s.Import(context.Background(), feedback.ImportRequest{Source: source, Actor: "test", Reason: "capacity regression"}, strings.NewReader(feedbackRaw("one@example.test")))
}
func TestFeedbackCapacityConcurrentAndDuplicate(t *testing.T) {
	db := capacityDB(t)
	s := feedback.Service{DB: db}
	capacityExec(t, db, `UPDATE feedback_capacity SET records=99999`)
	start := make(chan struct{})
	results := make(chan feedback.ImportResult, 8)
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; r, e := capacityImport(s, uuid.NewString()); results <- r; errs <- e }()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	accepted, blocked := 0, 0
	for e := range errs {
		if e == nil {
			accepted++
		} else if errors.Is(e, feedback.ErrCapacity) {
			blocked++
		} else {
			t.Fatal(e)
		}
	}
	if accepted != 1 || blocked != 7 {
		t.Fatal(accepted, blocked)
	}
	var saved feedback.ImportResult
	for r := range results {
		if r.Entry.ID != "" {
			saved = r
		}
	}
	before, e := s.Capacity(context.Background())
	if e != nil || !before.AtLimit || before.Records != 100000 {
		t.Fatal(before, e)
	}
	dup, e := capacityImport(s, saved.Entry.Source)
	if e != nil || !dup.Duplicate || dup.Entry.ID != saved.Entry.ID {
		t.Fatal(dup, e)
	}
	after, e := s.Capacity(context.Background())
	if e != nil || before != after {
		t.Fatal(before, after, e)
	}
	var n int
	if e = db.QueryRow(`SELECT count(*) FROM maintenance_audit WHERE action='FEEDBACK_QUARANTINED'`).Scan(&n); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	raw, _ := json.Marshal([]api.Principal{{ID: "reader", Role: "viewer", Token: strings.Repeat("v", 32)}})
	h, e := (api.Management{DB: db}).Handler(string(raw))
	if e != nil {
		t.Fatal(e)
	}
	w := adminRequest(h, "GET", "/feedback-capacity", "v", nil)
	requireStatus(t, w, 200)
	var observed feedback.EvidenceCapacity
	if json.Unmarshal(w.Body.Bytes(), &observed) != nil || observed != after {
		t.Fatal(w.Body)
	}
	requireStatus(t, adminRequest(h, "GET", "/feedback-capacity", "x", nil), 401)
	requireStatus(t, adminRequest(h, "GET", "/feedback-capacity?reset=true", "v", nil), 400)
}
func TestFeedbackCapacityBytesAndAuditRollback(t *testing.T) {
	db := capacityDB(t)
	s := feedback.Service{DB: db}
	_, e := capacityImport(s, "first")
	if e != nil {
		t.Fatal(e)
	}
	initial, e := s.Capacity(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	capacityExec(t, db, `UPDATE feedback_capacity SET stored_bytes=$1`, feedback.MaxEvidenceBytes-initial.StoredBytes)
	capacityExec(t, db, `CREATE FUNCTION fail_capacity_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test audit failure'; END $$`)
	capacityExec(t, db, `CREATE TRIGGER fail_capacity_audit BEFORE INSERT ON maintenance_audit FOR EACH ROW EXECUTE FUNCTION fail_capacity_audit()`)
	if _, e = capacityImport(s, "second"); e == nil {
		t.Fatal("audit failure accepted")
	}
	got, e := s.Capacity(context.Background())
	if e != nil || got.Records != 1 || got.StoredBytes != feedback.MaxEvidenceBytes-initial.StoredBytes {
		t.Fatal(got, e)
	}
	capacityExec(t, db, `DROP TRIGGER fail_capacity_audit ON maintenance_audit`)
	if _, e = capacityImport(s, "second"); e != nil {
		t.Fatal(e)
	}
	got, e = s.Capacity(context.Background())
	if e != nil || got.StoredBytes != feedback.MaxEvidenceBytes || !got.AtLimit {
		t.Fatal(got, e)
	}
	if _, e = capacityImport(s, "third"); !errors.Is(e, feedback.ErrCapacity) {
		t.Fatal(e)
	}
}
func TestFeedbackCapacityIMAPCheckpointRollback(t *testing.T) {
	db := capacityDB(t)
	s := feedback.Service{DB: db}
	capacityExec(t, db, `UPDATE feedback_capacity SET stored_bytes=$1`, feedback.MaxEvidenceBytes)
	o := collectionOptions()
	raw := []byte(feedbackRaw("one@example.test"))
	box := &testMailbox{epoch: 7, next: 2, meta: []feedback.MailMetadata{{UID: 1, Size: int64(len(raw))}}, raw: map[uint32][]byte{1: raw}}
	out, e := s.Collect(context.Background(), o, box.open)
	if !errors.Is(e, feedback.ErrCapacity) || out.Imported != 0 || out.LastUID != 0 {
		t.Fatal(out, e)
	}
	var uid, records, bytes int64
	e = db.QueryRow(`SELECT last_uid,records,stored_bytes FROM feedback_imap_sources WHERE name=$1`, o.Source).Scan(&uid, &records, &bytes)
	if e != nil || uid != 0 || records != 0 || bytes != 0 {
		t.Fatal(uid, records, bytes, e)
	}
	var n int
	if e = db.QueryRow(`SELECT count(*) FROM feedback_imap_receipts`).Scan(&n); e != nil || n != 0 {
		t.Fatal(n, e)
	}
	// Restoring headroom in this synthetic fixture must fetch the same UID again.
	capacityExec(t, db, `UPDATE feedback_capacity SET stored_bytes=0`)
	out, e = s.Collect(context.Background(), o, box.open)
	if e != nil || out.Imported != 1 || out.LastUID != 1 {
		t.Fatal(out, e)
	}
}
func TestFeedbackCapacityMigrationBackfill(t *testing.T) {
	db := capacityDB(t)
	s := feedback.Service{DB: db}
	if _, e := capacityImport(s, "historical"); e != nil {
		t.Fatal(e)
	}
	before, e := s.Capacity(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	capacityExec(t, db, `DROP TRIGGER feedback_capacity_charge ON feedback_reports; DROP FUNCTION charge_feedback_capacity(); DROP TABLE feedback_capacity`)
	// Model an already over-limit pre-upgrade archive without large payloads.
	capacityExec(t, db, `INSERT INTO feedback_reports(id,source,external_id,sha256,raw,parsed,parse_state,actor,reason)
 SELECT gen_random_uuid(),'legacy',g::text,repeat('0',64),'x'::bytea,'{}'::jsonb,'UNSUPPORTED','legacy','upgrade fixture' FROM generate_series(1,100000) g`)
	contents, e := migrations.Files.ReadFile("00011_feedback_capacity.sql")
	if e != nil {
		t.Fatal(e)
	}
	up := strings.Split(string(contents), "-- +goose Down")[0]
	tx, e := db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if _, e = tx.Exec(up); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	after, e := s.Capacity(context.Background())
	if e != nil || after.Records != before.Records+100000 || after.StoredBytes != before.StoredBytes+300000 || !after.AtLimit {
		t.Fatal(before, after, e)
	}
	if _, e = capacityImport(s, "post-upgrade"); !errors.Is(e, feedback.ErrCapacity) {
		t.Fatal(e)
	}
	duplicate, e := capacityImport(s, "historical")
	if e != nil || !duplicate.Duplicate {
		t.Fatal(duplicate, e)
	}
}
