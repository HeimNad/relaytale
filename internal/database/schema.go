package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"relaytale/migrations"
)

// MigrationLockID is database-wide, independent of process, host and search_path.
// It guards migration admission, including Goose version-table initialization.
const MigrationLockID int64 = 734219509

var ErrSchema = errors.New("database schema is incompatible")
var ErrSchemaBusy = errors.New("database migration is in progress")

func expectedVersions() (map[int64]bool, error) {
	files, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		return nil, err
	}
	versions := make(map[int64]bool, len(files))
	for _, name := range files {
		prefix, _, ok := strings.Cut(name, "_")
		n, err := strconv.ParseInt(prefix, 10, 64)
		if !ok || err != nil || n <= 0 || versions[n] {
			return nil, errors.New("invalid embedded migration versions")
		}
		versions[n] = true
	}
	if len(versions) == 0 {
		return nil, errors.New("no embedded migrations")
	}
	return versions, nil
}

// CheckSchema is deliberately independent of Goose's provider initialization:
// startup must never CREATE a version table or apply a pending migration.
// It verifies every embedded version, including holes and newer history, in a
// read-only snapshot protected against an already-running migration.
func CheckSchema(ctx context.Context, db *sql.DB) error {
	expected, err := expectedVersions()
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var locked bool
	if err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock_shared($1)`, MigrationLockID).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return ErrSchemaBusy
	}
	rows, err := tx.QueryContext(ctx, `SELECT version_id,is_applied FROM goose_db_version ORDER BY id`)
	if err != nil {
		return fmt.Errorf("%w: migration history unavailable", ErrSchema)
	}
	defer rows.Close()
	applied := make(map[int64]bool)
	for rows.Next() {
		var version int64
		var yes bool
		if err = rows.Scan(&version, &yes); err != nil {
			return err
		}
		if version == 0 {
			continue
		}
		if !expected[version] {
			return fmt.Errorf("%w: unknown migration version", ErrSchema)
		}
		applied[version] = yes
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for version := range expected {
		if !applied[version] {
			return fmt.Errorf("%w: unapplied migration", ErrSchema)
		}
	}
	if err = rows.Close(); err != nil {
		return err
	}
	return tx.Commit()
}
