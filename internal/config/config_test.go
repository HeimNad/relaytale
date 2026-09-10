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
