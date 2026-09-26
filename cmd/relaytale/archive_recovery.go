package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"os"

	"relaytale/internal/operations"
)

func runArchiveRecovery(ctx context.Context, db *sql.DB, command string, args []string) error {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	root := fs.String("storage-dir", os.Getenv("EML_STORAGE_DIR"), "archive root")
	var run func() (any, error)
	switch command {
	case "scan-archives":
		from := fs.String("from", "", "first UTC date YYYY-MM-DD")
		to := fs.String("to", "", "last UTC date YYYY-MM-DD")
		cursor := fs.String("cursor", "", "opaque continuation cursor")
		limit := fs.Int("limit", 1000, "maximum entries per page, 1..10000")
		record := fs.Bool("record", false, "persist immutable orphan observations; never deletes")
		run = func() (any, error) {
			return operations.ScanArchives(ctx, db, *root, *from, *to, *cursor, *limit, *record)
		}
	case "list-archive-observations":
		after := fs.String("after-id", "", "last observation UUID from previous page")
		limit := fs.Int("limit", 100, "maximum records, 1..1000")
		run = func() (any, error) { return operations.ListArchiveObservations(ctx, db, *root, *after, *limit) }
	case "reclaim-archive":
		id := fs.String("observation-id", "", "persisted observation UUID")
		actor := fs.String("actor", "", "operator identity")
		reason := fs.String("reason", "", "reason for reclaim")
		apply := fs.Bool("apply", false, "retire identity and remove verified unreferenced archive")
		run = func() (any, error) { return operations.ReclaimArchive(ctx, db, *root, *id, *actor, *reason, *apply) }
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected archive recovery arguments")
	}
	if *root == "" {
		*root = "data/eml"
	}
	report, err := run()
	if e := json.NewEncoder(os.Stdout).Encode(report); e != nil {
		return e
	}
	if err != nil {
		return errors.New("archive operation incomplete: " + err.Error())
	}
	return nil
}
