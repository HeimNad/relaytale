package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
	"relaytale/internal/encryption"
	"relaytale/internal/resource"
)

type Config struct {
	SMTPMaxConnections      int    `yaml:"smtp_max_connections"`
	SMTPMaxConnectionsPerIP int    `yaml:"smtp_max_connections_per_ip"`
	SMTPAuthPerMinute       int    `yaml:"smtp_auth_per_minute"`
	SMTPAuthBurst           int    `yaml:"smtp_auth_burst"`
	SMTPMaxSessionSeconds   int    `yaml:"smtp_max_session_seconds"`
	AuthConcurrency         int    `yaml:"auth_concurrency"`
	AuthMemoryBudget        int    `yaml:"auth_memory_budget_bytes"`
	MetricsToken            string `yaml:"-"`

	MemoryBudget int64  `yaml:"memory_budget_bytes"`
	SpoolBudget  int64  `yaml:"spool_budget_bytes"`
	SnapshotDir  string `yaml:"snapshot_dir"`

	HealthEnabled       bool          `yaml:"health_enabled"`
	FailoverEnabled     bool          `yaml:"failover_enabled"`
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
	c := Config{SMTPMaxConnections: 128, SMTPMaxConnectionsPerIP: 8, SMTPAuthPerMinute: 60, SMTPAuthBurst: 20, SMTPMaxSessionSeconds: 300, AuthConcurrency: 2, AuthMemoryBudget: 128 << 20, MemoryBudget: resource.DefaultMemory, SpoolBudget: resource.DefaultSpool, EMLRetentionDays: 180, DebugRetentionDays: 30, CleanupBatch: 100, SMTPAddr: ":587", SMTPDomain: "localhost", MaxMessageBytes: 25 * 1024 * 1024, HTTPAddr: ":8080", StorageDir: "data/eml", ShutdownTimeout: 30 * time.Second}
	guardInts := map[string]*int{
		"smtp-max-connections":        &c.SMTPMaxConnections,
		"smtp-max-connections-per-ip": &c.SMTPMaxConnectionsPerIP,
		"smtp-auth-per-minute":        &c.SMTPAuthPerMinute,
		"smtp-auth-burst":             &c.SMTPAuthBurst,
		"smtp-max-session-seconds":    &c.SMTPMaxSessionSeconds,
		"auth-concurrency":            &c.AuthConcurrency,
		"auth-memory-budget-bytes":    &c.AuthMemoryBudget,
	}
	var file string
	pre := flag.NewFlagSet("relaytale", flag.ContinueOnError)
	for name, target := range guardInts {
		pre.Int(name, *target, "SMTP admission/authentication bound")
	}
	pre.Int64("memory-budget-bytes", c.MemoryBudget, "active working-set reservation budget")
	pre.Int64("spool-budget-bytes", c.SpoolBudget, "maximum concurrent outbound snapshot bytes")
	pre.String("snapshot-dir", "", "temporary snapshot directory; default OS temp directory")
	pre.StringVar(&file, "config", "", "optional YAML configuration file")
	pre.Duration("maintenance-interval", 0, "automatic cleanup interval; 0 disables")
	pre.Int("eml-retention-days", 180, "completed EML retention; 0 disables")
	pre.Int("debug-retention-days", 30, "SMTP debug retention; 0 disables")
	pre.Int("cleanup-batch", 100, "maximum cleanup items per category")
	pre.Bool("health-enabled", false, "enforce provider circuit breaker")
	pre.Bool("failover-enabled", false, "enable safe provider failover; requires retry-enabled")
	pre.Bool("retry-enabled", false, "enable automatic retries (requires validation)")
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
	for key, target := range map[string]*int64{"MEMORY_BUDGET_BYTES": &c.MemoryBudget, "SPOOL_BUDGET_BYTES": &c.SpoolBudget} {
		if raw, ok := os.LookupEnv(key); ok {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return c, fmt.Errorf("invalid %s", key)
			}
			*target = value
		}
	}
	if raw, ok := os.LookupEnv("SNAPSHOT_DIR"); ok {
		c.SnapshotDir = raw
	}
	c.MasterKey = encryption.EnvironmentKey()
	c.MetricsToken = os.Getenv("METRICS_BEARER_TOKEN")
	for name, target := range guardInts {
		env := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
		if raw, ok := os.LookupEnv(env); ok {
			value, err := strconv.Atoi(raw)
			if err != nil {
				return c, fmt.Errorf("invalid %s", env)
			}
			*target = value
		}
	}
	if raw, ok := os.LookupEnv("RETRY_ENABLED"); ok {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return c, errors.New("invalid RETRY_ENABLED")
		}
		c.RetryEnabled = value
	}
	if raw, ok := os.LookupEnv("HEALTH_ENABLED"); ok {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return c, errors.New("invalid HEALTH_ENABLED")
		}
		c.HealthEnabled = value
	}
	if raw, ok := os.LookupEnv("FAILOVER_ENABLED"); ok {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return c, errors.New("invalid FAILOVER_ENABLED")
		}
		c.FailoverEnabled = value
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
		if target, ok := guardInts[f.Name]; ok {
			*target, _ = strconv.Atoi(f.Value.String())
			return
		}
		switch f.Name {
		case "memory-budget-bytes":
			c.MemoryBudget, _ = strconv.ParseInt(f.Value.String(), 10, 64)
		case "spool-budget-bytes":
			c.SpoolBudget, _ = strconv.ParseInt(f.Value.String(), 10, 64)
		case "snapshot-dir":
			c.SnapshotDir = f.Value.String()

		case "health-enabled":
			c.HealthEnabled, _ = strconv.ParseBool(f.Value.String())
		case "failover-enabled":
			c.FailoverEnabled, _ = strconv.ParseBool(f.Value.String())
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
	if c.SMTPMaxConnections < 1 || c.SMTPMaxConnections > 4096 || c.SMTPMaxConnectionsPerIP < 1 || c.SMTPMaxConnectionsPerIP > c.SMTPMaxConnections || c.SMTPAuthPerMinute < 1 || c.SMTPAuthPerMinute > 6000 || c.SMTPAuthBurst < 1 || c.SMTPAuthBurst > 1000 || c.SMTPMaxSessionSeconds < 1 || c.SMTPMaxSessionSeconds > 3600 || c.AuthConcurrency < 2 || c.AuthConcurrency > 8 || c.AuthMemoryBudget < c.AuthConcurrency*(64<<20) || c.AuthMemoryBudget > 512<<20 {
		return c, errors.New("invalid SMTP admission or authentication memory budget")
	}
	if c.MetricsToken != "" && (len(c.MetricsToken) < 32 || len(c.MetricsToken) > 512 || strings.ContainsAny(c.MetricsToken, " \t\r\n")) {
		return c, errors.New("METRICS_BEARER_TOKEN must be 32..512 bytes without whitespace")
	}
	if c.FailoverEnabled && !c.RetryEnabled {
		return c, errors.New("FAILOVER_ENABLED requires RETRY_ENABLED")
	}
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
	if c.MemoryBudget < resource.SendMemory || c.MemoryBudget > 16<<30 || c.SpoolBudget < c.MaxMessageBytes || c.SpoolBudget > 64<<30 {
		return c, errors.New("memory budget must be 8 MiB..16 GiB; spool budget must cover max message size and be at most 64 GiB")
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
