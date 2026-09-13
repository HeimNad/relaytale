package main

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/uuid"
	"relaytale/internal/database"
	"relaytale/internal/encryption"
	"relaytale/internal/operations"
	"relaytale/internal/provider"
	"relaytale/internal/queue"
	"relaytale/internal/smtpclient"
)

func operationDB(ctx context.Context) (*sql.DB, error) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	db, err := database.Open(ctx, url)
	if err != nil {
		return nil, errors.New("database connection failed")
	}
	return db, nil
}
func runOperation(command string, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	db, err := operationDB(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	switch command {
	case "preflight-eml":
		fs := flag.NewFlagSet(command, flag.ContinueOnError)
		root := fs.String("storage-dir", os.Getenv("EML_STORAGE_DIR"), "archive root")
		after := fs.String("after-id", "", "exclusive message UUID cursor")
		limit := fs.Int("limit", 1000, "maximum archives in this read-only batch (1..10000)")
		maxBytes := fs.Int64("max-message-bytes", 25<<20, "target maximum raw message bytes")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("unexpected preflight arguments")
		}
		if *root == "" {
			*root = "data/eml"
		}
		report, err := operations.Preflight(ctx, db, *root, *after, *limit, *maxBytes)
		if writeErr := json.NewEncoder(os.Stdout).Encode(report); writeErr != nil {
			return writeErr
		}
		if err != nil {
			return errors.New("preflight incomplete; verify schema, archive root, cursor and bounds")
		}
		if len(report.Issues) > 0 {
			return errors.New("preflight found archives requiring manual review")
		}
		return nil

	case "add-suppression", "release-suppression", "list-suppressions":
		return runSuppression(ctx, db, command, args)
	case "resolve-unknown":
		fs := flag.NewFlagSet(command, flag.ContinueOnError)
		v := queue.Resolution{}
		fs.StringVar(&v.RecipientID, "recipient-id", "", "recipient UUID")
		fs.StringVar(&v.ExpectedAttempt, "expected-attempt", "", "latest attempt UUID")
		fs.StringVar(&v.Action, "action", "", "retry, mark-delivered, mark-failed")
		fs.StringVar(&v.Actor, "actor", "", "operator identity (audit label)")
		fs.StringVar(&v.Reason, "reason", "", "required audit reason")
		fs.BoolVar(&v.AcknowledgeDuplicate, "acknowledge-duplicate-risk", false, "explicitly accept duplicate delivery risk")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("unexpected arguments")
		}
		id, err := (queue.Repository{DB: db}).ResolveUnknown(ctx, v)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"audit_id": id, "action": v.Action})

	case "export-records":
		fs := flag.NewFlagSet(command, flag.ContinueOnError)
		id := fs.String("message-id", "", "optional Gateway UUID")
		since := fs.String("since", "1970-01-01T00:00:00Z", "inclusive received timestamp, RFC3339")
		until := fs.String("until", time.Now().UTC().Format(time.RFC3339Nano), "exclusive received timestamp, RFC3339")
		output := fs.String("output", "", "new gzip JSONL output file")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() != 0 || *output == "" {
			return errors.New("--output is required")
		}
		from, err := time.Parse(time.RFC3339Nano, *since)
		if err != nil {
			return errors.New("invalid --since")
		}
		to, err := time.Parse(time.RFC3339Nano, *until)
		if err != nil {
			return errors.New("invalid --until")
		}
		var report operations.ExportReport
		if err := operations.Audit(ctx, db, "EXPORT_STARTED", "local_cli", map[string]any{"message_id": *id, "since": from, "until": to}); err != nil {
			return err
		}
		file, err := operations.WriteAtomic(*output, func(w io.Writer) error {
			gz := gzip.NewWriter(w)
			report, err = operations.Export(ctx, db, gz, operations.Filter{MessageID: *id, Since: from, Until: to})
			if err != nil {
				_ = gz.Close()
				return err
			}
			return gz.Close()
		})
		if err != nil {
			_ = operations.Audit(ctx, db, "EXPORT_FAILED", "local_cli", map[string]string{"reason": "export or publication failed"})
			return errors.New("export failed; verify filters, database and output path (existing files are never overwritten)")
		}
		if err := operations.Audit(ctx, db, "EXPORT_FINISHED", "local_cli", map[string]any{"sha256": file.SHA256, "bytes": file.Bytes, "records": report.Records}); err != nil {
			return errors.New("export file created but completion audit failed; verify the file and database")
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"file": file, "export": report})
	case "cleanup":
		fs := flag.NewFlagSet(command, flag.ContinueOnError)
		eml := fs.Int("eml-days", 180, "completed EML retention; 0 disables")
		debug := fs.Int("debug-days", 30, "SMTP debug retention; 0 disables")
		batch := fs.Int("batch", 100, "maximum items per category")
		apply := fs.Bool("apply", false, "apply cleanup; default is a dry run")
		root := fs.String("storage-dir", os.Getenv("EML_STORAGE_DIR"), "EML storage root")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("unexpected cleanup arguments")
		}
		if *root == "" {
			*root = "data/eml"
		}
		report, err := operations.Cleanup(ctx, db, *root, operations.Retention{EMLDays: *eml, DebugDays: *debug, Batch: *batch, Apply: *apply, Actor: "local_cli"})
		if writeErr := json.NewEncoder(os.Stdout).Encode(report); writeErr != nil {
			return writeErr
		}
		return err
	case "list-providers":
		if len(args) > 0 {
			return errors.New("list-providers takes no arguments")
		}
		rows, err := db.QueryContext(ctx, `SELECT jsonb_build_object('id',p.id,'name',p.name,'enabled',p.enabled,'host',p.host,'port',p.port,'security',p.security,'priority',p.priority,'max_connections',p.max_connections,'from_domains',p.from_domains,
 'hourly_limit',p.hourly_limit,'daily_limit',p.daily_limit,'hourly_reserved',q.hourly,'daily_reserved',q.daily,'quota_unit','recipient_attempt',
 'health_check_enabled',p.health_check_enabled,'circuit_state',coalesce(h.circuit_state,'CLOSED'),'open_until',h.open_until,'probe_attempt_id',h.probe_attempt_id,
 'last_success_at',h.last_success_at,'last_failure_at',h.last_failure_at,'last_error',h.last_error,'health_window_seconds',600,'health_successes',s.successes,'health_failures',s.failures)
 FROM providers p LEFT JOIN provider_health h ON h.provider_id=p.id
 CROSS JOIN LATERAL (SELECT coalesce(sum(units) FILTER(WHERE reserved_at>clock_timestamp()-interval '1 hour'),0) hourly,coalesce(sum(units),0) daily FROM provider_quota WHERE provider_id=p.id AND reserved_at>clock_timestamp()-interval '24 hours') q
 CROSS JOIN LATERAL (SELECT count(*) FILTER(WHERE health_outcome='SUCCESS') successes,count(*) FILTER(WHERE health_outcome='FAILURE') failures FROM delivery_attempts WHERE provider_id=p.id AND health_observed_at>clock_timestamp()-interval '10 minutes' AND started_at>=coalesce(h.samples_since,'epoch')) s
 ORDER BY p.priority,p.id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var row json.RawMessage
			if err := rows.Scan(&row); err != nil {
				return err
			}
			if err := json.NewEncoder(os.Stdout).Encode(row); err != nil {
				return err
			}
		}
		return rows.Err()
	case "test-provider":
		fs := flag.NewFlagSet(command, flag.ContinueOnError)
		id := fs.String("id", "", "Provider UUID")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("unexpected arguments")
		}
		parsed, err := uuid.Parse(*id)
		if err != nil {
			return errors.New("valid --id required")
		}
		var p provider.Provider
		var seconds int
		err = db.QueryRowContext(ctx, `SELECT id,name,host,port,security,username,password_ciphertext,nonce,timeout_seconds FROM providers WHERE id=$1`, parsed.String()).Scan(&p.ID, &p.Name, &p.Host, &p.Port, &p.Security, &p.Username, &p.Ciphertext, &p.Nonce, &seconds)
		if err != nil {
			return errors.New("provider not found or unavailable")
		}
		if seconds < 1 || seconds > 300 {
			return errors.New("invalid provider timeout")
		}
		p.Timeout = time.Duration(seconds) * time.Second
		box, err := encryption.New(encryption.EnvironmentKey())
		if err != nil {
			return err
		}
		password, err := box.Open(p.ID, p.Ciphertext, p.Nonce)
		if err != nil {
			return err
		}
		out := (smtpclient.Client{Domain: "localhost"}).Probe(ctx, p, password)
		if err := operations.Audit(ctx, db, "PROVIDER_DIAGNOSTIC", "local_cli", map[string]any{"provider_id": p.ID, "status": out.Status, "error_class": out.ErrorClass, "timings": out.Timings}); err != nil {
			return err
		}
		if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"provider_id": p.ID, "status": out.Status, "error_class": out.ErrorClass, "smtp_code": out.Code, "timings": out.Timings, "events": out.Events}); err != nil {
			return err
		}
		if out.Status != "READY" {
			return errors.New("provider diagnostic failed")
		}
		return nil
	case "doctor":
		if len(args) > 0 {
			return errors.New("doctor takes no arguments")
		}
		rows, err := db.QueryContext(ctx, `SELECT status,count(*),min(created_at) FROM messages GROUP BY status ORDER BY status`)
		if err != nil {
			return err
		}
		defer rows.Close()
		states := []map[string]any{}
		for rows.Next() {
			var status string
			var count int64
			var oldest time.Time
			if err := rows.Scan(&status, &count, &oldest); err != nil {
				return err
			}
			states = append(states, map[string]any{"status": status, "count": count, "oldest": oldest.UTC()})
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		var version int
		if err := db.QueryRowContext(ctx, `SELECT coalesce(max(version_id),0) FROM goose_db_version WHERE is_applied`).Scan(&version); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"database": "available", "schema_version": version, "messages": states})
	default:
		return fmt.Errorf("unknown operation: %s", command)
	}
}
