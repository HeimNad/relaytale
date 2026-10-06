package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"time"

	"relaytale/internal/database"
)

func runRuntimeOwner(command string, args []string) error {
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	id := f.String("owner-id", "", "exact observed owner UUID")
	stopped := f.Bool("confirm-stopped", false, "assert this owner and all of its SMTP operations have stopped on every host")
	actor := f.String("actor", "", "operator identity")
	reason := f.String("reason", "", "why this abandoned owner is safe to release")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected ownership arguments")
	}
	if command == "release-runtime-owner" && (!*stopped || *id == "" || *actor == "" || *reason == "") {
		return errors.New("release requires exact owner-id, confirm-stopped, actor and reason; stop the original process and reconcile in-flight sends first")
	}
	if command == "list-runtime-owner" && (*id != "" || *stopped || *actor != "" || *reason != "") {
		return errors.New("listing accepts no release arguments")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return errors.New("DATABASE_URL required")
	}
	db, err := database.Open(ctx, url)
	if err != nil {
		return errors.New("ownership database unavailable")
	}
	defer db.Close()
	if command == "list-runtime-owner" {
		owner, err := database.ListRuntimeOwner(ctx, db)
		if err != nil {
			return errors.New("ownership metadata unavailable; verify schema")
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"owner": owner, "automatic_takeover": false})
	}
	if err = database.ReleaseStaleOwner(ctx, db, *id, *actor, *reason); err != nil {
		return errors.New("ownership release refused; verify exact owner, stopped process, live database session and audit permissions")
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]string{"operation": "release-runtime-owner", "result": "complete", "next": "reconcile UNKNOWN and verify configuration before starting one instance"})
}
