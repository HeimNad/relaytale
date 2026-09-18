package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"relaytale/internal/feedback"
	"relaytale/internal/feedbackimap"
)

func collectFeedback(args []string) error {
	fs := flag.NewFlagSet("collect-feedback", flag.ContinueOnError)
	source := fs.String("source", "", "stable mailbox source name")
	folder := fs.String("folder", "", "explicit mailbox folder (required)")
	port := fs.Int("port", 993, "implicit TLS IMAP port")
	batch := fs.Int("batch", 20, "UID slots per batch (1..100)")
	interval := fs.Duration("interval", 0, "repeat interval; zero runs once, otherwise at least 30s")
	accept := fs.Uint64("accept-uidvalidity", 0, "explicitly acknowledge this new UIDVALIDITY (one-shot only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *source == "" || *batch < 1 || *batch > 100 || *accept > 4294967295 || (*interval != 0 && (*interval < 30*time.Second || *accept != 0)) {
		return errors.New("invalid collection flags; epoch acknowledgement is one-shot only")
	}
	cfg := feedbackimap.Config{Host: os.Getenv("FEEDBACK_IMAP_HOST"), Port: *port, Username: os.Getenv("FEEDBACK_IMAP_USERNAME"), Password: os.Getenv("FEEDBACK_IMAP_PASSWORD"), Folder: *folder}
	if err := cfg.Validate(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := operationDB(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	for {
		result, err := (feedback.Service{DB: db}).Collect(ctx, feedback.CollectOptions{Source: *source, IdentitySHA256: cfg.Identity(), Batch: *batch, AcceptUIDValidity: uint32(*accept)}, func(ctx context.Context) (feedback.Mailbox, error) { return feedbackimap.Dial(ctx, cfg) })
		if e := json.NewEncoder(os.Stdout).Encode(result); e != nil {
			return e
		}
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			if errors.Is(err, feedback.ErrMailboxChanged) || errors.Is(err, feedback.ErrCapacity) || errors.Is(err, feedback.ErrCollectorBusy) {
				return err
			}
			return errors.New("feedback collection failed; saved checkpoints retained; check mailbox access and database availability")
		}
		if *interval == 0 {
			return nil
		}
		timer := time.NewTimer(*interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func feedbackCollectionStatus(args []string) error {
	fs := flag.NewFlagSet("feedback-collection-status", flag.ContinueOnError)
	source := fs.String("source", "", "mailbox source name")
	epoch := fs.Uint64("uidvalidity", 0, "receipt epoch; default current")
	after := fs.Uint64("after-uid", 0, "exclusive receipt UID cursor")
	limit := fs.Int("limit", 20, "receipts per page (1..100)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *epoch > 4294967295 || *after > 4294967295 {
		return errors.New("invalid status arguments")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := operationDB(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	result, err := (feedback.Service{DB: db}).CollectionStatus(ctx, *source, uint32(*epoch), uint32(*after), *limit)
	if err != nil {
		return errors.New("collection status unavailable; check source, bounds and database")
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
