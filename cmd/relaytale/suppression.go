package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"time"

	"relaytale/internal/suppression"
)

func runSuppression(ctx context.Context, db *sql.DB, command string, args []string) error {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	service := suppression.Service{DB: db}
	switch command {
	case "add-suppression":
		v := suppression.AddRequest{}
		fs.StringVar(&v.Email, "email", "", "bare recipient mailbox; global case-insensitive scope")
		fs.StringVar(&v.Category, "category", "manual", "manual, hard_bounce, complaint, invalid, unsubscribe (operator assertion)")
		fs.StringVar(&v.Actor, "actor", "", "required operator audit label")
		fs.StringVar(&v.Reason, "reason", "", "required reason / evidence reference")
		expires := fs.String("expires-at", "", "optional expiry in RFC3339")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("unexpected arguments")
		}
		if *expires != "" {
			t, err := time.Parse(time.RFC3339Nano, *expires)
			if err != nil {
				return errors.New("invalid --expires-at")
			}
			v.ExpiresAt = &t
		}
		id, err := service.Add(ctx, v)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"id": id, "scope": "gateway_global"})
	case "release-suppression":
		id := fs.String("id", "", "exact suppression UUID; never releases a replacement entry")
		actor := fs.String("actor", "", "required operator audit label")
		reason := fs.String("reason", "", "required release reason")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("unexpected arguments")
		}
		if err := service.Release(ctx, *id, *actor, *reason); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"id": *id, "released": true, "historical_messages_requeued": false})
	case "list-suppressions":
		email := fs.String("email", "", "optional exact mailbox filter, case-insensitive")
		after := fs.String("after", "", "previous page next_after UUID")
		limit := fs.Int("limit", 100, "page size 1..200; includes history")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("unexpected arguments")
		}
		entries, err := service.List(ctx, *email, *after, *limit)
		if err != nil {
			return err
		}
		next := ""
		if len(entries) == *limit {
			next = entries[len(entries)-1].ID
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"entries": entries, "next_after": next})
	default:
		return errors.New("unknown suppression operation")
	}
}
