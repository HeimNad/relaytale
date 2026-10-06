package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
	"relaytale/migrations"
)

func Open(ctx context.Context, url string) (*sql.DB, error) {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return db, nil
}

// migrationAdmission avoids exhausting one process's pool with lock waiters.
var migrationAdmission = make(chan struct{}, 1)

// Migrate guards Goose's otherwise-unlocked HasPending/bootstrap before Up.
// Goose additionally holds an execution lock on its migration connection.
func Migrate(ctx context.Context, db *sql.DB) (retErr error) {
	select {
	case migrationAdmission <- struct{}{}:
		defer func() { <-migrationAdmission }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if db.Stats().MaxOpenConnections == 1 {
		return errors.New("migration requires at least two database connections")
	}
	bootstrap, err := lock.NewPostgresSessionLocker(lock.WithLockID(MigrationLockID), lock.WithLockTimeout(1, 300), lock.WithUnlockTimeout(1, 5))
	if err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err = bootstrap.SessionLock(ctx, conn); err != nil {
		// A canceled response can conceal a successfully acquired session lock.
		// Discard this connection rather than returning possible ownership to the pool.
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		return fmt.Errorf("migration admission: %w", err)
	}
	if err = CheckNoRuntime(ctx, conn); err != nil {
		discard(conn)
		return err
	}
	guard, err := registerOffline(ctx, conn)
	if err != nil {
		discard(conn)
		return err
	}
	operation, stop := guard.OfflineContext(ctx)
	defer stop()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		retErr = errors.Join(retErr, guard.Release(cleanup))
	}()
	ctx = operation
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockID(734219510), lock.WithLockTimeout(1, 300), lock.WithUnlockTimeout(1, 5))
	if err != nil {
		return err
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrations.Files, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("create migration provider: %w", err)
	}
	if _, err = p.Up(ctx); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	return nil
}
