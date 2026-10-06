package main

import "testing"

func TestRuntimeReleaseRequiresExplicitStoppedOwner(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://must-not-be-contacted.invalid/private")
	for _, args := range [][]string{nil, {"--owner-id", "anything"}, {"--confirm-stopped"}} {
		if err := runRuntimeOwner("release-runtime-owner", args); err == nil || err.Error() != "release requires exact owner-id, confirm-stopped, actor and reason; stop the original process and reconcile in-flight sends first" {
			t.Fatal(args, err)
		}
	}
}
