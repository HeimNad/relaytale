package recovery

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"relaytale/internal/database"
	"relaytale/internal/encryption"
)

type Options struct {
	DB          *sql.DB
	DatabaseURL string
	StorageRoot string
	Bundle      string
	// Offline is the operator's assertion that ALL writers and senders are stopped.
	// Database locks cannot fence an already-running SMTP transaction or file writer.
	Offline bool
}

type Report struct {
	WebhookPending     int64 `json:"webhook_pending_or_in_flight"`
	AvailableArchives  int64 `json:"available_archives"`
	Providers          int64 `json:"providers_verified"`
	Messages           int64 `json:"messages"`
	IdempotencyRecords int64 `json:"idempotency_records"`
	Retirements        int64 `json:"retirements"`
	Sending            int64 `json:"sending_requires_reconciliation"`
	Unknown            int64 `json:"unknown_requires_reconciliation"`
	GateRetained       bool  `json:"startup_gate_retained"`
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func command(ctx context.Context, databaseURL, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	// libpq does not expand connection URIs supplied via PGDATABASE. Decode the
	// URI into environment fields; neither argv nor command errors expose secrets.
	u, err := url.Parse(databaseURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() == "" || u.User == nil || u.Fragment != "" {
		return errors.New("recovery tools require a PostgreSQL connection URI")
	}
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "PG") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	databaseName := strings.TrimPrefix(u.Path, "/")
	if databaseName == "" || strings.ContainsAny(databaseName, "=/\x00") {
		return errors.New("unsupported recovery database name")
	}
	if name == "pg_restore" {
		cmd.Args = append([]string{cmd.Path, "--dbname=" + databaseName}, args...)
	}
	password, _ := u.User.Password()
	cmd.Env = append(cmd.Env, "PGHOST="+u.Hostname(), "PGPORT="+port, "PGUSER="+u.User.Username(), "PGPASSWORD="+password, "PGDATABASE="+strings.TrimPrefix(u.Path, "/"), "PGPASSFILE=/dev/null")
	allowed := map[string]string{"sslmode": "PGSSLMODE", "sslrootcert": "PGSSLROOTCERT", "sslcert": "PGSSLCERT", "sslkey": "PGSSLKEY", "sslcrl": "PGSSLCRL", "connect_timeout": "PGCONNECT_TIMEOUT", "application_name": "PGAPPNAME", "channel_binding": "PGCHANNELBINDING"}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return errors.New("invalid database URI options")
	}
	for key, v := range values {
		env, ok := allowed[key]
		if !ok || len(v) != 1 {
			return errors.New("unsupported recovery database URI option")
		}
		cmd.Env = append(cmd.Env, env+"="+v[0])
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed; inspect PostgreSQL availability, version and privileges (details withheld)", name)
	}
	return nil
}

func paths(o Options) (string, string, error) {
	if !o.Offline {
		return "", "", errors.New("stop all writers and senders, then explicitly acknowledge offline operation")
	}
	root, err := filepath.Abs(o.StorageRoot)
	if err != nil {
		return "", "", err
	}
	bundle, err := filepath.Abs(o.Bundle)
	if err != nil {
		return "", "", err
	}
	if o.StorageRoot == "" || o.Bundle == "" || root == "/" || within(root, bundle) || within(bundle, root) {
		return "", "", errors.New("storage and bundle must be separate directories")
	}
	return root, bundle, nil
}

