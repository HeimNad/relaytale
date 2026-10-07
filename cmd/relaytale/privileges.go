package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"relaytale/internal/database"
)

// Permission inspection is a read-only control command, usable before startup.
func checkDatabasePrivileges(args []string) error {
	if len(args) != 0 {
		return errors.New("check-db-permissions takes no arguments")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := operationDB(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	if err = database.CheckSchema(ctx, db); err != nil {
		return errors.New("schema unavailable; migrate offline first")
	}
	if err = database.CheckRuntimePrivileges(ctx, db); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]string{"runtime_permissions": "restricted", "schema": "compatible"})
}
