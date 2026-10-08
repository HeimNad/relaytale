package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"time"

	"relaytale/internal/api"
)

func runTemplates(ctx context.Context, db *sql.DB, command string, args []string) error {
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	id := f.String("api-key-id", "", "existing website credential UUID")
	actor, reason := new(string), new(string)
	if command == "publish-template" {
		actor = f.String("actor", "", "required operator audit identity")
		reason = f.String("reason", "", "required audit reason without secrets")
	}
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected template arguments")
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if command == "list-templates" {
		items, err := api.ListPublishedTemplates(bounded, db, *id)
		if err != nil {
			return errors.New("template listing unavailable; verify identity and database")
		}
		return json.NewEncoder(os.Stdout).Encode(items)
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, (32<<10)+1))
	if err != nil {
		return errors.New("cannot read reviewed template")
	}
	replayed, err := api.PublishTemplate(bounded, db, *id, raw, *actor, *reason)
	if err != nil {
		return errors.New("template publication not confirmed; verify identity, artifact, bounds and database; retry the same artifact")
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"published": true, "replayed": replayed, "activation": "explicit_identity_opt_in_and_restart"})
}
