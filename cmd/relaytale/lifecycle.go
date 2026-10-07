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

func runLifecycle(ctx context.Context, db *sql.DB, args []string) error {
	fs := flag.NewFlagSet("preflight-retention", flag.ContinueOnError)
	p := operations.LifecycleOptions{}
	fs.IntVar(&p.EMLDays, "eml-days", 0, "proposed EML retention days; 0 disables candidates")
	fs.IntVar(&p.Limit, "limit", 100, "maximum messages per snapshot (1..1000)")
	fs.StringVar(&p.After, "after-id", "", "exclusive message UUID cursor")
	asOf := fs.String("as-of", "", "RFC3339 evaluation time; reuse report as_of on subsequent pages")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected retention arguments")
	}
	if *asOf != "" {
		var err error
		p.AsOf, err = time.Parse(time.RFC3339Nano, *asOf)
		if err != nil {
			return errors.New("invalid --as-of")
		}
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	report, err := operations.LifecyclePreflight(bounded, db, p)
	if err != nil {
		return errors.New("retention preflight incomplete; verify schema, cursor and bounds; no deletion authorized")
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}
