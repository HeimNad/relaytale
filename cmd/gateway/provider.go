package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"mailgateway/internal/database"
	"mailgateway/internal/encryption"
	"mailgateway/internal/provider"
)

func createProvider(args []string) error {
	fs := flag.NewFlagSet("create-provider", flag.ContinueOnError)
	name := fs.String("name", "", "provider name")
	host := fs.String("host", "", "SMTP hostname")
	port := fs.Int("port", 587, "SMTP port")
	security := fs.String("security", "starttls", "starttls or implicit_tls")
	username := fs.String("username", "", "provider SMTP username")
	domains := fs.String("from-domains", "", "comma-separated sender domains")
	hourly := fs.Int("hourly-limit", 0, "rolling hour recipient attempts; 0 unlimited")
	daily := fs.Int("daily-limit", 0, "rolling 24h recipient attempts; 0 unlimited")
	priority := fs.Int("priority", 10, "lower is preferred")
	connections := fs.Int("max-connections", 1, "maximum concurrent submissions")
	timeout := fs.Duration("timeout", 30*time.Second, "total SMTP operation timeout")
	stdin := fs.Bool("password-stdin", false, "read password from stdin (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || !*stdin {
		return errors.New("use --password-stdin to supply the provider password")
	}
	box, err := encryption.New(os.Getenv("MAILGATEWAY_MASTER_KEY"))
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 4098))
	if err != nil {
		return errors.New("cannot read provider password")
	}
	password := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
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
	id, err := provider.Create(ctx, db, box, provider.Provider{Name: *name, Host: *host, Port: *port, Security: *security, Username: *username, Priority: *priority, MaxConnections: *connections, Timeout: *timeout, HourlyLimit: *hourly, DailyLimit: *daily}, password, strings.Split(*domains, ","))
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Provider created: %s\n", id)
	return nil
}
