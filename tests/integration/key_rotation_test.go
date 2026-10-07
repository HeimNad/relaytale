package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"relaytale/internal/database"
	"relaytale/internal/encryption"
)

func TestProviderKeyRotationAtomic(t *testing.T) {
	db, _ := recoveryDB(t)
	ctx := context.Background()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	old, _ := encryption.New(strings.Repeat("ab", 32))
	ring, _ := encryption.NewRing("v2", map[string]string{"legacy": strings.Repeat("ab", 32), "v2": strings.Repeat("cd", 32)})
	current, _ := encryption.NewRing("v2", map[string]string{"v2": strings.Repeat("cd", 32)})
	ids := []string{uuid.NewString(), uuid.NewString()}
	for i, id := range ids {
		c, n, _ := old.Seal(id, "synthetic-password")
		if _, err := db.Exec(`INSERT INTO providers(id,name,host,port,security,username,password_ciphertext,nonce,enabled) VALUES($1,$2,'smtp.example.test',465,'implicit_tls','synthetic',$3,$4,$5)`, id, id, c, n, i == 0); err != nil {
			t.Fatal(err)
		}
	}
	guard, err := database.AcquireOffline(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	ctx, stop := guard.Context(ctx)
	defer stop()
	report, err := encryption.RotateProviders(ctx, db, ring, "test", "dry run", false)
	if err != nil || report.Changed != 2 || report.Checked != 2 {
		t.Fatalf("dry run: %+v %v", report, err)
	}
	check := func(box *encryption.Box) {
		t.Helper()
		for _, id := range ids {
			var c, n []byte
			if err := db.QueryRow(`SELECT password_ciphertext,nonce FROM providers WHERE id=$1`, id).Scan(&c, &n); err != nil {
				t.Fatal(err)
			}
			if p, err := box.Open(id, c, n); err != nil || p != "synthetic-password" {
				t.Fatal("credential unreadable")
			}
		}
	}
	check(old)
	if _, err := encryption.RotateProviders(ctx, db, current, "test", "missing legacy key", true); err == nil {
		t.Fatal("missing key allowed rotation")
	}
	check(old)
	// Force failure after all row updates: the audit and credential changes must roll back together.
	if _, err := db.Exec(`ALTER TABLE maintenance_audit ADD CONSTRAINT reject_rotation CHECK(action <> 'PROVIDER_KEYS_ROTATED')`); err != nil {
		t.Fatal(err)
	}
	if _, err := encryption.RotateProviders(ctx, db, ring, "test", "fault injection", true); err == nil {
		t.Fatal("audit failure accepted")
	}
	check(old)
	if _, err := db.Exec(`ALTER TABLE maintenance_audit DROP CONSTRAINT reject_rotation`); err != nil {
		t.Fatal(err)
	}
	report, err = encryption.RotateProviders(ctx, db, ring, "test", "apply", true)
	if err != nil || report.Changed != 2 {
		t.Fatalf("apply: %+v %v", report, err)
	}
	check(current)
	report, err = encryption.RotateProviders(ctx, db, current, "test", "repeat", true)
	if err != nil || report.Changed != 0 || report.Checked != 2 {
		t.Fatal("repeat rotation not idempotent")
	}
	wrong, _ := encryption.NewRing("v2", map[string]string{"v2": strings.Repeat("ef", 32)})
	if _, err := encryption.RotateProviders(ctx, db, wrong, "test", "wrong current key", false); err == nil {
		t.Fatal("current credentials not authenticated")
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM maintenance_audit WHERE action='PROVIDER_KEYS_ROTATED'`).Scan(&count); err != nil || count != 2 {
		t.Fatal("rotation audit missing")
	}
}
