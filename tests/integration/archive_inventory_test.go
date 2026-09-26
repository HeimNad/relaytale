package integration

import (
	"context"
	"encoding/base64"
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
	mustExec(t, f, `UPDATE messages SET eml_path=$2 WHERE id=$1`, id, "unusual/"+id+".eml")
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

func TestArchiveInventoryPagesAndCursorValidation(t *testing.T) {
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
	day, err := time.Parse("2006/01/02", filepath.ToSlash(rel))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a\\b", string([]byte{0xff, 0xfe})} {
		if err := os.WriteFile(filepath.Join(folder, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 17; i++ {
		if err := os.WriteFile(filepath.Join(folder, uuid.NewString()+".eml"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var after, lastName string
	seen := map[string]bool{}
	for page := 0; page < 10; page++ {
		report, err := operations.InventoryArchivesPage(context.Background(), f.db, f.root, day.Format("2006-01-02"), after, 3)
		if err != nil || report.Scanned > 3 {
			t.Fatal(report, err)
		}
		for _, item := range report.Items {
			name := filepath.Base(item.Path)
			if seen[name] || name <= lastName {
				t.Fatal("repeated or unordered name", name)
			}
			seen[name] = true
			lastName = name
		}
		if report.NextAfter == after || report.NextAfter == "" {
			t.Fatal("cursor did not advance", report)
		}
		after = report.NextAfter
		if report.Complete {
			break
		}
		if !report.Truncated {
			t.Fatal("missing continuation indicator")
		}
	}
	if len(seen) != 20 {
		t.Fatal("missing entries", len(seen))
	}
	report, err := operations.InventoryArchivesPage(context.Background(), f.db, f.root, day.Format("2006-01-02"), after, 3)
	if err != nil || !report.Complete || report.Scanned != 0 {
		t.Fatal(report, err)
	}
	for _, cursor := range []string{"!invalid", base64.RawURLEncoding.EncodeToString([]byte("../file")), base64.RawURLEncoding.EncodeToString([]byte(".")), base64.RawURLEncoding.EncodeToString([]byte("..")), base64.RawURLEncoding.EncodeToString([]byte("a\x00b"))} {
		report, err := operations.InventoryArchivesPage(context.Background(), f.db, f.root, day.Format("2006-01-02"), cursor, 3)
		if err == nil || report.Complete || report.NextAfter != "" {
			t.Fatal("invalid cursor accepted", cursor)
		}
	}
}
