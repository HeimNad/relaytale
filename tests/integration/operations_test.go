package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"relaytale/internal/operations"
	"relaytale/internal/testsmtp"
)

func TestRetentionAndExport(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	id := f.seed(t)
	runOne(t, f.worker)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := f.db.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`UPDATE messages SET completed_at=now()-interval '200 days' WHERE id=$1`, id)
	exec(`UPDATE delivery_attempts SET finished_at=now()-interval '40 days',raw_debug_log='private-debug-marker' WHERE message_id=$1`, id)
	var path string
	if err := f.db.QueryRowContext(ctx, `SELECT eml_path FROM messages WHERE id=$1`, id).Scan(&path); err != nil {
		t.Fatal(err)
	}
	// Old held and queued messages must never be eligible, regardless of age.
	for _, status := range []string{"QUEUED", "TEMP_FAILED", "DELIVERY_UNKNOWN", "PARTIAL_ACCEPTED"} {
		held := f.seed(t)
		exec(`UPDATE messages SET status=$2,completed_at=now()-interval '200 days' WHERE id=$1`, held, status)
	}
	p := operations.Retention{EMLDays: 180, DebugDays: 30, Batch: 100}
	report, err := operations.Cleanup(ctx, f.db, f.root, p)
	if err != nil || len(report.Archives) != 1 || report.Archives[0].ID != id || report.DebugCandidates != 1 {
		t.Fatalf("dry run: %+v %v", report, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("dry run deleted archive")
	}
	var exported bytes.Buffer
	exportedReport, err := operations.Export(ctx, f.db, &exported, operations.Filter{MessageID: id})
	if err != nil {
		t.Fatal(err)
	}
	if exportedReport.Records["message"] != 1 || exportedReport.Records["recipient"] != 2 || exportedReport.Records["attempt"] != 1 {
		t.Fatal(exportedReport)
	}
	for _, secret := range []string{"private-debug-marker", "raw_debug_log", "claim_token", "provider-password", "credentials_encrypted"} {
		if strings.Contains(exported.String(), secret) {
			t.Fatalf("export leaked %s", secret)
		}
	}
	for _, line := range bytes.Split(bytes.TrimSpace(exported.Bytes()), []byte("\n")) {
		if !json.Valid(line) {
			t.Fatal("invalid JSONL")
		}
	}
	p.Apply = true
	report, err = operations.Cleanup(ctx, f.db, f.root, p)
	if err != nil || report.Purged != 1 || report.DebugCleared != 1 {
		t.Fatalf("apply: %+v %v", report, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("archive remains")
	}
	var state string
	if err := f.db.QueryRowContext(ctx, `SELECT archive_state FROM messages WHERE id=$1`, id).Scan(&state); err != nil || state != "PURGED" {
		t.Fatal(state, err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE maintenance_audit SET actor='tampered'`); err == nil {
		t.Fatal("audit not append-only")
	}
	// Resume a crash after file deletion, before completion commit.
	exec(`UPDATE messages SET archive_state='PURGE_PENDING' WHERE id=$1`, id)
	report, err = operations.Cleanup(ctx, f.db, f.root, p)
	if err != nil || report.Purged != 1 {
		t.Fatalf("recovery: %+v %v", report, err)
	}
	report, err = operations.Cleanup(ctx, f.db, f.root, p)
	if err != nil || report.Purged != 0 {
		t.Fatal("cleanup not idempotent", err)
	}
	exported.Reset()
	if _, err := operations.Export(ctx, f.db, &exported, operations.Filter{MessageID: id}); err != nil || !strings.Contains(exported.String(), "ARCHIVE_PURGED") {
		t.Fatal("ledger lost", err)
	}
}

func TestEventsPersistBeforeCompletion(t *testing.T) {
	f := delivery(t, testsmtp.Options{FinalDelay: time.Second}, 1)
	id := f.seed(t)
	done := make(chan error, 1)
	go func() { _, err := f.worker.RunOne(context.Background()); done <- err }()
	select {
	case <-f.fake.Payloads:
	case <-time.After(5 * time.Second):
		t.Fatal("no payload")
	}
	var count int
	if err := f.db.QueryRow(`SELECT count(*) FROM events WHERE message_id=$1 AND attempt_sequence IS NOT NULL`, id).Scan(&count); err != nil || count < 3 {
		t.Fatal("events not streamed", count, err)
	}
	if f.status(t, id) != "SENDING" {
		t.Fatal("expected in-progress attempt")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(`SELECT count(*)-count(DISTINCT attempt_sequence) FROM events WHERE message_id=$1 AND attempt_sequence IS NOT NULL`, id).Scan(&count); err != nil || count != 0 {
		t.Fatal("duplicate events", err)
	}
}

func TestCleanupRefusesOutsideRoot(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	id := f.seed(t)
	runOne(t, f.worker)
	var path string
	if err := f.db.QueryRow(`SELECT eml_path FROM messages WHERE id=$1`, id).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE messages SET completed_at=now()-interval '200 days' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Exec(`UPDATE messages SET archive_state='AVAILABLE',completed_at=now() WHERE id=$1`, id) })
	report, err := operations.Cleanup(context.Background(), f.db, t.TempDir(), operations.Retention{EMLDays: 180, Batch: 100, Apply: true})
	if err == nil || len(report.Failures) != 1 {
		t.Fatal("outside-root deletion not rejected", report, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("file outside root removed")
	}
	report, err = operations.Cleanup(context.Background(), f.db, f.root, operations.Retention{EMLDays: 180, Batch: 100, Apply: true})
	if err != nil || report.Purged != 1 {
		t.Fatal("pending retry failed", report, err)
	}
}
