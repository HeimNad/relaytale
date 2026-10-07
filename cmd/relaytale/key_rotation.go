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
)

func runKeyRotation(args []string) (runErr error) {
	f := flag.NewFlagSet("rotate-provider-keys", flag.ContinueOnError)
	offline := f.Bool("offline", false, "assert all runtime and auxiliary processes are stopped")
	apply := f.Bool("apply", false, "commit rotation; default authenticates credentials without changing them")
	actor := f.String("actor", "", "operator identity")
	reason := f.String("reason", "", "audit reason; never include secrets")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || !*offline {
		return errors.New("rotation requires --offline; stop all runtime and auxiliary processes")
	}
	box, err := encryption.NewEnvironment(encryption.EnvironmentKey())
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	db, err := operationDB(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	if err = database.CheckOfflinePrivileges(ctx, db); err != nil {
		return err
	}
	guard, err := database.AcquireOffline(ctx, db)
	if err != nil {
		return err
	}
	defer func() {
		if err := guard.Close(); err != nil {
			runErr = errors.Join(runErr, errors.New("offline ownership retained; verify original process stopped before release"))
		}
	}()
	ctx, stopOwnership := guard.Context(ctx)
	defer stopOwnership()
	report, err := encryption.RotateProviders(ctx, db, box, *actor, *reason, *apply)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}
