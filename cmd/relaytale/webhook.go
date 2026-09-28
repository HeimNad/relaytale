package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"relaytale/internal/webhook"
	"time"
)

func runWebhook(ctx context.Context, db *sql.DB, command string, args []string) error {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	if command == "list-webhooks" {
		after := fs.Int64("after-sequence", 0, "exclusive sequence cursor")
		limit := fs.Int("limit", 100, "1..100 records")
		if e := fs.Parse(args); e != nil {
			return e
		}
		if fs.NArg() != 0 || *after < 0 || *limit < 1 || *limit > 100 {
			return errors.New("invalid webhook list bounds")
		}
		rows, e := db.QueryContext(ctx, `SELECT o.sequence,o.endpoint_id,o.event_id,o.state,o.attempts,o.generation,o.next_at,e.enabled FROM webhook_outbox o JOIN webhook_endpoints e ON e.id=o.endpoint_id WHERE o.sequence>$1 ORDER BY o.sequence LIMIT $2`, *after, *limit)
		if e != nil {
			return errors.New("webhook listing failed")
		}
		defer rows.Close()
		type item struct {
			Sequence   int64     `json:"sequence"`
			EndpointID string    `json:"endpoint_id"`
			EventID    string    `json:"event_id"`
			State      string    `json:"state"`
			Attempts   int       `json:"attempts"`
			Generation int       `json:"generation"`
			NextAt     time.Time `json:"next_at"`
			Enabled    bool      `json:"endpoint_enabled"`
		}
		out := []item{}
		for rows.Next() {
			var v item
			if e = rows.Scan(&v.Sequence, &v.EndpointID, &v.EventID, &v.State, &v.Attempts, &v.Generation, &v.NextAt, &v.Enabled); e != nil {
				return e
			}
			out = append(out, v)
		}
		if e = rows.Err(); e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	actor := fs.String("actor", "", "operator audit label")
	reason := fs.String("reason", "", "required audit reason")
	if command == "pause-webhook" {
		id := fs.String("endpoint-id", "", "endpoint UUID")
		if e := fs.Parse(args); e != nil {
			return e
		}
		if fs.NArg() != 0 {
			return errors.New("unexpected arguments")
		}
		return webhook.Pause(ctx, db, *id, *actor, *reason)
	}
	sequence := fs.Int64("sequence", 0, "notification sequence")
	generation := fs.Int("expected-generation", -1, "current generation from list-webhooks")
	if e := fs.Parse(args); e != nil {
		return e
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	return webhook.Replay(ctx, db, *sequence, *generation, *actor, *reason)
}
