package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"relaytale/internal/database"
	"relaytale/internal/recovery"
)

func TestAuxiliaryOwnershipCoexistenceAndOfflineExclusion(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	runtime, err := database.AcquireRuntime(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Release(ctx)
	a, err := database.AcquireAuxiliary(ctx, db, "collect-feedback:test")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release(ctx)
	b, err := database.AcquireAuxiliary(ctx, db, "doctor")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Release(ctx)
	if _, err = database.AcquireAuxiliary(ctx, db, "collect-feedback:test"); !errors.Is(err, database.ErrRuntimeOccupied) {
		t.Fatal("duplicate collector", err)
	}
	owners, err := database.ListAuxiliaryOwners(ctx, db)
	if err != nil || len(owners) != 2 {
		t.Fatal(owners, err)
	}
	if err = database.ReleaseStaleAuxiliary(ctx, db, owners[0].OwnerID, "test", "live session must refuse"); !errors.Is(err, database.ErrRuntimeOccupied) {
		t.Fatal("live release", err)
	}
	if err = runtime.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = database.AcquireOffline(ctx, db); err == nil {
		t.Fatal("offline during auxiliary")
	}
	if err = database.Migrate(ctx, db); !errors.Is(err, database.ErrRuntimeOccupied) {
		t.Fatal("migration during auxiliary", err)
	}
	if err = recovery.Backup(ctx, recovery.Options{DB: db}); !errors.Is(err, database.ErrRuntimeOccupied) {
		t.Fatal("backup during auxiliary", err)
	}
	if err = a.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err = b.Release(ctx); err != nil {
		t.Fatal(err)
	}
	offline, err := database.AcquireOffline(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer offline.Close()
	if _, err = database.AcquireAuxiliary(ctx, db, "doctor"); !errors.Is(err, database.ErrSchemaBusy) {
		t.Fatal("auxiliary during offline", err)
	}
}

func TestAuxiliaryLossRetainsOfflineBlockAndAudit(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	g, err := database.AcquireAuxiliary(ctx, db, "collect-feedback:test")
	if err != nil {
		t.Fatal(err)
	}
	defer g.Retain()
	owners, err := database.ListAuxiliaryOwners(ctx, db)
	if err != nil || len(owners) != 1 {
		t.Fatal(owners, err)
	}
	o := owners[0]
	defer func() { database.ReleaseStaleAuxiliary(ctx, db, o.OwnerID, "test", "cleanup after stopped work") }()
	work, cancel := g.OfflineContext(ctx)
	defer cancel()
	if _, err = db.ExecContext(ctx, `SELECT pg_terminate_backend($1)`, o.BackendPID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-work.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("work not canceled")
	}
	if err = g.Release(ctx); !errors.Is(err, database.ErrOwnerLost) {
		t.Fatal(err)
	}
	if _, err = database.AcquireOffline(ctx, db); !errors.Is(err, database.ErrRuntimeOccupied) {
		t.Fatal("stale auxiliary bypass", err)
	}
	if err = database.Migrate(ctx, db); !errors.Is(err, database.ErrRuntimeOccupied) {
		t.Fatal("stale auxiliary migration", err)
	}
	if _, err = database.AcquireAuxiliary(ctx, db, o.Slot); !errors.Is(err, database.ErrRuntimeOccupied) {
		t.Fatal("automatic slot takeover", err)
	}
	if err = database.ReleaseStaleAuxiliary(ctx, db, uuid.NewString(), "test", "wrong id"); err == nil {
		t.Fatal("wrong owner released")
	}
	if err = database.ReleaseStaleAuxiliary(ctx, db, o.OwnerID, "test", "confirmed stopped after cancellation"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = db.QueryRow(`SELECT count(*) FROM maintenance_audit WHERE action='AUXILIARY_OWNER_RELEASED' AND data->>'owner_id'=$1`, o.OwnerID).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	next, err := database.AcquireAuxiliary(ctx, db, o.Slot)
	if err != nil {
		t.Fatal(err)
	}
	if err = next.Release(ctx); err != nil {
		t.Fatal(err)
	}
}
