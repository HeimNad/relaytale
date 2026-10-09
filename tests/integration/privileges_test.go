package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"relaytale/internal/api"
	"relaytale/internal/database"
	"relaytale/internal/message"
	"relaytale/internal/operations"
	"relaytale/internal/testsmtp"
)

func TestRestrictedDatabaseRoleDeliveryAndBoundaries(t *testing.T) {
	admin, adminURL := recoveryDB(t)
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	owner := "owner_" + suffix
	runtime := "runtime_" + suffix
	recoverRole := "restore_" + suffix
	quote := func(s string) string { return pgx.Identifier{s}.Sanitize() }
	for _, r := range []string{owner, runtime, recoverRole} {
		if _, err := admin.Exec("CREATE ROLE " + quote(r) + " LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT PASSWORD 'synthetic-test-only'"); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		// recoveryDB drops the database in its earlier-registered cleanup, so first
		// remove ownership and privileges from these synthetic roles in this DB.
		for _, r := range []string{runtime, recoverRole, owner} {
			admin.Exec("REASSIGN OWNED BY " + quote(r) + " TO CURRENT_USER")
			admin.Exec("DROP OWNED BY " + quote(r))
			if _, err := admin.Exec("DROP ROLE " + quote(r)); err != nil {
				t.Error(err)
			}
		}
	})
	var dbName string
	if err := admin.QueryRow(`SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("ALTER DATABASE " + quote(dbName) + " OWNER TO " + quote(owner)); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("GRANT " + quote(owner) + " TO " + quote(recoverRole) + " WITH INHERIT FALSE, SET TRUE"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("ALTER ROLE " + quote(recoverRole) + " IN DATABASE " + quote(dbName) + " SET role TO " + quote(owner)); err != nil {
		t.Fatal(err)
	}
	roleURL := func(r string) string {
		u, _ := url.Parse(adminURL)
		u.User = url.UserPassword(r, "synthetic-test-only")
		return u.String()
	}
	open := func(r string) *sql.DB {
		db, err := database.Open(ctx, roleURL(r))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	migrateDB := open(owner)
	if err := database.Migrate(ctx, migrateDB); err != nil {
		t.Fatal(err)
	}
	profile, err := filepath.Abs("../../docker/postgres/runtime-grants.sql")
	if err != nil {
		t.Fatal(err)
	}
	apply := exec.Command("psql", roleURL(owner), "-X", "-q", "-v", "ON_ERROR_STOP=1", "-v", "runtime_role="+runtime, "-v", "recovery_role="+recoverRole, "-f", profile)
	if out, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("grants failed: %s (%v)", out, err)
	}
	restricted := open(runtime)
	if err := database.CheckRuntimePrivileges(ctx, restricted); err != nil {
		t.Fatal(err)
	}
	capacity, capacityErr := operations.Capacity(ctx, restricted, t.TempDir(), "", 1)
	if capacityErr != nil || len(capacity.Relations) != 8 || len(capacity.Items) != 0 {
		t.Fatal("restricted runtime capacity observation", capacity, capacityErr)
	}
	if err := database.CheckRuntimePrivileges(ctx, migrateDB); err == nil {
		t.Fatal("owner accepted as runtime")
	}
	if err := database.CheckRuntimePrivileges(ctx, admin); err == nil {
		t.Fatal("superuser accepted as runtime")
	}
	if err := database.CheckOfflinePrivileges(ctx, restricted); err == nil {
		t.Fatal("runtime accepted for offline")
	}
	restoreDB := open(recoverRole)
	if err := database.CheckOfflinePrivileges(ctx, restoreDB); err != nil {
		t.Fatal(err)
	}
	var current, session string
	if err := restoreDB.QueryRow(`SELECT current_user,session_user`).Scan(&current, &session); err != nil || current != owner || session != recoverRole {
		t.Fatal(current, session, err)
	}
	denied := []string{
		"CREATE TABLE public.must_not_exist(id int)", "CREATE TEMP TABLE must_not_exist(id int)", "CREATE SCHEMA must_not_exist",
		"CREATE ROLE must_not_exist", "SET ROLE " + quote(owner), "SET ROLE " + quote(recoverRole),
		"ALTER TABLE public.messages DISABLE TRIGGER ALL", "TRUNCATE public.messages", "DROP TABLE public.messages",
		"UPDATE public.goose_db_version SET is_applied=false", "DELETE FROM public.goose_db_version", "INSERT INTO public.goose_db_version(version_id,is_applied) VALUES(999,true)",
		"UPDATE public.maintenance_audit SET actor='changed'", "DELETE FROM public.events",
	}
	for _, q := range denied {
		if _, err := restricted.ExecContext(ctx, q); err == nil {
			t.Fatalf("forbidden operation succeeded: %s", q)
		}
	}
	// Existing full delivery fixture creates identities/providers and uses queue,
	// recorder, quota, health and trigger flows through the actual restricted login.
	t.Setenv("TEST_DATABASE_URL", roleURL(owner))
	f := delivery(t, testsmtp.Options{}, 1)
	f.db = restricted
	f.worker.Repo.DB = restricted
	f.addProvider(t, f.fake, 1, 1)
	sub := prepareAPI(t, f, "recipient@example.test")
	if _, err := (message.Postgres{DB: f.db}).EnqueueIdempotent(ctx, sub, testAPIKey(t, f), "privilege-key"); err != nil {
		t.Fatal(err)
	}
	runOne(t, f.worker)
	select {
	case <-f.fake.Payloads:
	case <-time.After(time.Second):
		t.Fatal("delivery unavailable with restricted role")
	}
	_, templateKey, _ := sendFixture(t, f)
	template := api.SendTemplate{ID: "notice", Text: "Hello"}
	template.Version = api.TemplateVersion(template)
	templateRaw, _ := json.Marshal(template)
	if _, err := api.PublishTemplate(ctx, restricted, templateKey.ID, templateRaw, "operator", "restricted publication test"); err != nil {
		t.Fatal(err)
	}
	if _, err := restricted.ExecContext(ctx, "UPDATE api_template_versions SET language='en'"); err == nil {
		t.Fatal("immutable template update permitted")
	}
	if _, err := api.RetireTemplate(ctx, restricted, templateKey.ID, template.ID, template.Version, "operator", "restricted retirement test"); err != nil {
		t.Fatal(err)
	}
	if _, err := restricted.ExecContext(ctx, "DELETE FROM api_template_retirements"); err == nil {
		t.Fatal("retirement history deletion permitted")
	}
	g, err := database.AcquireRuntime(ctx, restricted)
	if err != nil {
		t.Fatal(err)
	}
	if err = g.Release(ctx); err != nil {
		t.Fatal(err)
	}
	a, err := database.AcquireAuxiliary(ctx, restricted, "doctor")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Release(ctx); err != nil {
		t.Fatal(err)
	}

	// Even non-inheriting, non-settable membership must not be carried by runtime.
	if _, err = admin.Exec("GRANT " + quote(owner) + " TO " + quote(runtime) + " WITH INHERIT FALSE, SET FALSE"); err != nil {
		t.Fatal(err)
	}
	if err = database.CheckRuntimePrivileges(ctx, restricted); err == nil {
		t.Fatal("membership accepted")
	}
	if _, err = admin.Exec("REVOKE " + quote(owner) + " FROM " + quote(runtime)); err != nil {
		t.Fatal(err)
	}
	// Ownership of a trigger/validator function can undermine schema invariants.
	if _, err = admin.Exec("CREATE FUNCTION public.privilege_probe() RETURNS integer LANGUAGE sql AS 'SELECT 1'"); err != nil {
		t.Fatal(err)
	}
	if _, err = admin.Exec("ALTER FUNCTION public.privilege_probe() OWNER TO " + quote(runtime)); err != nil {
		t.Fatal(err)
	}
	if err = database.CheckRuntimePrivileges(ctx, restricted); err == nil {
		t.Fatal("function ownership accepted")
	}
	if _, err = admin.Exec("DROP FUNCTION public.privilege_probe()"); err != nil {
		t.Fatal(err)
	}
	if err = database.CheckRuntimePrivileges(ctx, restricted); err != nil {
		t.Fatal("valid permissions rejected", err)
	}
	// Unexpected membership and history write grants make the startup check fail.
	if _, err = admin.Exec("GRANT SELECT,INSERT ON public.goose_db_version TO " + quote(runtime)); err != nil {
		t.Fatal(err)
	}
	if err = database.CheckRuntimePrivileges(ctx, restricted); err == nil {
		t.Fatal("history write grant accepted")
	}
}
