package integration

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"relaytale/internal/message"
	"relaytale/internal/operations"
	"relaytale/internal/storage"
	"relaytale/internal/testsmtp"
)

func oldArchiveObservation(t *testing.T, f *deliveryFixture, sub message.Submission) string {
	t.Helper()
	id := uuid.NewString()
	path, err := filepath.Rel(f.root, sub.Archive.Path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, f, `INSERT INTO archive_observations(id,archive_id,root,path,size,sha256,observed_at) VALUES($1,$2,$3,$4,$5,$6,clock_timestamp()-interval '25 hours')`, id, sub.ID, f.root, filepath.ToSlash(path), sub.Archive.Size, sub.Archive.SHA256)
	return id
}
func TestArchiveScanRangeAndObservations(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	for _, day := range []string{"2026-01-01", "2026-02-14", "2026-03-02"} {
		at, _ := time.Parse("2006-01-02", day)
		_, err := (storage.LocalStore{Root: f.root, MaxBytes: 1 << 20}).Save(context.Background(), uuid.NewString(), at, strings.NewReader(f.raw()))
		if err != nil {
			t.Fatal(err)
		}
	}
	var cursor string
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		report, err := operations.ScanArchives(context.Background(), f.db, f.root, "2026-01-01", "2026-03-02", cursor, 1, true)
		if err != nil || report.Scanned > 1 || report.Days > 31 {
			t.Fatal(report, err)
		}
		for _, v := range report.Observations {
			if seen[v.ID] {
				t.Fatal("duplicate across pages")
			}
			seen[v.ID] = true
		}
		if report.Complete {
			break
		}
		if report.NextCursor == "" || report.NextCursor == cursor {
			t.Fatal("cursor stuck")
		}
		cursor = report.NextCursor
	}
	if len(seen) != 3 {
		t.Fatal(seen)
	}
	first, err := operations.ScanArchives(context.Background(), f.db, f.root, "2026-01-01", "2026-03-02", "", 1, true)
	if err != nil || len(first.Observations) != 1 || !seen[first.Observations[0].ID] {
		t.Fatal(first, err)
	}
	if _, err = operations.ScanArchives(context.Background(), f.db, f.root, "2026-01-01", "2026-03-03", first.NextCursor, 1, true); err == nil {
		t.Fatal("cross-scope cursor accepted")
	}
	listed, err := operations.ListArchiveObservations(context.Background(), f.db, f.root, "", 100)
	if err != nil || len(listed) != 3 {
		t.Fatal(listed, err)
	}
	if _, err = operations.ReclaimArchive(context.Background(), f.db, f.root, listed[0].ID, "tester", "age check", true); err == nil {
		t.Fatal("fresh evidence reclaimed")
	}
	assertCount(t, f, 0, `SELECT count(*) FROM archive_retirements WHERE observation_id=$1`, listed[0].ID)
	if _, err = f.db.Exec(`DELETE FROM archive_observations WHERE id=$1`, listed[0].ID); err == nil {
		t.Fatal("evidence mutable")
	}
}
func TestArchiveReclaimDryRunApplyAndLateEnqueue(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	sub := prepareAPI(t, f, "one@example.test")
	id := oldArchiveObservation(t, f, sub)
	result, err := operations.ReclaimArchive(context.Background(), f.db, f.root, id, "", "", false)
	if err != nil || result.Retired || result.Removed {
		t.Fatal(result, err)
	}
	if _, err = os.Stat(sub.Archive.Path); err != nil {
		t.Fatal(err)
	}
	result, err = operations.ReclaimArchive(context.Background(), f.db, f.root, id, "tester", "orphan confirmed", true)
	if err != nil || !result.Removed || !result.Retired {
		t.Fatal(result, err)
	}
	if _, err = os.Stat(sub.Archive.Path); !os.IsNotExist(err) {
		t.Fatal("file not reclaimed", err)
	}
	result, err = operations.ReclaimArchive(context.Background(), f.db, f.root, id, "tester", "retry completion", true)
	if err != nil || !result.Removed {
		t.Fatal(result, err)
	}
	if err = (message.Postgres{DB: f.db}).Enqueue(context.Background(), sub); err == nil {
		t.Fatal("late SMTP enqueue passed retirement fence")
	}
	key := testAPIKey(t, f)
	if _, err = (message.Postgres{DB: f.db}).EnqueueIdempotent(context.Background(), sub, key, "late"); err == nil {
		t.Fatal("late API enqueue passed retirement fence")
	}
	assertCount(t, f, 0, `SELECT count(*) FROM messages WHERE id=$1`, sub.ID)
	assertCount(t, f, 0, `SELECT count(*) FROM api_submission_ledger WHERE api_key_id=$1`, key)
	if _, err = f.db.Exec(`DELETE FROM archive_retirements WHERE archive_id=$1`, sub.ID); err == nil {
		t.Fatal("retirement mutable")
	}
}
func TestArchiveReclaimProtectsReferencesAndChanges(t *testing.T) {
	for _, mode := range []string{"referenced", "alias", "changed", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			f := delivery(t, testsmtp.Options{}, 1)
			sub := prepareAPI(t, f, "one@example.test")
			id := oldArchiveObservation(t, f, sub)
			switch mode {
			case "referenced":
				if err := (message.Postgres{DB: f.db}).Enqueue(context.Background(), sub); err != nil {
					t.Fatal(err)
				}
				mustExec(t, f, `UPDATE messages SET status='DELIVERY_UNKNOWN' WHERE id=$1`, sub.ID)
			case "alias":
				alias := filepath.Join(f.root, "alias-"+uuid.NewString()+".eml")
				if err := os.Symlink(sub.Archive.Path, alias); err != nil {
					t.Fatal(err)
				}
				other := sub
				other.ID = uuid.NewString()
				other.Archive.Path = alias
				if err := (message.Postgres{DB: f.db}).Enqueue(context.Background(), other); err != nil {
					t.Fatal(err)
				}
			case "changed":
				if err := os.WriteFile(sub.Archive.Path, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				outside := filepath.Join(t.TempDir(), "evidence")
				if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(sub.Archive.Path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, sub.Archive.Path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := operations.ReclaimArchive(context.Background(), f.db, f.root, id, "tester", "refuse", true); err == nil {
				t.Fatal("unsafe reclaim accepted")
			}
			if _, err := os.Lstat(sub.Archive.Path); err != nil {
				t.Fatal("protected evidence lost", err)
			}
			assertCount(t, f, 0, `SELECT count(*) FROM archive_retirements WHERE archive_id=$1`, sub.ID)
		})
	}
}
func TestArchiveReclaimCommitResponseLoss(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	sub := prepareAPI(t, f, "one@example.test")
	id := oldArchiveObservation(t, f, sub)
	config, err := pgx.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	config.TLSConfig = nil
	config.Fallbacks = nil
	var armed, dropped atomic.Bool
	config.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, e := (&net.Dialer{}).DialContext(ctx, network, address)
		if e != nil {
			return nil, e
		}
		return &commitResponseConn{Conn: conn, armed: &armed, dropped: &dropped}, nil
	}
	db := stdlib.OpenDB(*config)
	defer db.Close()
	armed.Store(true)
	result, err := operations.ReclaimArchive(context.Background(), db, f.root, id, "tester", "lose commit response", true)
	if err == nil || result.Removed || !dropped.Load() {
		t.Fatal(result, err)
	}
	assertCount(t, f, 1, `SELECT count(*) FROM archive_retirements WHERE archive_id=$1`, sub.ID)
	if _, err = os.Stat(sub.Archive.Path); err != nil {
		t.Fatal("ambiguous commit deleted file", err)
	}
	result, err = operations.ReclaimArchive(context.Background(), f.db, f.root, id, "tester", "resume", true)
	if err != nil || !result.Removed {
		t.Fatal(result, err)
	}
}
func TestArchiveReclaimCompletionFailureResumes(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	sub := prepareAPI(t, f, "one@example.test")
	id := oldArchiveObservation(t, f, sub)
	name := "fail_reclaim_" + strings.ReplaceAll(sub.ID, "-", "")
	mustExec(t, f, `CREATE FUNCTION `+name+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.archive_id='`+sub.ID+`'::uuid THEN RAISE EXCEPTION 'injected completion failure'; END IF; RETURN NEW; END $$`)
	mustExec(t, f, `CREATE TRIGGER `+name+` BEFORE INSERT ON archive_reclaims FOR EACH ROW EXECUTE FUNCTION `+name+`() `)
	t.Cleanup(func() {
		f.db.Exec(`DROP TRIGGER IF EXISTS ` + name + ` ON archive_reclaims`)
		f.db.Exec(`DROP FUNCTION IF EXISTS ` + name + `()`)
	})
	result, err := operations.ReclaimArchive(context.Background(), f.db, f.root, id, "tester", "completion failure", true)
	if err == nil || !result.Retired {
		t.Fatal(result, err)
	}
	if _, err = os.Stat(sub.Archive.Path); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	mustExec(t, f, `DROP TRIGGER `+name+` ON archive_reclaims`)
	result, err = operations.ReclaimArchive(context.Background(), f.db, f.root, id, "tester", "resume", true)
	if err != nil || !result.Removed {
		t.Fatal(result, err)
	}
	// The operator identity/reason survive even when the completion write failed.
	var actor, reason string
	if err = f.db.QueryRow(`SELECT actor,reason FROM archive_retirements WHERE archive_id=$1`, sub.ID).Scan(&actor, &reason); err != nil || actor != "tester" || reason != "completion failure" {
		t.Fatal(actor, reason, err)
	}
}

func TestArchiveReclaimWaitsForInFlightMessage(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	sub := prepareAPI(t, f, "one@example.test")
	id := oldArchiveObservation(t, f, sub)
	tx, err := f.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO messages(id,source_type,envelope_from,status,eml_path,eml_size,eml_sha256) VALUES($1,'internal','test@example.test','DELIVERY_UNKNOWN',$2,$3,$4)`, sub.ID, sub.Archive.Path, sub.Archive.Size, sub.Archive.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	result, err := operations.ReclaimArchive(context.Background(), f.db, f.root, id, "tester", "in flight", true)
	if err == nil || result.Retired || result.Removed {
		t.Fatal("did not fail closed on active writer", result, err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = operations.ReclaimArchive(context.Background(), f.db, f.root, id, "tester", "committed", true); err == nil {
		t.Fatal("committed UNKNOWN reclaimed")
	}
	if _, err = os.Stat(sub.Archive.Path); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveRetirementRejectsStaleIsolationAndPathReuse(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	sub := prepareAPI(t, f, "one@example.test")
	id := oldArchiveObservation(t, f, sub)
	if _, err := operations.ReclaimArchive(context.Background(), f.db, f.root, id, "tester", "retire", true); err != nil {
		t.Fatal(err)
	}
	sub.ID = uuid.NewString()
	if err := (message.Postgres{DB: f.db}).Enqueue(context.Background(), sub); err == nil {
		t.Fatal("retired path reused under different ID")
	}
	tx, err := f.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`SET TRANSACTION ISOLATION LEVEL REPEATABLE READ`); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(`INSERT INTO messages(id,source_type,envelope_from,status,eml_path,eml_size,eml_sha256) VALUES($1,'internal','test@example.test','QUEUED',$2,0,$3)`, uuid.NewString(), uuid.NewString()+".eml", strings.Repeat("0", 64))
	if err == nil {
		t.Fatal("snapshot isolation bypassed retirement visibility fence")
	}
}

func TestArchiveReclaimRollbackKeepsOriginal(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	sub := prepareAPI(t, f, "one@example.test")
	id := oldArchiveObservation(t, f, sub)
	name := "fail_retire_" + strings.ReplaceAll(sub.ID, "-", "")
	mustExec(t, f, `CREATE FUNCTION `+name+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.archive_id='`+sub.ID+`'::uuid THEN RAISE EXCEPTION 'injected retirement commit failure'; END IF; RETURN NEW; END $$`)
	mustExec(t, f, `CREATE CONSTRAINT TRIGGER `+name+` AFTER INSERT ON archive_retirements DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION `+name+`() `)
	t.Cleanup(func() {
		f.db.Exec(`DROP TRIGGER IF EXISTS ` + name + ` ON archive_retirements`)
		f.db.Exec(`DROP FUNCTION IF EXISTS ` + name + `()`)
	})
	result, err := operations.ReclaimArchive(context.Background(), f.db, f.root, id, "tester", "rollback", true)
	if err == nil || result.Removed {
		t.Fatal(result, err)
	}
	assertCount(t, f, 0, `SELECT count(*) FROM archive_retirements WHERE archive_id=$1`, sub.ID)
	if _, err = os.Stat(sub.Archive.Path); err != nil {
		t.Fatal("rolled-back retirement removed original", err)
	}
}

func TestArchiveRetirementEvidenceIdentityConstraint(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	sub := prepareAPI(t, f, "one@example.test")
	id := oldArchiveObservation(t, f, sub)
	if _, err := f.db.Exec(`INSERT INTO archive_retirements(archive_id,observation_id,actor,reason) VALUES($1,$2,'tester','mismatched evidence')`, uuid.NewString(), id); err == nil {
		t.Fatal("retirement accepted mismatched archive evidence")
	}
	if _, err := os.Stat(sub.Archive.Path); err != nil {
		t.Fatal(err)
	}
}
