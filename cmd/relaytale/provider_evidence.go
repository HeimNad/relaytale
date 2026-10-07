package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"time"

	"relaytale/internal/provider"
)

func runProviderEvidence(ctx context.Context, db *sql.DB, command string, args []string) error {
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	id := f.String("id", "", "Provider UUID")
	if command == "list-provider-evidence" {
		limit := f.Int("limit", 100, "newest records (1..1000)")
		if err := f.Parse(args); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return errors.New("unexpected arguments")
		}
		records, err := provider.EvidenceHistory(ctx, db, *id, *limit)
		if err != nil {
			return errors.New("provider evidence unavailable; check ID, limit and database")
		}
		return json.NewEncoder(os.Stdout).Encode(records)
	}
	revision := f.Int64("expected-revision", 0, "current Provider configuration revision")
	topic := f.String("topic", "", "max_message_bytes, max_recipients, smtputf8, 8bitmime, provider_dkim_signing")
	value := f.String("value", "", "canonical positive integer or supported/unsupported/unknown")
	source := f.String("source", "operator_report", "operator_report or provider_documentation")
	reference := f.String("source-url", "", "HTTPS reference without credentials, query or fragment; never fetched")
	observed := f.String("observed-at", "", "RFC3339 observation time (required)")
	actor := f.String("actor", "", "operator identity")
	reason := f.String("reason", "", "audit reason; no secrets")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	at, err := time.Parse(time.RFC3339, *observed)
	if err != nil {
		return errors.New("observed-at must be RFC3339")
	}
	err = provider.RecordEvidence(ctx, db, *id, *revision, provider.Evidence{Topic: *topic, Value: *value, Source: *source, SourceURL: *reference, ObservedAt: at.UTC()}, *actor, *reason)
	if errors.Is(err, provider.ErrConflict) {
		return err
	}
	if err != nil {
		return errors.New("evidence not confirmed; check fields, revision and database")
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"recorded": true, "trust": "operator_claim", "affects_delivery": false})
}
