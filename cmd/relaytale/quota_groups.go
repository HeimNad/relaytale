package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"os"

	"github.com/google/uuid"
	"relaytale/internal/provider"
)

func runQuotaGroup(ctx context.Context, db *sql.DB, command string, args []string) error {
	if command == "list-provider-quota-groups" {
		fs := flag.NewFlagSet(command, flag.ContinueOnError)
		after := fs.String("after-id", "00000000-0000-0000-0000-000000000000", "exclusive group UUID")
		limit := fs.Int("limit", 100, "page size 1..1000")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if _, err := uuid.Parse(*after); err != nil || fs.NArg() != 0 || *limit < 1 || *limit > 1000 {
			return errors.New("invalid group pagination")
		}
		rows, err := db.QueryContext(ctx, `SELECT to_jsonb(g) FROM provider_quota_group_usage g WHERE g.id>$1 ORDER BY g.id LIMIT $2`, *after, *limit)
		if err != nil {
			return errors.New("cannot list provider quota groups")
		}
		defer rows.Close()
		for rows.Next() {
			var raw json.RawMessage
			if err = rows.Scan(&raw); err != nil {
				return err
			}
			if err = json.NewEncoder(os.Stdout).Encode(raw); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	id := fs.String("id", "", "group UUID; provider UUID for attach")
	expected := fs.Int64("expected-revision", 0, "current revision for configure or attach")
	actor := fs.String("actor", "", "operator identity")
	reason := fs.String("reason", "", "required audit reason")
	var s provider.QuotaGroupSettings
	var gid string
	if command == "attach-provider-quota-group" {
		fs.StringVar(&gid, "group-id", "", "target group UUID")
	} else {
		fs.StringVar(&s.Name, "name", "", "group name")
		fs.IntVar(&s.HourlyLimit, "hourly-limit", 0, "rolling hour recipient attempts; 0 unlimited")
		fs.IntVar(&s.DailyLimit, "daily-limit", 0, "rolling 24h recipient attempts; 0 unlimited")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected quota group arguments")
	}
	var rev int64
	var err error
	if command == "attach-provider-quota-group" {
		rev, err = provider.AttachQuotaGroup(ctx, db, *id, gid, *expected, *actor, *reason)
	} else {
		create := command == "create-provider-quota-group"
		if create && *id == "" {
			*id = uuid.Must(uuid.NewV7()).String()
		}
		rev, err = provider.ManageQuotaGroup(ctx, db, create, *id, *expected, s, *actor, *reason)
	}
	if err != nil {
		return errors.New("quota group operation failed; check IDs, revision, settings and membership; inspect current state before retrying")
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"id": *id, "revision": rev})
}
