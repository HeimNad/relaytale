package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"go.yaml.in/yaml/v3"
	"mailgateway/internal/encryption"
)

type Config struct {
	RetryEnabled        bool          `yaml:"retry_enabled"`
	MaintenanceInterval time.Duration `yaml:"maintenance_interval"`
	EMLRetentionDays    int           `yaml:"eml_retention_days"`
	DebugRetentionDays  int           `yaml:"debug_retention_days"`
	CleanupBatch        int           `yaml:"cleanup_batch"`

	WorkerCount     int    `yaml:"worker_count"`
	MasterKey       string `yaml:"-"`
	SMTPAddr        string `yaml:"smtp_listen_addr"`
	SMTPDomain      string `yaml:"smtp_domain"`
	SMTPCert        string `yaml:"smtp_tls_cert"`
	SMTPKey         string `yaml:"smtp_tls_key"`
	MaxMessageBytes int64  `yaml:"max_message_bytes"`

	HTTPAddr        string        `yaml:"http_listen_addr"`
	DatabaseURL     string        `yaml:"database_url"`
	StorageDir      string        `yaml:"storage_dir"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

// Load applies defaults < YAML < environment < command-line flags.
func Load(args []string) (Config, error) {
	c := Config{EMLRetentionDays: 180, DebugRetentionDays: 30, CleanupBatch: 100, SMTPAddr: ":587", SMTPDomain: "localhost", MaxMessageBytes: 25 * 1024 * 1024, HTTPAddr: ":8080", StorageDir: "data/eml", ShutdownTimeout: 30 * time.Second}
	var file string
	pre := flag.NewFlagSet("gateway", flag.ContinueOnError)
	pre.StringVar(&file, "config", "", "optional YAML configuration file")
	pre.Duration("maintenance-interval", 0, "automatic cleanup interval; 0 disables")
	pre.Int("eml-retention-days", 180, "completed EML retention; 0 disables")
	pre.Int("debug-retention-days", 30, "SMTP debug retention; 0 disables")
	pre.Int("cleanup-batch", 100, "maximum cleanup items per category")
	pre.Bool("retry-enabled", false, "enable same-provider automatic retries (requires validation)")
	pre.Int("workers", 0, "delivery workers; 0 disables outbound delivery")
	pre.String("smtp-addr", "", "SMTP listen address; empty disables SMTP")
	pre.String("smtp-domain", "", "SMTP greeting domain")
	pre.String("smtp-cert", "", "SMTP TLS certificate file")
	pre.String("smtp-key", "", "SMTP TLS key file")
	pre.Int64("max-message-bytes", c.MaxMessageBytes, "maximum raw message bytes")
	pre.String("http-addr", "", "HTTP listen address")
	pre.String("database-url", "", "PostgreSQL connection URL")
	pre.String("storage-dir", "", "EML storage directory")
	pre.Duration("shutdown-timeout", c.ShutdownTimeout, "graceful shutdown timeout")
	if err := pre.Parse(args); err != nil {
		return c, err
	}
	if pre.NArg() != 0 {
		return c, errors.New("unexpected positional arguments")
	}
	if file != "" {
		f, err := os.Open(file)
		if err != nil {
			return c, fmt.Errorf("open config: %w", err)
		}
		defer f.Close()
		dec := yaml.NewDecoder(f)
		dec.KnownFields(true)
		if err := dec.Decode(&c); err != nil {
			return c, fmt.Errorf("decode config: %w", err)
		}
	}
	for k, dst := range map[string]*string{"SMTP_LISTEN_ADDR": &c.SMTPAddr, "SMTP_DOMAIN": &c.SMTPDomain, "SMTP_TLS_CERT": &c.SMTPCert, "SMTP_TLS_KEY": &c.SMTPKey, "HTTP_LISTEN_ADDR": &c.HTTPAddr, "DATABASE_URL": &c.DatabaseURL, "EML_STORAGE_DIR": &c.StorageDir} {
		if v, ok := os.LookupEnv(k); ok {
			*dst = v
		}
	}
	if v, ok := os.LookupEnv("SHUTDOWN_TIMEOUT"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, errors.New("invalid SHUTDOWN_TIMEOUT")
		}
		c.ShutdownTimeout = d
	}
	if v, ok := os.LookupEnv("MAX_MESSAGE_BYTES"); ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return c, errors.New("invalid MAX_MESSAGE_BYTES")
		}
		c.MaxMessageBytes = n
	}
	if v, ok := os.LookupEnv("WORKER_COUNT"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return c, errors.New("invalid WORKER_COUNT")
		}
		c.WorkerCount = n
	}
	c.MasterKey = os.Getenv("MAILGATEWAY_MASTER_KEY")
	if raw, ok := os.LookupEnv("RETRY_ENABLED"); ok {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return c, errors.New("invalid RETRY_ENABLED")
		}
		c.RetryEnabled = value
	}
	for key, target := range map[string]*int{"EML_RETENTION_DAYS": &c.EMLRetentionDays, "DEBUG_RETENTION_DAYS": &c.DebugRetentionDays, "CLEANUP_BATCH": &c.CleanupBatch} {
		if raw, ok := os.LookupEnv(key); ok {
			value, err := strconv.Atoi(raw)
			if err != nil {
				return c, fmt.Errorf("invalid %s", key)
			}
			*target = value
		}
	}
	if raw, ok := os.LookupEnv("MAINTENANCE_INTERVAL"); ok {
		value, err := time.ParseDuration(raw)
		if err != nil {
			return c, errors.New("invalid MAINTENANCE_INTERVAL")
		}
		c.MaintenanceInterval = value
	}

	pre.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "retry-enabled":
			c.RetryEnabled, _ = strconv.ParseBool(f.Value.String())
		case "maintenance-interval":
			c.MaintenanceInterval, _ = time.ParseDuration(f.Value.String())
		case "eml-retention-days":
			c.EMLRetentionDays, _ = strconv.Atoi(f.Value.String())
		case "debug-retention-days":
			c.DebugRetentionDays, _ = strconv.Atoi(f.Value.String())
		case "cleanup-batch":
			c.CleanupBatch, _ = strconv.Atoi(f.Value.String())
		case "workers":
			c.WorkerCount, _ = strconv.Atoi(f.Value.String())
		case "smtp-addr":
			c.SMTPAddr = f.Value.String()
		case "smtp-domain":
			c.SMTPDomain = f.Value.String()
		case "smtp-cert":
			c.SMTPCert = f.Value.String()
		case "smtp-key":
			c.SMTPKey = f.Value.String()
		case "max-message-bytes":
			c.MaxMessageBytes, _ = strconv.ParseInt(f.Value.String(), 10, 64)

		case "http-addr":
			c.HTTPAddr = f.Value.String()
		case "database-url":
			c.DatabaseURL = f.Value.String()
		case "storage-dir":
			c.StorageDir = f.Value.String()
		case "shutdown-timeout":
			c.ShutdownTimeout, _ = time.ParseDuration(f.Value.String())
		}
	})
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	if c.HTTPAddr == "" || c.StorageDir == "" || c.ShutdownTimeout <= 0 {
		return c, errors.New("listen address, storage directory and positive shutdown timeout are required")
	}
	if c.MaxMessageBytes <= 0 || c.MaxMessageBytes > 1024*1024*1024 {
		return c, errors.New("message size must be between 1 byte and 1 GiB")
	}
	if c.SMTPDomain == "" {
		return c, errors.New("SMTP domain is required")
	}
	if c.WorkerCount < 0 || c.WorkerCount > 32 {
		return c, errors.New("WORKER_COUNT must be between 0 and 32")
	}
	if c.WorkerCount > 0 {
		if _, err := encryption.New(c.MasterKey); err != nil {
			return c, err
		}
	}
	if c.MaintenanceInterval < 0 || (c.MaintenanceInterval > 0 && c.MaintenanceInterval < time.Minute) {
		return c, errors.New("maintenance interval must be 0 or at least 1m")
	}
	if c.EMLRetentionDays < 0 || c.DebugRetentionDays < 0 || c.EMLRetentionDays > 36500 || c.DebugRetentionDays > 36500 || c.CleanupBatch < 1 || c.CleanupBatch > 1000 {
		return c, errors.New("invalid retention days or cleanup batch")
	}
	return c, nil
}
