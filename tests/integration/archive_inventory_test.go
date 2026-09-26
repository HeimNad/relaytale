package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"relaytale/internal/operations"
	"relaytale/internal/testsmtp"
)

func TestArchiveInventoryReadOnlyAndBounded(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	id := f.seed(t)
	var path string
	if err := f.db.QueryRow(`SELECT eml_path FROM messages WHERE id=$1`, id).Scan(&path); err != nil {
		t.Fatal(err)
	}
	folder := filepath.Dir(path)
	rel, err := filepath.Rel(f.root, folder)
	if err != nil {
		t.Fatal(err)
	}
	date, err := time.Parse("2006/01/02", filepath.ToSlash(rel))
	if err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(folder, uuid.NewString()+".eml")
	if err = os.WriteFile(orphan, []byte("private evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	unusual := filepath.Join(folder, ".incoming-evidence")
	if err = os.WriteFile(unusual, []byte("in progress"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(folder, uuid.NewString()+".eml")
	if err = os.Symlink(orphan, link); err != nil {
		t.Fatal(err)
	}
	var before int
	if err = f.db.QueryRow(`SELECT count(*) FROM events WHERE message_id=$1`, id).Scan(&before); err != nil {
		t.Fatal(err)
	}
	report, err := operations.InventoryArchives(context.Background(), f.db, f.root, date.Format("2006-01-02"), 10)
	if err != nil || !report.Complete || report.Truncated || report.Scanned != 4 {
		t.Fatal(report, err)
	}
	reasons := map[string]string{}
	for _, item := range report.Items {
		reasons[filepath.Base(item.Path)] = item.Reason
	}
	for name, want := range map[string]string{filepath.Base(path): "REFERENCE_PRESENT", filepath.Base(orphan): "NO_VISIBLE_REFERENCE_REVIEW", filepath.Base(unusual): "UNRECOGNIZED_NAME", filepath.Base(link): "NON_REGULAR_ENTRY"} {
		if reasons[name] != want {
			t.Fatal(name, reasons[name], want)
		}
	}
	got, err := os.ReadFile(orphan)
	if err != nil || string(got) != "private evidence" {
		t.Fatal("evidence changed", err)
	}
	assertCount(t, f, before, `SELECT count(*) FROM events WHERE message_id=$1`, id)
	report, err = operations.InventoryArchives(context.Background(), f.db, f.root, date.Format("2006-01-02"), 1)
	if err != nil || report.Complete || !report.Truncated || report.Scanned != 1 {
		t.Fatal(report, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err = operations.InventoryArchives(ctx, f.db, f.root, date.Format("2006-01-02"), 10)
	if err == nil || report.Complete {
		t.Fatal("cancelled scan claimed complete")
	}
	// Even a different archive path must not make a known message ID a candidate.
	mustExec(t, f, `UPDATE messages SET eml_path=$2 WHERE id=$1`, id, "unusual/path.eml")
	report, err = operations.InventoryArchives(context.Background(), f.db, f.root, date.Format("2006-01-02"), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range report.Items {
		if filepath.Base(item.Path) == filepath.Base(path) && item.Reason != "REFERENCE_PRESENT" {
			t.Fatal(item)
		}
	}
}

func TestArchiveInventoryRejectsDateTraversalAndSymlinks(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	for _, day := range []string{"", "../../outside", "2026-02-30"} {
		report, err := operations.InventoryArchives(context.Background(), f.db, f.root, day, 10)
		if err == nil || report.Complete {
			t.Fatal(day, report, err)
		}
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(f.root, "2026")); err != nil {
		t.Fatal(err)
	}
	report, err := operations.InventoryArchives(context.Background(), f.db, f.root, "2026-01-01", 10)
	if err == nil || report.Complete {
		t.Fatal("followed date symlink")
	}
}
