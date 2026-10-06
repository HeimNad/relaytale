package main

import "testing"

func TestMigrationRequiresOfflineBeforeDatabaseAccess(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://must-not-be-contacted.invalid/private")
	for _, args := range [][]string{nil, {"--offline=false"}, {"--offline", "unexpected"}} {
		if err := runMigrate(args); err == nil || err.Error() != "migration requires --offline; stop all writers and senders first" {
			t.Fatalf("args=%v: %v", args, err)
		}
	}
	t.Setenv("DATABASE_URL", "")
	if err := runMigrate([]string{"--offline"}); err == nil || err.Error() != "DATABASE_URL required for migration" {
		t.Fatal(err)
	}
}