func Backup(ctx context.Context, o Options) error {
	if err := database.CheckNoRuntime(ctx, o.DB); err != nil {
		return err
	}
	root, bundle, err := paths(o)
	if err != nil {
		return err
	}
	if err = CheckStartup(root); err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return errors.New("storage root must be a real directory")
	}
	tx, err := o.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SET LOCAL lock_timeout='5s'"); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT tablename FROM pg_tables WHERE schemaname='public' AND tablename<>'runtime_owner' ORDER BY tablename`)
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var n string
		if err = rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, pgx.Identifier{"public", n}.Sanitize())
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(tables) == 0 {
		return errors.New("source database is empty")
	}
	if _, err = tx.ExecContext(ctx, "LOCK TABLE "+strings.Join(tables, ",")+" IN SHARE MODE"); err != nil {
		return errors.New("cannot quiesce database writers")
	}
	// Export only after obtaining every lock. Writers that committed while we
	// waited are visible to this snapshot; subsequent writes remain blocked.
	var snapshot string
	var snapshotAt time.Time
	if err = tx.QueryRowContext(ctx, "SELECT pg_export_snapshot(), clock_timestamp()").Scan(&snapshot, &snapshotAt); err != nil {
		return err
	}
	if _, err = verify(ctx, tx, root, root, nil, false); err != nil {
		return err
	}
	if err = os.Mkdir(bundle, 0700); err != nil {
		return err
	}
	if err = syncDir(filepath.Dir(bundle)); err != nil {
		return err
	}
	dump := filepath.Join(bundle, "database.dump")
	if err = command(ctx, o.DatabaseURL, "pg_dump", "--format=custom", "--no-owner", "--no-privileges", "--schema=public", "--exclude-table-data=public.runtime_owner", "--snapshot="+snapshot, "--file="+dump); err != nil {
		return err
	}
	if err = os.Chmod(dump, 0600); err != nil {
		return err
	}
	df, err := os.Open(dump)
	if err != nil {
		return err
	}
	err = df.Sync()
	df.Close()
	if err != nil {
		return err
	}
	d, err := digest(ctx, dump)
	if err != nil {
		return err
	}
	d.Path = "database.dump"
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	m := Manifest{Version: 1, CreatedAt: snapshotAt.UTC(), StorageRoot: root, WorkingDir: cwd, Schema: schemaHash(), Files: []File{d}}
	err = filepath.WalkDir(root, func(path string, e fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if e.Type()&os.ModeSymlink != 0 {
			return errors.New("archive symlink is unsupported")
		}
		if e.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		copied, err := copyFile(ctx, path, filepath.Join(bundle, "eml", rel))
		if err != nil {
			return err
		}
		copied.Path = "eml/" + filepath.ToSlash(rel)
		if len(m.Files) >= 100000 {
			return errors.New("bundle entry limit exceeded")
		}
		m.Files = append(m.Files, copied)
		return nil
	})
	if err != nil {
		return err
	}
	// Validate the COPIED bytes against metadata while the same locks are held.
	if _, err = verify(ctx, tx, root, filepath.Join(bundle, "eml"), nil, false); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if len(b) > maxManifest {
		return errors.New("manifest exceeds supported size")
	}
	// Manifest is the completion record. Incomplete bundles never receive it.
	f, err := os.OpenFile(filepath.Join(bundle, ".manifest-partial"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(filepath.Join(bundle, ".manifest-partial"), filepath.Join(bundle, "manifest.json")); err != nil {
		return err
	}
	return syncDir(bundle)
}

func Restore(ctx context.Context, o Options) error {
	if err := database.CheckNoRuntime(ctx, o.DB); err != nil {
		return err
	}
	root, bundle, err := paths(o)
	if err != nil {
		return err
	}
	m, err := VerifyBundle(ctx, bundle)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if root != m.StorageRoot || cwd != m.WorkingDir {
		return errors.New("restore requires the original absolute storage root and working directory")
	}
	var count int
	if err = o.DB.QueryRowContext(ctx, `SELECT count(*) FROM pg_namespace n
 WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_%'
 AND (n.nspname<>'public'
 OR EXISTS(SELECT 1 FROM pg_class c WHERE c.relnamespace=n.oid)
 OR EXISTS(SELECT 1 FROM pg_proc p WHERE p.pronamespace=n.oid)
 OR EXISTS(SELECT 1 FROM pg_type t WHERE t.typnamespace=n.oid))`).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return errors.New("restore target database must be empty")
	}
	if info, e := os.Lstat(root); e == nil {
		if !info.IsDir() {
			return errors.New("restore root is not a directory")
		}
		entries, e := os.ReadDir(root)
		if e != nil {
			return e
		}
		if len(entries) != 0 {
			return errors.New("restore target storage must be empty")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if err = mkdir(root); err != nil {
		return err
	}
	gate, err := os.OpenFile(filepath.Join(root, Marker), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = gate.WriteString("Restore in progress. Verify archives, key and post-backup external effects before manual release.\n")
	if err == nil {
		err = gate.Sync()
	}
	closeErr := gate.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = syncDir(root); err != nil {
		return err
	}
	for _, v := range m.Files {
		if v.Path == "database.dump" {
			continue
		}
		got, err := copyFile(ctx, filepath.Join(bundle, filepath.FromSlash(v.Path)), filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(v.Path, "eml/"))))
		if err != nil {
			return err
		}
		if got.Size != v.Size || got.SHA256 != v.SHA256 {
			return errors.New("bundle changed during restore")
		}
	}
	// pg_dump --schema=public includes CREATE SCHEMA public. A fresh PostgreSQL
	// database already has that empty schema. Drop only the empty schema, never
	// CASCADE; unexpected functions/types make this fail without deleting them.
	if _, err = o.DB.ExecContext(ctx, "DROP SCHEMA IF EXISTS public"); err != nil {
		return errors.New("target public schema is not empty or cannot be prepared")
	}
	if err = command(ctx, o.DatabaseURL, "pg_restore", "--exit-on-error", "--single-transaction", "--no-owner", "--no-privileges", filepath.Join(bundle, "database.dump")); err != nil {
		return err
	}
	return nil
}

// VerifyRestored performs no migrations and does not release the gate. A
// successful result proves local consistency, never absence of post-backup sends.
func VerifyRestored(ctx context.Context, db *sql.DB, root string, box *encryption.Box) (Report, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return Report{}, err
	}
	defer tx.Rollback()
	a, err := filepath.Abs(root)
	if err != nil {
		return Report{}, err
	}
	r, err := verify(ctx, tx, a, a, box, true)
	_, gateErr := os.Lstat(filepath.Join(a, Marker))
	r.GateRetained = gateErr == nil
	return r, err
}

