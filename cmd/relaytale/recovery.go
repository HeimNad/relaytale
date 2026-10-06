package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"relaytale/internal/database"
	"relaytale/internal/encryption"
	"relaytale/internal/recovery"
)

func runRecovery(name string, args []string) (runErr error) {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	root := f.String("storage-dir", os.Getenv("EML_STORAGE_DIR"), "absolute EML storage directory (required)")
	bundle := f.String("bundle", "", "private backup directory")
	offline := f.Bool("offline", false, "assert all application, collection and maintenance processes are stopped")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || *root == "" || !*offline {
		return errors.New("recovery requires --storage-dir and --offline; stop all writers and senders first")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return errors.New("DATABASE_URL required")
	}
	db, err := database.Open(ctx, url)
	if err != nil {
		return errors.New("recovery database unavailable")
	}
	defer db.Close()
	guard, err := database.AcquireOffline(ctx, db)
	if err != nil {
		return errors.New("recovery blocked by runtime ownership or another offline operation")
	}
	defer func() {
		if err := guard.Close(); err != nil {
			runErr = errors.Join(runErr, errors.New("offline ownership retained; confirm original process stopped before release"))
		}
	}()
	ctx, stopOwnership := guard.Context(ctx)
	defer stopOwnership()
	o := recovery.Options{DB: db, DatabaseURL: url, StorageRoot: *root, Bundle: *bundle, Offline: *offline}
	switch name {
	case "backup":
		err = recovery.Backup(ctx, o)
	case "restore":
		err = recovery.Restore(ctx, o)
	case "verify-restore":
		var box *encryption.Box
		if key := encryption.EnvironmentKey(); key != "" {
			box, err = encryption.New(key)
			if err != nil {
				return err
			}
		}
		var result recovery.Report
		result, err = recovery.VerifyRestored(ctx, db, *root, box)
		if err == nil {
			return json.NewEncoder(os.Stdout).Encode(result)
		}
	default:
		return errors.New("unknown recovery command")
	}
	// SQL/OS errors may contain private paths or data. Never print raw driver errors.
	if err != nil {
		return errors.New("recovery failed; keep all writers stopped and the restore gate intact; check bundle, paths, key, tools and database")
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]string{"operation": name, "result": "complete", "next": "verify and reconcile before manual startup gate release"})
}
