package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"relaytale/internal/api"
	"relaytale/internal/database"
	"relaytale/internal/encryption"
	"relaytale/internal/message"
	"relaytale/internal/operations"
	"relaytale/internal/recovery"
	"relaytale/internal/testsmtp"
	"relaytale/internal/webhook"
)

// Dedicated databases are essential: a shared fixture DB has unrelated archive
// roots and cannot represent a coherent stopped deployment.
func recoveryDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		if os.Getenv("REQUIRE_INTEGRATION_TESTS") == "1" {
			t.Fatal("TEST_DATABASE_URL required")
		}
		t.Skip("database unavailable")
	}
	admin, err := database.Open(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	name := "recovery_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec("CREATE DATABASE " + quoted); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	db, err := database.Open(context.Background(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); admin.Exec("DROP DATABASE " + quoted + " WITH (FORCE)"); admin.Close() })
	return db, u.String()
}

func TestRecoveryRoundTripAndFailureGates(t *testing.T) {
	source, sourceURL := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", sourceURL)
	f := delivery(t, testsmtp.Options{}, 1)
	groupID := quotaGroup(t, f, 10, 20)
	attachGroup(t, f, providerID(t, f), groupID)
	sub := prepareAPI(t, f, "one@example.test")
	sub.Business = message.Business{ClientReference: "backup:order", Tags: []string{"receipt"}}
	identity := testAPIKey(t, f)
	sub.Stream = "otp"
	sub.ExpiresAt = time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	mustExec(t, f, `INSERT INTO api_message_streams(api_key_id,name,weight,day_limit) VALUES($1,'otp',3,5)`, identity)
	mustExec(t, f, `UPDATE api_keys SET submission_day_limit=1 WHERE id=$1`, identity)
	result, err := (message.Postgres{DB: f.db}).EnqueueIdempotent(context.Background(), sub, identity, "backup-key")
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, f, `UPDATE queue_fair_owners SET stream_cursor=2,last_ticket=nextval('queue_fair_ticket') WHERE owner_key='api:'||$1`, identity)
	endpoint := webhook.Endpoint{SchemaVersion: 2, ID: uuid.NewString(), APIKeyID: identity, URL: "https://example.com/hook", Keys: []webhook.Key{{ID: "v1", Secret: strings.Repeat("ab", 32)}}}
	// testAPIKey is a synthetic legacy identity; mark its explicit HTTP scope.
	mustExec(t, f, `UPDATE api_keys SET prefix='rts_' WHERE id=$1`, identity)
	if err = webhook.Sync(context.Background(), f.db, []webhook.Endpoint{endpoint}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, f, `INSERT INTO events(id,message_id,event_type,source,data) VALUES($1,$2,'DELIVERY_UNKNOWN','worker','{}')`, uuid.NewString(), result.MessageID)
	var webhookPayload string
	if err = f.db.QueryRow(`SELECT payload FROM webhook_outbox WHERE message_id=$1`, result.MessageID).Scan(&webhookPayload); err != nil {
		t.Fatal(err)
	}
	mustExec(t, f, `UPDATE messages SET status='DELIVERY_UNKNOWN' WHERE id=$1`, result.MessageID)
	mustExec(t, f, `UPDATE recipients SET status='DELIVERY_UNKNOWN' WHERE message_id=$1`, result.MessageID)
	orphan := prepareAPI(t, f, "two@example.test")
	observation := oldArchiveObservation(t, f, orphan)
	if _, err = operations.ReclaimArchive(context.Background(), f.db, f.root, observation, "rehearsal", "synthetic orphan", true); err != nil {
		t.Fatal(err)
	}
	// A missing PURGED archive is intentional and must not invalidate a backup.
	purged := f.seed(t)
	var purgedPath string
	if err = f.db.QueryRow(`SELECT eml_path FROM messages WHERE id=$1`, purged).Scan(&purgedPath); err != nil {
		t.Fatal(err)
	}
	mustExec(t, f, `UPDATE messages SET archive_state='PURGED',eml_purged_at=now() WHERE id=$1`, purged)
	if err = os.Remove(purgedPath); err != nil {
		t.Fatal(err)
	}
	_, sandboxKey, sandboxReceiver := sendFixture(t, f)
	sandboxKey.ID = uuid.NewString()
	sandboxKey.Token = "rts_" + strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")
	sandboxKey.Environment = "sandbox"
	sandboxConfig, _ := json.Marshal([]api.SendKey{sandboxKey})
	sandboxHandler, err := api.NewSendHandler(context.Background(), string(sandboxConfig), sandboxReceiver)
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := submitStatusFixture(t, sandboxHandler, sandboxKey, f.raw(), nil)
	bundle := filepath.Join(t.TempDir(), "bundle")
	ctx := context.Background()
	opts := recovery.Options{DB: source, DatabaseURL: sourceURL, StorageRoot: f.root, Bundle: bundle, Offline: true}
	start := time.Now()
	if err = recovery.Backup(ctx, opts); err != nil {
		t.Fatal(err)
	}
	t.Logf("backup duration: %s", time.Since(start))
	m, err := recovery.VerifyBundle(ctx, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) < 2 {
		t.Fatal("empty bundle")
	}
	// Corrupt one archived payload, reject before creating target files or tables.
	payload := filepath.Join(bundle, filepath.FromSlash("eml/"+strings.TrimPrefix(sub.Archive.Path, f.root+"/")))
	original, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(payload, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	target, targetURL := recoveryDB(t)
	restored := opts
	restored.DB = target
	restored.DatabaseURL = targetURL
	if err = recovery.Restore(ctx, restored); err == nil {
		t.Fatal("corrupt bundle accepted")
	}
	if err = os.WriteFile(payload, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err = recovery.Restore(ctx, opts); err == nil {
		t.Fatal("nonempty source database accepted as target")
	}
	if err = recovery.Restore(ctx, restored); err == nil {
		t.Fatal("nonempty archive target accepted")
	}

	if _, err = target.Exec(`CREATE FUNCTION public.existing_function() RETURNS integer LANGUAGE sql AS 'SELECT 1'`); err != nil {
		t.Fatal(err)
	}
	if err = recovery.Restore(ctx, restored); err == nil || !strings.Contains(err.Error(), "database must be empty") {
		t.Fatal("existing function not rejected", err)
	}
	if _, err = target.Exec(`DROP FUNCTION public.existing_function()`); err != nil {
		t.Fatal(err)
	}
	// Stand in for a new volume mounted at the same absolute path.
	saved := f.root + "-source"
	if err = os.Rename(f.root, saved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(saved) })
	start = time.Now()
	if err = recovery.Restore(ctx, restored); err != nil {
		t.Fatal(err)
	}
	if err = recovery.CheckStartup(f.root); err == nil {
		t.Fatal("restore started service")
	}
	report, err := recovery.VerifyRestored(ctx, target, f.root, f.box)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("restore plus verify duration: %s; report: %+v", time.Since(start), report)
	if report.Messages != 3 || report.AvailableArchives != 2 || report.Unknown != 1 || report.IdempotencyRecords != 2 || report.Retirements != 1 || report.Providers != 1 || !report.GateRetained {
		t.Fatal(report)
	}
	var sandboxEnvironment, sandboxStatus string
	if err = target.QueryRow(`SELECT environment,status FROM messages WHERE id=$1`, sandboxID).Scan(&sandboxEnvironment, &sandboxStatus); err != nil || sandboxEnvironment != "sandbox" || sandboxStatus != "SANDBOX_STORED" {
		t.Fatal("sandbox restore mismatch", err)
	}
	if _, err = target.Exec(`UPDATE messages SET environment='production',status='QUEUED' WHERE id=$1`, sandboxID); err == nil {
		t.Fatal("restored sandbox can be promoted")
	}
	var restoredWebhook string
	if err = target.QueryRow(`SELECT payload FROM webhook_outbox WHERE message_id=$1`, result.MessageID).Scan(&restoredWebhook); err != nil || restoredWebhook != webhookPayload || !strings.Contains(restoredWebhook, `"client_reference": "backup:order"`) || report.WebhookPending != 1 {
		t.Fatal("webhook recovery mismatch", err, report)
	}
	wrong, _ := encryption.New(strings.Repeat("cd", 32))
	if _, err = recovery.VerifyRestored(ctx, target, f.root, wrong); err == nil {
		t.Fatal("wrong key accepted")
	}
	if _, err = recovery.VerifyRestored(ctx, target, f.root, nil); err == nil {
		t.Fatal("missing key accepted")
	}
	replay, err := (message.Postgres{DB: target}).EnqueueIdempotent(ctx, sub, identity, "backup-key")
	if err != nil || !replay.Replayed || replay.MessageID != result.MessageID {
		t.Fatal(replay, err)
	}
	var restoredExpiry time.Time
	if err = target.QueryRow(`SELECT expires_at FROM messages WHERE id=$1`, result.MessageID).Scan(&restoredExpiry); err != nil || !restoredExpiry.Equal(sub.ExpiresAt) {
		t.Fatal("deadline did not survive restore", err)
	}
	var restoredStream string
	var restoredCursor, restoredWeight int
	if err = target.QueryRow(`SELECT m.message_stream,o.stream_cursor,s.weight FROM messages m JOIN queue_fair_owners o ON o.owner_key=queue_owner(m.source_type,m.api_key_id,m.smtp_account_id) JOIN api_message_streams s ON s.api_key_id=m.api_key_id AND s.name=m.message_stream WHERE m.id=$1`, result.MessageID).Scan(&restoredStream, &restoredCursor, &restoredWeight); err != nil || restoredStream != "otp" || restoredCursor != 2 || restoredWeight != 3 {
		t.Fatal("stream state did not survive restore", err)
	}
	var restoredGroup string
	var restoredGroupLimit int
	if err = target.QueryRow(`SELECT p.quota_group_id,g.daily_limit FROM providers p JOIN provider_quota_groups g ON g.id=p.quota_group_id WHERE g.id=$1`, groupID).Scan(&restoredGroup, &restoredGroupLimit); err != nil || restoredGroup != groupID || restoredGroupLimit != 20 {
		t.Fatal("provider quota group did not survive restore", err)
	}
	var restoredBudget, restoredUsage int
	if err = target.QueryRow(`SELECT submission_day_limit,(SELECT coalesce(sum(units),0) FROM api_submission_quota WHERE api_key_id=$1) FROM api_keys WHERE id=$1`, identity).Scan(&restoredBudget, &restoredUsage); err != nil || restoredBudget != 1 || restoredUsage != 1 {
		t.Fatal("website budget did not survive restore", restoredBudget, restoredUsage, err)
	}
	if err = (message.Postgres{DB: target}).Enqueue(ctx, orphan); err == nil {
		t.Fatal("retired ID accepted after restore")
	}
	var n int
	if err = target.QueryRow(`SELECT count(*) FROM delivery_attempts`).Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if err = os.Remove(sub.Archive.Path); err != nil {
		t.Fatal(err)
	}
	if _, err = recovery.VerifyRestored(ctx, target, f.root, f.box); err == nil {
		t.Fatal("missing required archive accepted")
	}
	if err = recovery.CheckStartup(f.root); err == nil {
		t.Fatal("failed validation removed gate")
	}
}

