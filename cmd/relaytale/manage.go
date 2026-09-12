package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"relaytale/internal/auth"
	"relaytale/internal/database"
	"relaytale/internal/devtls"
)

func manage(args []string) error {
	switch args[0] {
	case "create-provider":
		return createProvider(args[1:])
	case "init-dev-tls":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		dir := fs.String("dir", "/data/tls", "output directory (localhost only)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("unexpected arguments")
		}
		if err := devtls.Generate(*dir); err != nil {
			return fmt.Errorf("generate development TLS: %w", err)
		}
		fmt.Fprintln(os.Stdout, "Localhost development certificate created. Trust smtp.crt in your test client.")
		return nil
	case "create-smtp-account":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		username := fs.String("username", "", "unique SMTP username")
		senders := fs.String("allowed-from", "", "comma-separated exact sender addresses (required)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 || *username == "" || len(*username) > 128 || strings.TrimSpace(*username) != *username || *senders == "" {
			return errors.New("username and allowed-from are required")
		}
		allowed := strings.Split(*senders, ",")
		for i, v := range allowed {
			a, err := auth.CanonicalAddress(strings.TrimSpace(v))
			if err != nil {
				return err
			}
			allowed[i] = a
		}
		raw := make([]byte, 24)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		password := base64.RawURLEncoding.EncodeToString(raw)
		hash, err := auth.Hash(password)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		url := os.Getenv("DATABASE_URL")
		if url == "" {
			return errors.New("DATABASE_URL is required")
		}
		db, err := database.Open(ctx, url)
		if err != nil {
			return errors.New("database connection failed")
		}
		defer db.Close()
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		_, err = db.ExecContext(ctx, `INSERT INTO smtp_accounts(id,username,password_hash,allowed_from) VALUES($1,$2,$3,$4)`, id.String(), *username, hash, allowed)
		if err != nil {
			return errors.New("account creation failed; check migrations and duplicate username")
		}
		fmt.Fprintf(os.Stdout, "Username: %s\nPassword: %s\nSave this password now; it cannot be retrieved later.\n", *username, password)
		return nil
	default:
		return errors.New("unknown management command")
	}
}