func verify(ctx context.Context, q queryer, original, actual string, box *encryption.Box, keys bool) (Report, error) {
	if err := database.CheckNoRuntime(ctx, q); err != nil {
		return Report{}, err
	}
	var r Report
	var version int64
	if err := q.QueryRowContext(ctx, `SELECT coalesce(max(version_id),0) FROM goose_db_version WHERE is_applied`).Scan(&version); err != nil {
		return r, err
	}
	if version != schemaVersion() {
		return r, errors.New("database migration version does not match this binary")
	}
	var extraTables bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_tables WHERE schemaname NOT IN ('public','pg_catalog','information_schema') AND schemaname NOT LIKE 'pg_toast%')`).Scan(&extraTables); err != nil {
		return r, err
	}
	if extraTables {
		return r, errors.New("recovery supports a dedicated public-schema database only")
	}
	rows, err := q.QueryContext(ctx, `SELECT eml_path,eml_size,eml_sha256 FROM messages WHERE archive_state='AVAILABLE'`)
	if err != nil {
		return r, err
	}
	for rows.Next() {
		var path, sha string
		var size int64
		if err = rows.Scan(&path, &size, &sha); err != nil {
			rows.Close()
			return r, err
		}
		a, e := filepath.Abs(path)
		if e != nil || !within(original, a) {
			rows.Close()
			return r, errors.New("message archive is outside configured root")
		}
		rel, e := filepath.Rel(original, a)
		if e != nil {
			rows.Close()
			return r, e
		}
		got, e := digest(ctx, filepath.Join(actual, rel))
		if e != nil {
			rows.Close()
			return r, errors.New("required archive missing or unreadable")
		}
		if got.Size != size || got.SHA256 != sha {
			rows.Close()
			return r, errors.New("archive metadata checksum mismatch")
		}
		r.AvailableArchives++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return r, err
	}
	var bad bool
	err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM archive_retirements r JOIN messages m ON m.id=r.archive_id OR lower(regexp_replace(m.eml_path,'^.*/',''))=r.archive_id::text||'.eml') OR EXISTS(SELECT 1 FROM api_submission_ledger l LEFT JOIN messages m ON m.id=l.message_id WHERE m.id IS NULL)`).Scan(&bad)
	if err != nil {
		return r, err
	}
	if bad {
		return r, errors.New("idempotency or retirement inconsistency")
	}
	if err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages m LEFT JOIN queue_fair_owners o ON o.owner_key=queue_owner(m.source_type,m.api_key_id,m.smtp_account_id) WHERE o.owner_key IS NULL)`).Scan(&bad); err != nil {
		return r, err
	}
	if bad {
		return r, errors.New("queue fairness identity missing")
	}
	if err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages m LEFT JOIN api_keys k ON k.id=m.api_key_id WHERE (m.source_type='api' AND m.environment IS DISTINCT FROM k.environment) OR (m.environment='sandbox' AND (m.status<>'SANDBOX_STORED' OR m.attempt_count<>0 OR m.locked_by IS NOT NULL OR m.route_provider_id IS NOT NULL OR EXISTS(SELECT 1 FROM delivery_attempts a WHERE a.message_id=m.id) OR EXISTS(SELECT 1 FROM recipients r WHERE r.message_id=m.id AND r.status<>'SANDBOX_STORED'))))`).Scan(&bad); err != nil {
		return r, err
	}
	if bad {
		return r, errors.New("sandbox isolation inconsistency")
	}
	if err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sandbox_simulations s JOIN messages m ON m.id=s.message_id JOIN recipients r ON r.id=s.recipient_id JOIN events e ON e.id=s.event_id WHERE m.environment<>'sandbox' OR r.message_id<>m.id OR e.message_id<>m.id OR e.recipient_id IS DISTINCT FROM r.id OR e.source<>'sandbox' OR e.data IS DISTINCT FROM jsonb_build_object('simulation',true,'environment','sandbox','outcome',s.outcome) OR e.event_type IS DISTINCT FROM CASE s.outcome WHEN 'accepted' THEN 'SANDBOX_SIMULATED_ACCEPTED' WHEN 'temporary_failure' THEN 'SANDBOX_SIMULATED_TEMP_FAILED' WHEN 'permanent_failure' THEN 'SANDBOX_SIMULATED_PERM_FAILED' WHEN 'delivery_unknown' THEN 'SANDBOX_SIMULATED_UNKNOWN' WHEN 'bounce' THEN 'SANDBOX_SIMULATED_BOUNCE' END) OR EXISTS(SELECT 1 FROM events e LEFT JOIN sandbox_simulations s ON s.event_id=e.id WHERE e.event_type LIKE 'SANDBOX_SIMULATED_%' AND s.event_id IS NULL) OR EXISTS(SELECT 1 FROM sandbox_simulations GROUP BY message_id HAVING count(*)>100)`).Scan(&bad); err != nil {
		return r, err
	}
	if bad {
		return r, errors.New("sandbox simulation audit inconsistency")
	}
	if err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM delivery_attempts a JOIN messages m ON m.id=a.message_id WHERE m.expires_at IS NOT NULL AND a.data_armed_at>=m.expires_at)`).Scan(&bad); err != nil {
		return r, err
	}
	if bad {
		return r, errors.New("DATA authorization violates message deadline")
	}
	// Budget history must survive restore with its original website and units.
	if err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM api_submission_quota q JOIN messages m ON m.id=q.message_id WHERE m.source_type<>'api' OR m.api_key_id IS DISTINCT FROM q.api_key_id OR m.message_stream<>q.message_stream OR q.units<>(SELECT count(*) FROM recipients r WHERE r.message_id=m.id)) OR EXISTS(SELECT 1 FROM api_submission_ledger l LEFT JOIN api_submission_quota q ON q.message_id=l.message_id WHERE l.created_at>clock_timestamp()-interval '24 hours' AND q.message_id IS NULL)`).Scan(&bad); err != nil {
		return r, err
	}
	if bad {
		return r, errors.New("website budget history inconsistency")
	}
	if err = q.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER(WHERE status='SENDING'),count(*) FILTER(WHERE status='DELIVERY_UNKNOWN') FROM messages`).Scan(&r.Messages, &r.Sending, &r.Unknown); err != nil {
		return r, err
	}
	if err = q.QueryRowContext(ctx, `SELECT count(*) FROM api_submission_ledger`).Scan(&r.IdempotencyRecords); err != nil {
		return r, err
	}
	if err = q.QueryRowContext(ctx, `SELECT count(*) FROM archive_retirements`).Scan(&r.Retirements); err != nil {
		return r, err
	}
	if err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM webhook_outbox o JOIN webhook_endpoints e ON e.id=o.endpoint_id JOIN messages m ON m.id=o.message_id JOIN events v ON v.id=o.event_id WHERE m.api_key_id IS DISTINCT FROM e.api_key_id OR m.source_type<>'api' OR v.message_id<>m.id OR (o.payload::jsonb->>'event_id') IS DISTINCT FROM o.event_id::text OR (o.payload::jsonb->>'message_id') IS DISTINCT FROM o.message_id::text OR (o.payload::jsonb->>'sequence') IS DISTINCT FROM o.sequence::text OR (m.environment='sandbox' AND ((o.payload::jsonb->>'environment') IS DISTINCT FROM 'sandbox' OR (o.payload::jsonb->>'simulation') IS DISTINCT FROM 'true')) OR (m.environment='production' AND o.payload::jsonb ? 'simulation') OR (o.payload::jsonb->>'schema_version'='2' AND (o.payload::jsonb->'business') IS DISTINCT FROM jsonb_build_object('client_reference',m.client_reference,'tags',m.business_tags)))`).Scan(&bad); err != nil {
		return r, err
	}
	if bad {
		return r, errors.New("webhook ownership or payload inconsistency")
	}
	if err = q.QueryRowContext(ctx, `SELECT count(*) FROM webhook_outbox WHERE state IN ('PENDING','IN_FLIGHT')`).Scan(&r.WebhookPending); err != nil {
		return r, err
	}
	if !keys {
		return r, nil
	}
	rows, err = q.QueryContext(ctx, `SELECT id::text,password_ciphertext,nonce FROM providers`)
	if err != nil {
		return r, err
	}
	for rows.Next() {
		var id string
		var ciphertext, nonce []byte
		if err = rows.Scan(&id, &ciphertext, &nonce); err != nil {
			rows.Close()
			return r, err
		}
		if box == nil {
			rows.Close()
			return r, errors.New("master key required to verify provider credentials")
		}
		if _, err = box.Open(id, ciphertext, nonce); err != nil {
			rows.Close()
			return r, errors.New("provider credential verification failed")
		}
		r.Providers++
	}
	err = rows.Err()
	rows.Close()
	return r, err
}