func TestRecoveryInterruptedTools(t *testing.T) {
	source, sourceURL := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", sourceURL)
	f := delivery(t, testsmtp.Options{}, 1)
	f.seed(t)
	bundle := filepath.Join(t.TempDir(), "bundle")
	opts := recovery.Options{DB: source, DatabaseURL: sourceURL, StorageRoot: f.root, Bundle: bundle, Offline: true}
	ctx := context.Background()
	t.Run("failed dump never publishes", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if err := recovery.Backup(ctx, opts); err == nil {
			t.Fatal("missing pg_dump accepted")
		}
		if _, err := os.Stat(filepath.Join(bundle, "manifest.json")); !os.IsNotExist(err) {
			t.Fatal("completion manifest published", err)
		}
	})
	opts.Bundle = filepath.Join(t.TempDir(), "good")
	if err := recovery.Backup(ctx, opts); err != nil {
		t.Fatal(err)
	}
	target, targetURL := recoveryDB(t)
	opts.DB = target
	opts.DatabaseURL = targetURL
	saved := f.root + "-source"
	if err := os.Rename(f.root, saved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(saved) })

	t.Run("database import failure retains gate", func(t *testing.T) {
		u, err := url.Parse(targetURL)
		if err != nil {
			t.Fatal(err)
		}
		name := pgx.Identifier{strings.TrimPrefix(u.Path, "/")}.Sanitize()
		if _, err = source.Exec("ALTER DATABASE " + name + " SET default_transaction_read_only=on"); err != nil {
			t.Fatal(err)
		}
		if err = recovery.Restore(ctx, opts); err == nil {
			t.Fatal("read-only import accepted")
		}
		if err = recovery.CheckStartup(f.root); err == nil {
			t.Fatal("SQL failure lost gate")
		}
		var n int
		if err = target.QueryRow(`SELECT count(*) FROM pg_tables WHERE schemaname='public'`).Scan(&n); err != nil || n != 0 {
			t.Fatal(n, err)
		}
		if _, err = source.Exec("ALTER DATABASE " + name + " RESET default_transaction_read_only"); err != nil {
			t.Fatal(err)
		}
		// Remove only this synthetic failed restore directory for the next fault case.
		if err = os.RemoveAll(f.root); err != nil {
			t.Fatal(err)
		}
	})
	t.Setenv("PATH", t.TempDir())
	if err := recovery.Restore(ctx, opts); err == nil {
		t.Fatal("missing pg_restore accepted")
	}
	if err := recovery.CheckStartup(f.root); err == nil {
		t.Fatal("failed restore has no gate")
	}
	var count int
	if err := target.QueryRow(`SELECT count(*) FROM pg_tables WHERE schemaname='public'`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}
