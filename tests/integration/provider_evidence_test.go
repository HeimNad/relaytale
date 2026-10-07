package integration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"relaytale/internal/provider"
	"relaytale/internal/testsmtp"
)

func TestProviderEvidenceFencesAndStaleness(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{}, 1)
	id := providerID(t, f)
	ctx := context.Background()
	var rev int64
	if err := f.db.QueryRow(`SELECT revision FROM providers WHERE id=$1`, id).Scan(&rev); err != nil {
		t.Fatal(err)
	}
	e := provider.Evidence{Topic: "max_message_bytes", Value: "25000000", Source: "operator_report", ObservedAt: time.Now().Add(-31 * 24 * time.Hour)}
	if err := provider.RecordEvidence(ctx, f.db, id, rev+1, e, "test", "conflict"); !errors.Is(err, provider.ErrConflict) {
		t.Fatal("stale revision accepted")
	}
	if _, err := f.db.Exec(`ALTER TABLE maintenance_audit ADD CONSTRAINT reject_evidence CHECK(action <> 'PROVIDER_CAPABILITY_EVIDENCE')`); err != nil {
		t.Fatal(err)
	}
	if err := provider.RecordEvidence(ctx, f.db, id, rev, e, "test", "fault"); err == nil {
		t.Fatal("audit failure accepted")
	}
	if _, err := f.db.Exec(`ALTER TABLE maintenance_audit DROP CONSTRAINT reject_evidence`); err != nil {
		t.Fatal(err)
	}
	if err := provider.RecordEvidence(ctx, f.db, id, rev, e, "test", "provider plan"); err != nil {
		t.Fatal(err)
	}
	check := func(changed bool) {
		t.Helper()
		records, err := provider.EvidenceHistory(ctx, f.db, id, 100)
		if err != nil || len(records) != 1 {
			t.Fatal("unexpected evidence history", err)
		}
		var row struct {
			Changed bool `json:"configuration_changed"`
			Old     bool `json:"older_than_30_days"`
		}
		if err = json.Unmarshal(records[0], &row); err != nil || row.Changed != changed || !row.Old {
			t.Fatal("freshness mismatch", err)
		}
	}
	check(false)
	mustExec(t, f, `UPDATE providers SET priority=priority+1 WHERE id=$1`, id)
	check(true)
	if err := provider.RecordEvidence(ctx, f.db, id, rev, e, "test", "old revision"); !errors.Is(err, provider.ErrConflict) {
		t.Fatal("config change not fenced")
	}
	assertCount(t, f, 0, `SELECT count(*) FROM delivery_attempts`)
}
