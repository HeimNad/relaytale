package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"time"

	"relaytale/internal/operations"
)

func runCapacity(ctx context.Context, db *sql.DB, args []string) error {
	fs := flag.NewFlagSet("capacity-report", flag.ContinueOnError)
	root := fs.String("storage-dir", os.Getenv("EML_STORAGE_DIR"), "archive root")
	after := fs.String("after-id", "", "exclusive message UUID cursor")
	limit := fs.Int("limit", 100, "maximum records per page (1..1000)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected capacity arguments")
	}
	if *root == "" {
		*root = "data/eml"
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	report, err := operations.Capacity(bounded, db, *root, *after, *limit)
	if err != nil {
		return errors.New("capacity report incomplete; verify schema, root, cursor and bounds; no deletion authorized")
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}
