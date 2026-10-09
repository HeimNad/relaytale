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
	"relaytale/internal/metrics"
	"relaytale/internal/operations"
	"relaytale/internal/queue"
	"relaytale/internal/recovery"
	"relaytale/internal/resource"
	"relaytale/internal/smtpclient"
	"relaytale/internal/smtpserver"
	"relaytale/internal/storage"
	"relaytale/internal/webhook"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		// Connection errors can include credentials. Keep startup detail out of logs.
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("relaytale stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) (runErr error) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "check-db-permissions":
			return checkDatabasePrivileges(os.Args[2:])
		case "list-runtime-owner", "release-runtime-owner", "list-auxiliary-owners", "release-auxiliary-owner":
			return runRuntimeOwner(os.Args[1], os.Args[2:])
		case "migrate":
			return runMigrate(os.Args[2:])
		case "rotate-provider-keys":
			return runKeyRotation(os.Args[2:])
		case "backup", "restore", "verify-restore":
			return runRecovery(os.Args[1], os.Args[2:])
		case "feedback-collection-status":
			return feedbackCollectionStatus(os.Args[2:])
		case "collect-feedback":
			return collectFeedback(os.Args[2:])
		case "hash-web-password":
			return hashWebPassword(os.Args[2:])
		case "license":
			fmt.Print(project.LicenseText)
			return nil
		case "publish-template", "list-templates", "retire-template", "capacity-report", "preflight-retention", "diagnose-sender-domain", "record-provider-evidence", "list-provider-evidence", "create-provider-quota-group", "configure-provider-quota-group", "attach-provider-quota-group", "list-provider-quota-groups", "list-webhooks", "pause-webhook", "replay-webhook", "find-feedback-candidates", "explain-feedback", "inspect-feedback", "import-feedback", "list-feedback", "show-feedback", "dismiss-feedback", "scan-archives", "list-archive-observations", "reclaim-archive", "inventory-eml", "preflight-eml", "add-suppression", "release-suppression", "list-suppressions", "resolve-unknown", "export-records", "cleanup", "list-providers", "test-provider", "doctor":
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
	if err := recovery.CheckStartup(cfg.StorageDir); err != nil {
		return err
	}
	resources := resource.New(cfg.MemoryBudget, cfg.SpoolBudget)
	if cfg.SnapshotDir != "" {
		if err := os.MkdirAll(cfg.SnapshotDir, 0700); err != nil {
			return errors.New("cannot create snapshot directory")
		}
		if err := storage.CheckWritable(cfg.SnapshotDir); err != nil {
			return errors.New("snapshot directory is not writable")
		}
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
	if err := database.CheckSchema(startup, db); err != nil {
		return errors.New("database schema unavailable, busy or incompatible; stop all writers and run relaytale migrate --offline using this binary")
	}

	if err := database.CheckRuntimePrivileges(startup, db); err != nil {
		return err
	}

	guard, err := database.AcquireRuntime(startup, db)
	if err != nil {
		return errors.New("runtime ownership unavailable; inspect list-runtime-owner before any takeover")
	}
	active := false
	defer func() {
		if active && runErr != nil {
			guard.Retain()
			return
		}
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if err := guard.Release(cleanup); err != nil {
			runErr = errors.Join(runErr, errors.New("runtime ownership retained; verify stopped processes before release"))
		}
	}()

	// Bind all listeners before reporting readiness. Missing TLS fails closed.
	httpListener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("HTTP listen: %w", err)
	}
	defer httpListener.Close()
	var adminBox *encryption.Box
	if cfg.AdminAPIKeys != "" || cfg.WebUIOrigin != "" {
		adminBox, err = encryption.NewEnvironment(cfg.MasterKey)
		if err != nil {
			return err
		}
	}
	admin, err := (api.Management{DB: db, Box: adminBox}).Handler(cfg.AdminAPIKeys, api.WebConfig{Origin: cfg.WebUIOrigin, Users: cfg.WebUIUsers})
	if err != nil {
		return err
	}
	send, err := api.NewSendHandler(startup, cfg.SendAPIKeys, message.APIReceiver{Store: storage.LocalStore{Root: cfg.StorageDir, MaxBytes: cfg.MaxMessageBytes}, Repo: message.Postgres{DB: db}, Resources: resources, SnapshotDir: cfg.SnapshotDir, MaxBytes: cfg.MaxMessageBytes})
	if err != nil {
		return err
	}
	endpoints, err := webhook.Parse(cfg.WebhookEndpoints)
	if err != nil {
		return err
	}
	if err = webhook.Sync(startup, db, endpoints); err != nil {
		return errors.New("webhook configuration failed")
	}
	srv := &http.Server{Handler: api.Handler(db, func() error {
		select {
		case <-guard.Lost():
			return database.ErrOwnerLost
		default:
			return storage.CheckWritable(cfg.StorageDir)
		}
	}, &metrics.Handler{DB: db, Resources: resources, Token: cfg.MetricsToken}, admin, send), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
	result := make(chan error, 2)
	var workerBox *encryption.Box
	if cfg.WorkerCount > 0 {
		workerBox, err = encryption.NewEnvironment(cfg.MasterKey)
		if err != nil {
			return err
		}
	}
	select {
	case <-guard.Lost():
		return database.ErrOwnerLost
	default:
	}
	var shutdownSMTP func(context.Context) error
	var closeSMTP func() error
	if cfg.SMTPAddr != "" {
		cert, err := tls.LoadX509KeyPair(cfg.SMTPCert, cfg.SMTPKey)
		if err != nil {
			return errors.New("SMTP TLS certificate/key required; configure SMTP_TLS_CERT and SMTP_TLS_KEY")
		}
		accounts, err := auth.NewAccountsWithConcurrency(db, cfg.AuthConcurrency)
		if err != nil {
			return err
		}
		receiver := message.Receiver{Resources: resources, Store: storage.LocalStore{Root: cfg.StorageDir, MaxBytes: cfg.MaxMessageBytes}, Repo: message.Postgres{DB: db}}
		smtp := smtpserver.New(&smtpserver.Backend{Accounts: accounts, Receiver: receiver, Log: log, Timeout: 60 * time.Second}, cfg.SMTPDomain, cert, cfg.MaxMessageBytes, smtpserver.Limits{Connections: cfg.SMTPMaxConnections, PerSource: cfg.SMTPMaxConnectionsPerIP, AuthPerMinute: cfg.SMTPAuthPerMinute, AuthBurst: cfg.SMTPAuthBurst, Lifetime: time.Duration(cfg.SMTPMaxSessionSeconds) * time.Second})
		listener, err := net.Listen("tcp", cfg.SMTPAddr)
		if err != nil {
			return fmt.Errorf("SMTP listen: %w", err)
		}
		defer listener.Close()
		shutdownSMTP = smtp.Shutdown
		closeSMTP = smtp.Close
		active = true
		go func() { result <- smtp.Serve(listener) }()
	}
	active = true
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
		worker := queue.Worker{Resources: resources, SnapshotDir: cfg.SnapshotDir, Repo: queue.Repository{DB: db, RetryEnabled: cfg.RetryEnabled, FailoverEnabled: cfg.FailoverEnabled, HealthEnabled: cfg.HealthEnabled}, Box: workerBox, Sender: smtpclient.Client{Domain: cfg.SMTPDomain}, StorageRoot: cfg.StorageDir, MaxBytes: cfg.MaxMessageBytes, Log: log}
		go func() { defer close(workersDone); worker.Run(claimCtx, operationCtx, cfg.WorkerCount) }()
	} else {
		close(workersDone)
	}
	webhookCtx, stopWebhooks := context.WithCancel(context.Background())
	defer stopWebhooks()
	webhookDone := make(chan struct{})
	if cfg.WebhookEnabled {
		go func() {
			defer close(webhookDone)
			(webhook.Worker{DB: db, Endpoints: endpoints, Log: log}).Run(webhookCtx)
		}()
	} else {
		close(webhookDone)
	}
	log.Info("relaytale started", "http_address", cfg.HTTPAddr, "smtp_address", cfg.SMTPAddr, "workers", cfg.WorkerCount, "phase", "6", "automatic_retry", cfg.RetryEnabled, "automatic_failover", cfg.FailoverEnabled, "provider_health", cfg.HealthEnabled)
	var serveErr error
	select {
	case serveErr = <-result:
	case <-guard.Lost():
		serveErr = database.ErrOwnerLost
	case <-ctx.Done():
	}
	stopClaims()
	stopWebhooks()
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

	select {
	case <-webhookDone:
	case <-shutdown.Done():
		return errors.New("webhook shutdown timed out")
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("listener stopped: %w", serveErr)
	}
	return errors.Join(httpErr, smtpErr)
}
