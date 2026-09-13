package integration

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"relaytale/internal/database"
	"strings"
	"syscall"
	"testing"
	"time"

	"relaytale/internal/api"
	"relaytale/internal/auth"
	"relaytale/internal/testsmtp"
)

// An opt-in, isolated fixture server for real-browser tests; never uses real SMTP.
func TestBrowserWorkspaceFixture(t *testing.T) {
	if os.Getenv("WEB_UI_E2E") != "1" {
		t.Skip("browser fixture only")
	}

	// Keep browser fixtures separate from the full integration suite and prior runs.
	dbURL, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil || dbURL.Host == "" {
		t.Fatal("isolated TEST_DATABASE_URL required")
	}
	base, err := database.Open(context.Background(), dbURL.String())
	if err != nil {
		t.Fatal(err)
	}
	schema := "browser_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = base.Exec(`CREATE SCHEMA ` + schema); err != nil {
		base.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { base.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); base.Close() })
	query := dbURL.Query()
	query.Set("search_path", schema)
	dbURL.RawQuery = query.Encode()
	t.Setenv("TEST_DATABASE_URL", dbURL.String())
	f := delivery(t, testsmtp.Options{DropFinal: true}, 1)
	mid := f.seed(t)
	mustExec(t, f, `UPDATE messages SET subject=$2 WHERE id=$1`, mid, "订单确认 · 需要核对投递结果")
	runOne(t, f.worker)
	second := f.seed(t)
	mustExec(t, f, `UPDATE messages SET subject=$2 WHERE id=$1`, second, `<img src="https://untrusted.invalid/pixel" onerror="window.injected=true">`)
	third := f.seed(t)
	mustExec(t, f, `UPDATE messages SET subject=$2 WHERE id=$1`, third, "欢迎使用 RelayTale / 集成验证")
	mustExec(t, f, `UPDATE providers SET name='本地验证通道' WHERE from_domains=ARRAY[$1]`, f.domain)
	hash, err := auth.Hash("browser-test-password-only")
	if err != nil {
		t.Fatal(err)
	}
	users, _ := json.Marshal([]api.WebUser{{ID: "owner", Role: "admin", PasswordHash: hash}, {ID: "reader", Role: "viewer", PasswordHash: hash}})
	origin := os.Getenv("WEB_UI_TEST_ORIGIN")
	if origin == "" {
		origin = "http://127.0.0.1:18080"
	}
	addr := os.Getenv("WEB_UI_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:18080"
	}
	manager, err := (api.Management{DB: f.db, Box: f.box}).Handler("", api.WebConfig{Origin: origin, Users: string(users)})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: api.Handler(f.db, func() error { return nil }, nil, manager), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second}
	defer server.Close()
	stopCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-stopCtx.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	}()

	t.Log("browser fixture ready")
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}
