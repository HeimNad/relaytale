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

func runTemplateDrafts(ctx context.Context, db *sql.DB, command string, args []string) error {
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	identity := f.String("api-key-id", "", "website identity UUID")
	var name, version, actor, reason *string
	var revision *int64
	switch command {
	case "save-template-draft":
		revision = f.Int64("expected-revision", -1, "0 for create, exact current revision for edit")
	case "show-template-draft", "publish-template-draft":
		name = f.String("id", "", "template ID")
		revision = f.Int64("revision", 0, "exact reviewed draft revision")
	case "list-template-drafts":
	default:
		return errors.New("unknown template draft command")
	}
	if command == "save-template-draft" || command == "publish-template-draft" {
		actor = f.String("actor", "", "operator audit identity")
		reason = f.String("reason", "", "audit reason without secrets")
	}
	if command == "publish-template-draft" {
		version = f.String("version", "", "reviewed content version")
	}
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected draft arguments")
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var result any
	var err error
	switch command {
	case "list-template-drafts":
		result, err = api.ListTemplateDrafts(bounded, db, *identity)
	case "show-template-draft":
		result, err = api.GetTemplateDraft(bounded, db, *identity, *name, *revision)
	case "save-template-draft":
		var raw []byte
		raw, err = io.ReadAll(io.LimitReader(os.Stdin, (32<<10)+1))
		if err != nil {
			return errors.New("cannot read template draft")
		}
		var next int64
		var replayed bool
		next, replayed, err = api.SaveTemplateDraft(bounded, db, *identity, *revision, raw, *actor, *reason)
		result = map[string]any{"saved": true, "revision": next, "replayed": replayed, "activation": "draft_only"}
	case "publish-template-draft":
		var replayed bool
		replayed, err = api.PublishTemplateDraft(bounded, db, *identity, *name, *revision, *version, *actor, *reason)
		result = map[string]any{"published": true, "replayed": replayed, "activation": "explicit_identity_opt_in_and_restart"}
	}
	if errors.Is(err, api.ErrTemplateDraftConflict) {
		return errors.New("draft changed; export and review the current revision before retrying")
	}
	if err != nil {
		return errors.New("template draft operation not confirmed; verify identity, revision, content, limits and database")
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
