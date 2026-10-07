package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"net"
	"os"

	"relaytale/internal/operations"
	"relaytale/internal/provider"
)

func runDomainDiagnostic(ctx context.Context, db *sql.DB, args []string) error {
	f := flag.NewFlagSet("diagnose-sender-domain", flag.ContinueOnError)
	domain := f.String("domain", "", "ASCII sender domain (required)")
	selector := f.String("selector", "", "optional DKIM selector")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	report, err := provider.DiagnoseDomain(ctx, net.DefaultResolver, *domain, *selector)
	if err != nil {
		return errors.New("invalid domain or selector")
	}
	if err = operations.Audit(ctx, db, "SENDER_DOMAIN_DIAGNOSTIC", "local_cli", report); err != nil {
		return errors.New("diagnostic audit not confirmed")
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}
