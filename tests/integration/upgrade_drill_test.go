package integration

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"
	"relaytale/internal/database"
	"relaytale/migrations"
)

// This fixed v22 fixture deliberately models the supported v22 -> v23 upgrade.
// Future migrations must preserve this historical interruption regression.
func upgradeV22(t *testing.T) *sql.DB {
	t.Helper()
	db, _ := recoveryDB(t)
	files := fstest.MapFS{}
	names, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name >= "00023" {
			continue
		}
		body, err := fs.ReadFile(migrations.Files, name)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = &fstest.MapFile{Data: body}
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestUpgradeInterruptedMigrationAndExplicitRecovery(t *testing.T) {
	for _, mode := range []string{"version_write_failure", "cancel", "owner_connection_loss"} {
		t.Run(mode, func(t *testing.T) {
			db := upgradeV22(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := database.CheckSchema(ctx, db); !errors.Is(err, database.ErrSchema) {
				t.Fatal("old schema admitted", err)
			}
			exec := func(query string) {
				t.Helper()
				if _, err := db.ExecContext(ctx, query); err != nil {
					t.Fatal(err)
				}
			}
			body := "RAISE EXCEPTION 'synthetic version write failure';"
			var holder *sql.Conn
			if mode != "version_write_failure" {
				var err error
				holder, err = db.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer holder.Close()
				if _, err = holder.ExecContext(ctx, "SELECT pg_advisory_lock(734219599)"); err != nil {
					t.Fatal(err)
				}
				defer holder.ExecContext(context.Background(), "SELECT pg_advisory_unlock(734219599)")
				body = "PERFORM pg_advisory_xact_lock(734219599);"
			}
			exec("CREATE FUNCTION upgrade_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.version_id = 23 AND NEW.is_applied THEN " + body + " END IF; RETURN NEW; END $$")
			exec("CREATE TRIGGER upgrade_fault BEFORE INSERT ON goose_db_version FOR EACH ROW EXECUTE FUNCTION upgrade_fault()")
			operation, stop := context.WithCancel(ctx)
			defer stop()
			done := make(chan error, 1)
			go func() { done <- database.Migrate(operation, db) }()
			var ownerID string
			if holder != nil {
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for {
					var waiting bool
					if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND query LIKE '%goose_db_version%')").Scan(&waiting); err != nil {
						t.Fatal(err)
					}
					if waiting {
						break
					}
					select {
					case err := <-done:
						t.Fatalf("migration ended before fault barrier: %v", err)
					case <-ctx.Done():
						t.Fatal("fault barrier timeout")
					case <-ticker.C:
					}
				}
				if mode == "cancel" {
					stop()
				} else {
					owner, err := database.ListRuntimeOwner(ctx, db)
					if err != nil || owner == nil {
						t.Fatal(owner, err)
					}
					ownerID = owner.OwnerID
					var killed bool
					if err = db.QueryRowContext(ctx, "SELECT pg_terminate_backend($1)", owner.BackendPID).Scan(&killed); err != nil || !killed {
						t.Fatal("terminate owner", err)
					}
				}
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("faulted migration succeeded")
				}
			case <-ctx.Done():
				t.Fatal("migration failed to stop")
			}
			var tableExists bool
			if err := db.QueryRowContext(ctx, "SELECT to_regclass('public.auxiliary_owner') IS NOT NULL").Scan(&tableExists); err != nil || tableExists {
				t.Fatal("in-flight DDL was not rolled back", tableExists, err)
			}
			var applied bool
			if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM goose_db_version WHERE version_id=23 AND is_applied)").Scan(&applied); err != nil || applied {
				t.Fatal("failed version committed", applied, err)
			}
			if err := database.CheckSchema(ctx, db); !errors.Is(err, database.ErrSchema) {
				t.Fatal("partial schema admitted", err)
			}
			owner, err := database.ListRuntimeOwner(ctx, db)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "owner_connection_loss" {
				if owner == nil || owner.OwnerID != ownerID {
					t.Fatal("lost owner was silently cleared", owner)
				}
				if err := database.Migrate(ctx, db); !errors.Is(err, database.ErrRuntimeOccupied) {
					t.Fatal("abandoned migration admitted", err)
				}
				if _, err := database.AcquireRuntime(ctx, db); !errors.Is(err, database.ErrRuntimeOccupied) {
					t.Fatal("runtime admitted", err)
				}
				if err := database.ReleaseStaleOwner(ctx, db, ownerID, "upgrade-drill", "migration returned; terminated backend confirmed stopped"); err != nil {
					t.Fatal(err)
				}
			} else if owner != nil {
				t.Fatal("graceful failure left owner", owner)
			}
			if holder != nil {
				if _, err := holder.ExecContext(ctx, "SELECT pg_advisory_unlock(734219599)"); err != nil {
					t.Fatal(err)
				}
			}
			exec("DROP TRIGGER upgrade_fault ON goose_db_version")
			exec("DROP FUNCTION upgrade_fault()")
			if err := database.Migrate(ctx, db); err != nil {
				t.Fatal("explicit recovery", err)
			}
			if err := database.CheckSchema(ctx, db); err != nil {
				t.Fatal("recovered schema", err)
			}
		})
	}
}
