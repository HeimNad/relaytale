package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"relaytale/internal/database"
	"relaytale/internal/recovery"
	"relaytale/internal/testsmtp"
)

func releaseAbandoned(t *testing.T, dbOwner func() error) {
	t.Helper()
	if err := dbOwner(); err != nil {
		t.Error(err)
	}
}

func TestRuntimeSingleOwnerAndOfflineExclusion(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	guard, err := database.AcquireRuntime(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			guard.Retain()
			owner, _ := database.ListRuntimeOwner(ctx, db)
			if owner != nil {
				releaseAbandoned(t, func() error {
					return database.ReleaseStaleOwner(ctx, db, owner.OwnerID, "test", "test cleanup after stopped guard")
				})
			}
		}
	})
	owner, err := database.ListRuntimeOwner(ctx, db)
	if err != nil || owner == nil {
		t.Fatal(owner, err)
	}
	if _, err = database.AcquireRuntime(ctx, db); !errors.Is(err, database.ErrRuntimeOccupied) {
		t.Fatal("second runtime", err)
	}
	if _, err = database.AcquireOffline(ctx, db); !errors.Is(err, database.ErrRuntimeOccupied) {
		t.Fatal("offline during runtime", err)
	}
	if err = database.Migrate(ctx, db); !errors.Is(err, database.ErrRuntimeOccupied) {
		t.Fatal("migration during runtime", err)
	}
	if err = database.ReleaseStaleOwner(ctx, db, owner.OwnerID, "test", "must not release live connection"); !errors.Is(err, database.ErrRuntimeOccupied) {
		t.Fatal("released live owner", err)
	}
	if err = recovery.Backup(ctx, recovery.Options{DB: db, Offline: true, StorageRoot: t.TempDir(), Bundle: t.TempDir()}); !errors.Is(err, database.ErrRuntimeOccupied) {
		t.Fatal("backup during runtime", err)
	}
	if err = guard.Release(ctx); err != nil {
		t.Fatal(err)
	}
	released = true
	if owner, err = database.ListRuntimeOwner(ctx, db); err != nil || owner != nil {
		t.Fatal("graceful owner not cleared", owner, err)
	}
	offline, err := database.AcquireOffline(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.AcquireRuntime(ctx, db); !errors.Is(err, database.ErrSchemaBusy) {
		offline.Close()
		t.Fatal("runtime during offline", err)
	}
	if _, err = database.AcquireOffline(ctx, db); !errors.Is(err, database.ErrSchemaBusy) {
		offline.Close()
		t.Fatal("second offline", err)
	}
	offline.Close()
	again, err := database.AcquireRuntime(ctx, db)
	if err != nil {
		t.Fatal("restart after clean exit", err)
	}
	if err = again.Release(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeConnectionLossNeverExpiresOwnership(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	guard, err := database.AcquireRuntime(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := database.ListRuntimeOwner(ctx, db)
	if err != nil || owner == nil {
		guard.Retain()
		t.Fatal(owner, err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			_ = database.ReleaseStaleOwner(ctx, db, owner.OwnerID, "test", "stopped failed test guard")
		}
	})
	if _, err = db.Exec(`SELECT pg_terminate_backend($1)`, owner.BackendPID); err != nil {
		guard.Retain()
		t.Fatal(err)
	}
	select {
	case <-guard.Lost():
	case <-time.After(5 * time.Second):
		guard.Retain()
		t.Fatal("lost session was not detected")
	}
	if err = guard.Release(ctx); !errors.Is(err, database.ErrOwnerLost) {
		t.Fatal("lost owner released automatically", err)
	}
	if _, err = database.AcquireRuntime(ctx, db); !errors.Is(err, database.ErrRuntimeOccupied) {
		t.Fatal("automatic takeover", err)
	}
	if err = database.Migrate(ctx, db); !errors.Is(err, database.ErrRuntimeOccupied) {
		t.Fatal("migration ignored orphan", err)
	}
	if err = database.ReleaseStaleOwner(ctx, db, uuid.NewString(), "test", "wrong expected owner"); err == nil {
		t.Fatal("wrong owner released")
	}
	if err = database.ReleaseStaleOwner(ctx, db, owner.OwnerID, "test", "original process and all SMTP operations confirmed stopped"); err != nil {
		t.Fatal(err)
	}
	released = true
	var audits int
	if err = db.QueryRow(`SELECT count(*) FROM maintenance_audit WHERE action='RUNTIME_OWNER_RELEASED' AND data->>'owner_id'=$1`, owner.OwnerID).Scan(&audits); err != nil || audits != 1 {
		t.Fatal("release audit", audits, err)
	}
	next, err := database.AcquireRuntime(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err = next.Release(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestOfflineConnectionLossRetainsOwnerAndCancelsWork(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	guard, err := database.AcquireOffline(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	work, stop := guard.Context(ctx)
	defer stop()
	owner, err := database.ListRuntimeOwner(ctx, db)
	if err != nil || owner == nil || owner.Mode != "offline" {
		_ = guard.Close()
		t.Fatal(owner, err)
	}
	t.Cleanup(func() { _ = database.ReleaseStaleOwner(ctx, db, owner.OwnerID, "test", "offline test process stopped") })
	if _, err = db.Exec(`SELECT pg_terminate_backend($1)`, owner.BackendPID); err != nil {
		_ = guard.Close()
		t.Fatal(err)
	}
	select {
	case <-work.Done():
	case <-time.After(5 * time.Second):
		_ = guard.Close()
		t.Fatal("offline work not canceled")
	}
	if err = guard.Close(); !errors.Is(err, database.ErrOwnerLost) {
		t.Fatal("offline owner cleared after loss", err)
	}
	if _, err = database.AcquireRuntime(ctx, db); !errors.Is(err, database.ErrRuntimeOccupied) {
		t.Fatal("runtime ignored abandoned offline work", err)
	}
	if err = database.Migrate(ctx, db); !errors.Is(err, database.ErrRuntimeOccupied) {
		t.Fatal("migration ignored abandoned offline work", err)
	}
	if err = database.ReleaseStaleOwner(ctx, db, owner.OwnerID, "test", "offline work confirmed stopped"); err != nil {
		t.Fatal(err)
	}
}

func TestOwnershipLossDuringFinalSMTPResponseBlocksReplacement(t *testing.T) {
	f := delivery(t, testsmtp.Options{FinalDelay: 4 * time.Second}, 1)
	id := f.seed(t)
	guard, err := database.AcquireRuntime(context.Background(), f.db)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := database.ListRuntimeOwner(context.Background(), f.db)
	if err != nil || owner == nil {
		guard.Retain()
		t.Fatal(owner, err)
	}
	work, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	var workErr error
	go func() { defer close(finished); _, workErr = f.worker.RunOne(work) }()
	released := false
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(12 * time.Second):
			t.Error("worker did not stop")
			return
		}
		if !released {
			guard.Retain()
			_ = database.ReleaseStaleOwner(context.Background(), f.db, owner.OwnerID, "test", "all injected SMTP work stopped")
		}
	})
	select {
	case <-f.fake.Payloads:
	case <-time.After(5 * time.Second):
		t.Fatal("SMTP body not received")
	}
	// The remote has the complete DATA. Losing only the ownership session does
	// not cancel that remote side effect or prove that a replacement is safe.
	if _, err = f.db.Exec(`SELECT pg_terminate_backend($1)`, owner.BackendPID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-guard.Lost():
	case <-time.After(5 * time.Second):
		t.Fatal("guard did not detect loss")
	}
	if _, err = database.AcquireRuntime(context.Background(), f.db); !errors.Is(err, database.ErrRuntimeOccupied) {
		t.Fatal("replacement while DATA uncertain", err)
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(12 * time.Second):
		t.Fatal("worker did not finalize")
	}
	if workErr != nil {
		t.Fatal("worker finalization", workErr)
	}
	if got := f.status(t, id); got != "DELIVERY_UNKNOWN" {
		t.Fatal("lost final response classification", got)
	}
	assertCount(t, f, 1, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1`, id)
	if worked, err := f.worker.RunOne(context.Background()); worked || err != nil {
		t.Fatal("UNKNOWN retried", worked, err)
	}
	if f.fake.Connections.Load() != 1 {
		t.Fatal("duplicate SMTP connection")
	}
	guard.Retain()
	if err = database.ReleaseStaleOwner(context.Background(), f.db, owner.OwnerID, "test", "worker finalized UNKNOWN and process confirmed stopped"); err != nil {
		t.Fatal(err)
	}
	released = true
}
