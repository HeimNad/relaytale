package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	project "relaytale"
	"relaytale/internal/api"
	"relaytale/internal/auth"
	"relaytale/internal/config"
	"relaytale/internal/database"
	"relaytale/internal/encryption"
	"relaytale/internal/message"
	"relaytale/internal/operations"
	"relaytale/internal/queue"
	"relaytale/internal/smtpclient"
	"relaytale/internal/smtpserver"
	"relaytale/internal/storage"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		// Connection errors can include credentials. Keep startup detail out of logs.
		log.Error("relaytale stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "license":
			fmt.Print(project.LicenseText)
			return nil
		case "resolve-unknown", "export-records", "cleanup", "list-providers", "test-provider", "doctor":
			return runOperation(os.Args[1], os.Args[2:])
		}
	}

	if len(os.Args) > 1 && (os.Args[1] == "init-dev-tls" || os.Args[1] == "create-smtp-account" || os.Args[1] == "create-provider") {
		return manage(os.Args[1:])
	}
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := os.MkdirAll(cfg.StorageDir, 0700); err != nil {
		return errors.New("cannot create EML storage directory")
	}
	if err := storage.CheckWritable(cfg.StorageDir); err != nil {
		return errors.New("EML storage is not writable")
	}
	startup, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	db, err := database.Open(startup, cfg.DatabaseURL)
	if err != nil {
		return errors.New("database connection failed; check DATABASE_URL and PostgreSQL availability")
	}
	defer db.Close()
	if err := database.Migrate(startup, db); err != nil {
		return errors.New("database migration failed; check database schema and migration permissions")
	}

	// Bind all listeners before reporting readiness. Missing TLS fails closed.
	httpListener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("HTTP listen: %w", err)
	}
	defer httpListener.Close()
	srv := &http.Server{Handler: api.Handler(db, func() error { return storage.CheckWritable(cfg.StorageDir) }), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
	result := make(chan error, 2)
	var shutdownSMTP func(context.Context) error
	var closeSMTP func() error
	if cfg.SMTPAddr != "" {
		cert, err := tls.LoadX509KeyPair(cfg.SMTPCert, cfg.SMTPKey)
		if err != nil {
			return errors.New("SMTP TLS certificate/key required; configure SMTP_TLS_CERT and SMTP_TLS_KEY")
		}
		accounts, err := auth.NewAccounts(db)
		if err != nil {
			return err
		}
		receiver := message.Receiver{Store: storage.LocalStore{Root: cfg.StorageDir, MaxBytes: cfg.MaxMessageBytes}, Repo: message.Postgres{DB: db}}
		smtp := smtpserver.New(&smtpserver.Backend{Accounts: accounts, Receiver: receiver, Log: log, Timeout: 60 * time.Second}, cfg.SMTPDomain, cert, cfg.MaxMessageBytes)
		listener, err := net.Listen("tcp", cfg.SMTPAddr)
		if err != nil {
			return fmt.Errorf("SMTP listen: %w", err)
		}
		defer listener.Close()
		shutdownSMTP = smtp.Shutdown
		closeSMTP = smtp.Close
		go func() { result <- smtp.Serve(listener) }()
	}
	go func() { result <- srv.Serve(httpListener) }()
	claimCtx, stopClaims := context.WithCancel(context.Background())
	defer stopClaims()
	operationCtx, stopOperations := context.WithCancel(context.Background())
	defer stopOperations()
	maintenanceCtx, stopMaintenance := context.WithCancel(context.Background())
	defer stopMaintenance()
	maintenanceDone := make(chan struct{})
	go func() {
		defer close(maintenanceDone)
		operations.Run(maintenanceCtx, db, cfg.StorageDir, cfg.MaintenanceInterval, operations.Retention{EMLDays: cfg.EMLRetentionDays, DebugDays: cfg.DebugRetentionDays, Batch: cfg.CleanupBatch}, log)
	}()
	workersDone := make(chan struct{})
	if cfg.WorkerCount > 0 {
		box, err := encryption.New(cfg.MasterKey)
		if err != nil {
			return err
		}
		worker := queue.Worker{Repo: queue.Repository{DB: db, RetryEnabled: cfg.RetryEnabled, FailoverEnabled: cfg.FailoverEnabled, HealthEnabled: cfg.HealthEnabled}, Box: box, Sender: smtpclient.Client{Domain: cfg.SMTPDomain}, StorageRoot: cfg.StorageDir, MaxBytes: cfg.MaxMessageBytes, Log: log}
		go func() { defer close(workersDone); worker.Run(claimCtx, operationCtx, cfg.WorkerCount) }()
	} else {
		close(workersDone)
	}
	log.Info("relaytale started", "http_address", cfg.HTTPAddr, "smtp_address", cfg.SMTPAddr, "workers", cfg.WorkerCount, "phase", "4C", "automatic_retry", cfg.RetryEnabled, "automatic_failover", cfg.FailoverEnabled, "provider_health", cfg.HealthEnabled)
	var serveErr error
	select {
	case serveErr = <-result:
	case <-ctx.Done():
	}
	stopClaims()
	stopMaintenance()
	log.Info("relaytale shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	var wg sync.WaitGroup
	var smtpErr error
	if shutdownSMTP != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			smtpErr = shutdownSMTP(shutdown)
			if smtpErr != nil {
				_ = closeSMTP()
			}
		}()
	}
	httpErr := srv.Shutdown(shutdown)
	if httpErr != nil {
		_ = srv.Close()
	}
	wg.Wait()
	select {
	case <-maintenanceDone:
	case <-shutdown.Done():
		return errors.New("maintenance shutdown timed out")
	}
	select {
	case <-workersDone:
	case <-shutdown.Done():
		stopOperations()
		// Each worker has one final bounded DB write after network cancellation.
		select {
		case <-workersDone:
		case <-time.After(12 * time.Second):
			return errors.New("workers exceeded shutdown deadline")
		}
	}

	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("listener stopped: %w", serveErr)
	}
	return errors.Join(httpErr, smtpErr)
}
