package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"os"

	"relaytale/internal/feedback"
)

func runFeedback(ctx context.Context, db *sql.DB, command string, args []string) error {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	s := feedback.Service{DB: db}
	var search feedback.CandidateRequest
	var inspect feedback.InspectRequest
	var v feedback.ImportRequest
	var id, digest, after, path string
	limit := 100
	switch command {
	case "find-feedback-candidates":
		fs.StringVar(&search.FeedbackID, "id", "", "feedback UUID")
		fs.StringVar(&search.ExpectedSHA256, "expected-sha256", "", "evidence digest")
		fs.StringVar(&search.RFCMessageID, "rfc-message-id", "", "exact original Message-ID search hint")
		fs.IntVar(&search.ReportIndex, "report-index", 0, "strict recipient report index")
		fs.IntVar(&search.Limit, "limit", 20, "candidate cap 1..50")
	case "explain-feedback":
		fs.StringVar(&id, "id", "", "feedback UUID")
		fs.StringVar(&digest, "expected-sha256", "", "digest of evidence")
	case "inspect-feedback":
		fs.StringVar(&inspect.FeedbackID, "id", "", "feedback UUID")
		fs.StringVar(&inspect.ExpectedSHA256, "expected-sha256", "", "digest of inspected evidence")
		fs.StringVar(&inspect.MessageID, "message-id", "", "candidate Gateway message UUID")
		fs.StringVar(&inspect.ProviderID, "provider-id", "", "candidate provider UUID, not source authentication")
		fs.StringVar(&inspect.AttemptID, "attempt-id", "", "candidate attempt UUID")
		fs.StringVar(&inspect.RecipientID, "recipient-id", "", "candidate recipient UUID")
		fs.IntVar(&inspect.ReportIndex, "report-index", 0, "zero-based recipient block in report")
	case "import-feedback":
		fs.StringVar(&path, "file", "", "local raw DSN file (at most 1 MiB); never treated as trusted")
		fs.StringVar(&v.Source, "source", "", "operator-defined source/account label, not authentication")
		fs.StringVar(&v.ExternalID, "external-id", "", "source event ID; defaults to content digest")
		fs.StringVar(&v.Actor, "actor", "", "operator audit label")
		fs.StringVar(&v.Reason, "reason", "", "evidence provenance / import reason")
	case "list-feedback":
		fs.StringVar(&after, "after", "", "UUID cursor")
		fs.IntVar(&limit, "limit", 100, "page size 1..200")
	case "show-feedback":
		fs.StringVar(&id, "id", "", "feedback UUID")
	case "dismiss-feedback":
		fs.StringVar(&id, "id", "", "feedback UUID")
		fs.StringVar(&digest, "expected-sha256", "", "digest of inspected evidence")
		fs.StringVar(&v.Actor, "actor", "", "operator audit label")
		fs.StringVar(&v.Reason, "reason", "", "review reason; does not change delivery or suppression")
	default:
		return errors.New("unknown feedback command")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	var result any
	switch command {
	case "find-feedback-candidates":
		found, err := s.Candidates(ctx, search)
		if err != nil {
			return errors.New("candidate search failed; verify evidence, hint, bounds and database")
		}
		result = found
	case "explain-feedback":
		explanation, err := s.ExplainStored(ctx, id, digest)
		if err != nil {
			return errors.New("feedback explanation unavailable; verify ID, digest and bounded DSN structure")
		}
		result = explanation
	case "inspect-feedback":
		inspection, err := s.Inspect(ctx, inspect)
		if err != nil {
			return errors.New("feedback inspection failed; verify IDs, digest, report index and schema")
		}
		result = inspection
	case "import-feedback":
		f, err := os.Open(path)
		if err != nil {
			return errors.New("cannot open feedback file")
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("feedback must be a regular file")
		}
		entry, err := s.Import(ctx, v, f)
		if err != nil {
			return errors.New("feedback import failed; check bounds, labels, schema and database")
		}
		result = entry
	case "list-feedback":
		entries, err := s.List(ctx, after, limit)
		if err != nil {
			return errors.New("feedback query failed; check cursor, limit and schema")
		}
		next := ""
		if len(entries) == limit {
			next = entries[len(entries)-1].ID
		}
		result = map[string]any{"entries": entries, "next_after": next}
	case "show-feedback":
		entry, err := s.Get(ctx, id)
		if err != nil {
			return errors.New("feedback not found or query failed")
		}
		result = entry
	case "dismiss-feedback":
		if err := s.Dismiss(ctx, id, digest, v.Actor, v.Reason); err != nil {
			return errors.New("feedback dismissal failed; verify ID, digest, audit fields and review state")
		}
		result = map[string]any{"id": id, "review": "DISMISS", "delivery_changed": false, "suppression_changed": false}
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
