package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrecedence(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://env/db")
	t.Setenv("HTTP_LISTEN_ADDR", ":8081")
	t.Setenv("EML_STORAGE_DIR", "env-storage")
	t.Setenv("SHUTDOWN_TIMEOUT", "10s")
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("http_listen_addr: ':8082'\ndatabase_url: postgres://yaml/db\nstorage_dir: yaml-storage\nshutdown_timeout: 5s\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load([]string{"--config", p, "--http-addr", ":9090"})
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != ":9090" || c.DatabaseURL != "postgres://env/db" || c.StorageDir != "env-storage" || c.ShutdownTimeout != 10*time.Second {
		t.Fatalf("incorrect precedence: %+v", c)
	}
}
func TestInvalidConfig(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	if _, err := Load(nil); err == nil {
		t.Fatal("missing database accepted")
	}
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("SHUTDOWN_TIMEOUT", "0s")
	if _, err := Load(nil); err == nil {
		t.Fatal("zero timeout accepted")
	}
}

func TestWorkerConfiguration(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("WORKER_COUNT", "4")
	t.Setenv("RELAYTALE_MASTER_KEY", "")
	if _, err := Load(nil); err == nil {
		t.Fatal("workers enabled without encryption key")
	}
	t.Setenv("RELAYTALE_MASTER_KEY", "abababababababababababababababababababababababababababababababab")
	cfg, err := Load(nil)
	if err != nil || cfg.WorkerCount != 4 {
		t.Fatalf("valid worker config: %v", err)
	}
	cfg, err = Load([]string{"--workers", "0"})
	if err != nil || cfg.WorkerCount != 0 {
		t.Fatal("CLI worker override ignored")
	}
	t.Setenv("WORKER_COUNT", "33")
	if _, err := Load(nil); err == nil {
		t.Fatal("unbounded worker count accepted")
	}
}

func TestRetryOptIn(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("RETRY_ENABLED", "false")
	c, err := Load(nil)
	if err != nil || c.RetryEnabled {
		t.Fatal("retry should be disabled", err)
	}
	c, err = Load([]string{"--retry-enabled"})
	if err != nil || !c.RetryEnabled {
		t.Fatal("CLI opt-in ignored", err)
	}
	t.Setenv("RETRY_ENABLED", "invalid")
	if _, err := Load(nil); err == nil {
		t.Fatal("invalid retry flag accepted")
	}
}

func TestFailoverOptIn(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("RETRY_ENABLED", "false")
	t.Setenv("FAILOVER_ENABLED", "false")
	c, err := Load(nil)
	if err != nil || c.FailoverEnabled {
		t.Fatal("default failover", err)
	}
	if _, err = Load([]string{"--failover-enabled"}); err == nil {
		t.Fatal("failover without retry")
	}
	c, err = Load([]string{"--failover-enabled", "--retry-enabled"})
	if err != nil || !c.FailoverEnabled {
		t.Fatal("opt-in", err)
	}
	t.Setenv("FAILOVER_ENABLED", "invalid")
	if _, err = Load(nil); err == nil {
		t.Fatal("invalid flag")
	}
}

func TestHealthOptIn(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("HEALTH_ENABLED", "false")
	c, err := Load(nil)
	if err != nil || c.HealthEnabled {
		t.Fatal(err)
	}
	c, err = Load([]string{"--health-enabled"})
	if err != nil || !c.HealthEnabled {
		t.Fatal(err)
	}
	t.Setenv("HEALTH_ENABLED", "invalid")
	if _, err = Load(nil); err == nil {
		t.Fatal("invalid health flag accepted")
	}
}

func TestResourceBudgetConfiguration(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("MEMORY_BUDGET_BYTES", "16777216")
	t.Setenv("SPOOL_BUDGET_BYTES", "52428800")
	cfg, err := Load([]string{"--memory-budget-bytes", "33554432", "--snapshot-dir", "/tmp/snapshots"})
	if err != nil || cfg.MemoryBudget != 32<<20 || cfg.SpoolBudget != 50<<20 || cfg.SnapshotDir != "/tmp/snapshots" {
		t.Fatal("budget precedence", err)
	}
	for _, args := range [][]string{{"--memory-budget-bytes", "0"}, {"--spool-budget-bytes", "1024"}, {"--max-message-bytes", "1073741824"}} {
		if _, err := Load(args); err == nil {
			t.Fatal("impossible work budget accepted", args)
		}
	}
}
