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
)

func runMigrate(args []string) error {
	f := flag.NewFlagSet("migrate", flag.ContinueOnError)
	offline := f.Bool("offline", false, "assert all application, collection and maintenance processes are stopped")
	if err := f.Parse(args); err != nil {
		return err
	}
	if !*offline || f.NArg() != 0 {
		return errors.New("migration requires --offline; stop all writers and senders first")
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return errors.New("DATABASE_URL required for migration")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	db, err := database.Open(ctx, url)
	if err != nil {
		return errors.New("migration database unavailable")
	}
	defer db.Close()
	if err := database.CheckOfflinePrivileges(ctx, db); err != nil {
		return err
	}
	if err = database.Migrate(ctx, db); err != nil {
		return errors.New("migration failed; keep writers stopped and inspect schema before retrying; no automatic downgrade")
	}
	if err = database.CheckSchema(ctx, db); err != nil {
		return errors.New("migration schema verification failed; keep writers stopped")
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]string{"operation": "migrate", "result": "complete", "next": "verify configuration and start one instance"})
}
