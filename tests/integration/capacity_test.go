package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"relaytale/internal/operations"
	"relaytale/internal/testsmtp"
)

func TestCapacityReadOnlyMissingMismatchAndPaging(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	ids := []string{f.seed(t), f.seed(t), f.seed(t)}
	var paths [3]string
	for i, id := range ids {
		if err := f.db.QueryRow("SELECT eml_path FROM messages WHERE id=$1", id).Scan(&paths[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(paths[0]); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths[1], []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	var before int
	f.db.QueryRow("SELECT count(*) FROM maintenance_audit").Scan(&before)
	reasons := map[string]string{}
	after := ""
	for page := 0; page < 3; page++ {
		report, err := operations.Capacity(context.Background(), f.db, f.root, after, 1)
		if err != nil {
			t.Fatal(err)
		}
		if report.DeletionAuthorized || len(report.Items) != 1 || len(report.Relations) != 8 || report.HasMore != (page < 2) || report.Consistency != "LIVE_OBSERVATIONS_NOT_ATOMIC" {
			t.Fatal(report)
		}
		item := report.Items[0]
		reasons[item.MessageID] = item.Observation
		after = report.NextAfter
		for _, relation := range report.Relations {
			if relation.TotalBytes != relation.TableBytes+relation.IndexBytes || relation.TotalBytes < 0 {
				t.Fatal(relation)
			}
		}
	}
	if reasons[ids[0]] != "MISSING" || reasons[ids[1]] != "SIZE_MISMATCH_REVIEW" || reasons[ids[2]] != "REGULAR_SIZE_MATCH" {
		t.Fatal(reasons)
	}
	assertCount(t, f, before, "SELECT count(*) FROM maintenance_audit")
	assertCount(t, f, 0, "SELECT count(*) FROM delivery_attempts")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err := operations.Capacity(ctx, f.db, f.root, "", 10)
	if err == nil || report.NextAfter != "" || report.Items != nil {
		t.Fatal("partial cancellation report", report, err)
	}
	for _, after := range []string{"not-a-uuid", "../x"} {
		if _, err := operations.Capacity(context.Background(), f.db, f.root, after, 1); err == nil {
			t.Fatal("invalid cursor accepted")
		}
	}
	// Orphan inventory observes regular-file lengths, but never follows symlinks.
	folder := filepath.Dir(paths[2])
	orphan := filepath.Join(folder, "unknown-file")
	if err := os.WriteFile(orphan, []byte("orphan"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(orphan, filepath.Join(folder, "link")); err != nil {
		t.Fatal(err)
	}
	day := filepath.Base(filepath.Dir(filepath.Dir(folder))) + "-" + filepath.Base(filepath.Dir(folder)) + "-" + filepath.Base(folder)
	inventory, err := operations.InventoryArchivesPage(context.Background(), f.db, f.root, day, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range inventory.Items {
		if filepath.Base(item.Path) == "unknown-file" {
			found = true
			if item.ObservedBytes == nil || *item.ObservedBytes != 6 || item.Reason != "UNRECOGNIZED_NAME" {
				t.Fatal(item)
			}
		}
		if filepath.Base(item.Path) == "link" && item.ObservedBytes != nil {
			t.Fatal("symlink measured")
		}
	}
	if !found {
		t.Fatal("orphan absent")
	}
}

func TestCapacityInventoryDetectsConcurrentChange(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	id := f.seed(t)
	var path string
	if err := f.db.QueryRow("SELECT eml_path FROM messages WHERE id=$1", id).Scan(&path); err != nil {
		t.Fatal(err)
	}
	folder := filepath.Dir(path)
	day := filepath.Base(filepath.Dir(filepath.Dir(folder))) + "-" + filepath.Base(filepath.Dir(folder)) + "-" + filepath.Base(folder)
	lock, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err = lock.Exec("LOCK TABLE messages IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type result struct {
		report operations.ArchiveInventoryReport
		err    error
	}
	done := make(chan result, 1)
	go func() {
		report, e := operations.InventoryArchivesPage(ctx, f.db, f.root, day, "", 100)
		done <- result{report, e}
	}()
	// Table lock deterministically pauses SQL reference lookup between file stats.
	for {
		var waiting int
		if err = f.db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT EXISTS(SELECT 1 FROM messages WHERE id=%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("inventory did not reach reference lookup")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err = os.WriteFile(path, []byte("changed during database wait"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = lock.Commit(); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || len(got.report.Items) != 1 || got.report.Items[0].Reason != "CHANGED_DURING_OBSERVATION" || got.report.Items[0].ObservedBytes != nil {
		t.Fatal(got)
	}
}
