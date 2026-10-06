package integration

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"relaytale/internal/database"
)

func migrationDB(t *testing.T) *sql.DB {
	t.Helper()
	parent := testDB(t)
	schema := "migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := parent.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := database.Open(context.Background(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		if _, err := parent.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Error(err)
		}
	})
	return db
}

func TestSchemaStartupReadOnlyAndVersionHistory(t *testing.T) {
	db := migrationDB(t)
	ctx := context.Background()
	if err := database.CheckSchema(ctx, db); !errors.Is(err, database.ErrSchema) {
		t.Fatal("empty schema", err)
	}
	var table sql.NullString
	if err := db.QueryRow(`SELECT to_regclass('goose_db_version')::text`).Scan(&table); err != nil || table.Valid {
		t.Fatal("startup created history", table, err)
	}
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := database.CheckSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	// PostgreSQL itself rejects any write made by startup schema verification.
	if _, err := db.Exec(`SET default_transaction_read_only=on`); err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := database.CheckSchema(ctx, db); err != nil {
		t.Fatal("read-only verification", err)
	}
	if _, err := db.Exec(`SET default_transaction_read_only=off`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO goose_db_version(version_id,is_applied) VALUES(999999,true)`); err != nil {
		t.Fatal(err)
	}
	if err := database.CheckSchema(ctx, db); !errors.Is(err, database.ErrSchema) {
		t.Fatal("newer history accepted", err)
	}
	if _, err := db.Exec(`DELETE FROM goose_db_version WHERE version_id=999999`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO goose_db_version(version_id,is_applied) VALUES(1,false)`); err != nil {
		t.Fatal(err)
	}
	if err := database.CheckSchema(ctx, db); !errors.Is(err, database.ErrSchema) {
		t.Fatal("history hole accepted", err)
	}
	if _, err := db.Exec(`INSERT INTO goose_db_version(version_id,is_applied) VALUES(1,true)`); err != nil {
		t.Fatal(err)
	}
	if err := database.CheckSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(2)
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal("two-connection migration pool", err)
	}
	db.SetMaxOpenConns(1)
	if err := database.Migrate(ctx, db); err == nil || err.Error() != "migration requires at least two database connections" {
		t.Fatal("single-connection migration pool", err)
	}

}

func TestMigrationLockCancellationAndConcurrentInitialization(t *testing.T) {
	db := migrationDB(t)
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, database.MigrationLockID); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, database.MigrationLockID)
	if err = database.CheckSchema(ctx, db); !errors.Is(err, database.ErrSchemaBusy) {
		t.Fatal("check did not reject active migration", err)
	}
	short, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	err = database.Migrate(short, db)
	cancel()
	if err == nil {
		t.Fatal("migration ignored exclusive lock")
	}
	var table sql.NullString
	if err = db.QueryRow(`SELECT to_regclass('goose_db_version')::text`).Scan(&table); err != nil || table.Valid {
		t.Fatal("blocked migration wrote schema", table, err)
	}
	if _, err = conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, database.MigrationLockID); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- database.Migrate(ctx, db) }()
	}
	for range 2 {
		if err = <-results; err != nil {
			t.Fatal("concurrent migrate", err)
		}
	}
	if err = database.CheckSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	var duplicated bool
	if err = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM goose_db_version WHERE version_id>0 GROUP BY version_id HAVING count(*)<>1)`).Scan(&duplicated); err != nil || duplicated {
		t.Fatal("duplicate migrations", duplicated, err)
	}
	// Lock must be released back to the pool after both runs.
	var available bool
	if err = conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, database.MigrationLockID).Scan(&available); err != nil || !available {
		t.Fatal("migration leaked lock", available, err)
	}
}
