package integration

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"relaytale/internal/operations"
	"relaytale/internal/testsmtp"
)

func TestPreflightPendingArchivesIsReadOnlyAndPaged(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	var after string
	if err := f.db.QueryRow(`SELECT coalesce(max(id::text),'00000000-0000-0000-0000-000000000000') FROM messages`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	good := f.seed(t)
	bad := f.seed(t)
	missing := f.seed(t)
	corrupt := f.seed(t)
	outside := f.seed(t)
	pathOf := func(id string) string {
		var path string
		if err := f.db.QueryRow(`SELECT eml_path FROM messages WHERE id=$1`, id).Scan(&path); err != nil {
			t.Fatal(err)
		}
		return path
	}
	path := pathOf(bad)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	before = append(before, []byte("legacy\rcarriage\r\n")...)
	if err = os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	mustExec(t, f, `UPDATE messages SET eml_size=$2,eml_sha256=$3 WHERE id=$1`, bad, len(before), fmt.Sprintf("%x", sha256.Sum256(before)))
	if err = os.Remove(pathOf(missing)); err != nil {
		t.Fatal(err)
	}
	mustExec(t, f, `UPDATE messages SET eml_sha256=$2 WHERE id=$1`, corrupt, strings.Repeat("0", 64))
	other := filepath.Join(t.TempDir(), "outside.eml")
	raw, err := os.ReadFile(pathOf(outside))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(other, raw, 0600); err != nil {
		t.Fatal(err)
	}
	mustExec(t, f, `UPDATE messages SET eml_path=$2 WHERE id=$1`, outside, other)
	var events int
	if err = f.db.QueryRow(`SELECT count(*) FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	issues := map[string]string{}
	scanned, valid := 0, 0
	for {
		report, err := operations.Preflight(context.Background(), f.db, f.root, after, 2, 25<<20)
		if err != nil {
			t.Fatal(err)
		}
		if report.Scanned > 2 {
			t.Fatal("batch exceeded")
		}
		scanned += report.Scanned
		valid += report.Valid
		for _, issue := range report.Issues {
			issues[issue.MessageID] = issue.Reason
		}
		if !report.HasMore {
			break
		}
		if report.NextAfter <= after {
			t.Fatal("cursor did not advance")
		}
		after = report.NextAfter
	}
	if scanned != 5 || valid != 1 || issues[bad] != "NONCANONICAL_EML" || issues[missing] != "ARCHIVE_UNREADABLE" || issues[corrupt] != "HASH_MISMATCH" || issues[outside] != "ARCHIVE_UNREADABLE" {
		t.Fatal(scanned, valid, issues)
	}
	current, err := os.ReadFile(path)
	if err != nil || string(current) != string(before) {
		t.Fatal("preflight rewrote MIME")
	}
	assertCount(t, f, events, `SELECT count(*) FROM events`)
	assertCount(t, f, 0, `SELECT count(*) FROM delivery_attempts WHERE provider_id=$1`, providerID(t, f))
	if f.status(t, good) != "QUEUED" || f.status(t, bad) != "QUEUED" {
		t.Fatal("preflight changed queue")
	}
}
