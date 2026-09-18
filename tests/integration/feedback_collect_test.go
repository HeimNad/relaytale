package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"relaytale/internal/feedback"
)

type testMailbox struct {
	epoch, next uint32
	meta        []feedback.MailMetadata
	raw         map[uint32][]byte
	fail        uint32
	calls       []uint32
	onSelect    func()
	closed      bool
}

func (b *testMailbox) Select(context.Context) (feedback.MailboxSnapshot, error) {
	if b.onSelect != nil {
		b.onSelect()
	}
	return feedback.MailboxSnapshot{UIDValidity: b.epoch, UIDNext: b.next}, nil
}
func (b *testMailbox) Metadata(_ context.Context, first, last uint32) (out []feedback.MailMetadata, err error) {
	for _, m := range b.meta {
		if m.UID >= first && m.UID <= last {
			out = append(out, m)
		}
	}
	return
}
func (b *testMailbox) Body(_ context.Context, uid uint32) ([]byte, bool, error) {
	b.calls = append(b.calls, uid)
	if uid == b.fail {
		return nil, false, errors.New("disconnect")
	}
	r, ok := b.raw[uid]
	return r, ok, nil
}
func (b *testMailbox) Close() error                                   { b.closed = true; return nil }
func (b *testMailbox) open(context.Context) (feedback.Mailbox, error) { return b, nil }
func collectionOptions() feedback.CollectOptions {
	return feedback.CollectOptions{Source: uuid.NewString(), IdentitySHA256: strings.Repeat("a", 64), Batch: 20}
}
func TestFeedbackCollectionCheckpointsAndEpoch(t *testing.T) {
	db := testDB(t)
	s := feedback.Service{DB: db}
	ctx := context.Background()
	o := collectionOptions()
	raw := []byte(feedbackRaw("one@example.test"))
	box := &testMailbox{epoch: 42, next: 5, raw: map[uint32][]byte{1: raw, 2: raw}, meta: []feedback.MailMetadata{{UID: 1, Size: int64(len(raw))}, {UID: 2, Size: int64(len(raw))}, {UID: 3, Size: feedback.MaxBytes + 1}, {UID: 4, Size: 10}}, fail: 2}
	out, err := s.Collect(ctx, o, box.open)
	if err == nil || out.LastUID != 1 || out.Imported != 1 || !box.closed {
		t.Fatal(out, err)
	}
	status, err := s.CollectionStatus(ctx, o.Source, 0, 0, 100)
	if err != nil || status.LastUID != 1 || status.Records != 1 {
		t.Fatal(status, err)
	}
	box.fail = 0
	box.calls = nil
	out, err = s.Collect(ctx, o, box.open)
	if err != nil || out.LastUID != 4 || out.Imported != 1 || out.Issues != 2 {
		t.Fatal(out, err)
	}
	for _, uid := range box.calls {
		if uid == 1 || uid == 3 {
			t.Fatal("refetched committed/oversize body", box.calls)
		}
	}
	status, err = s.CollectionStatus(ctx, o.Source, 0, 0, 100)
	if err != nil || status.Records != 4 || status.StoredBytes != int64(2*len(raw)) || status.Receipts[2].Outcome != "OVERSIZE" || status.Receipts[3].Outcome != "VANISHED" {
		t.Fatal(status, err)
	}
	for _, r := range status.Receipts {
		if r.FeedbackID != "" {
			entry, e := s.Get(ctx, r.FeedbackID)
			if e != nil || entry.Trust != "UNVERIFIED" || entry.State != "QUARANTINED" {
				t.Fatal(entry, e)
			}
		}
	}
	out, err = s.Collect(ctx, o, box.open)
	if err != nil || out.Imported != 0 {
		t.Fatal(out, err)
	}
	box.epoch = 43
	box.next = 2
	box.meta = box.meta[:1]
	for i := 0; i < 2; i++ {
		if _, err = s.Collect(ctx, o, box.open); !errors.Is(err, feedback.ErrMailboxChanged) {
			t.Fatal(err)
		}
	}
	status, err = s.CollectionStatus(ctx, o.Source, 0, 0, 100)
	if err != nil || status.UIDValidity != 42 || status.Records != 5 || len(status.LatestAlerts) != 1 {
		t.Fatal(status, err)
	}
	o.AcceptUIDValidity = 43
	out, err = s.Collect(ctx, o, box.open)
	if err != nil || out.Imported != 1 {
		t.Fatal(out, err)
	}
	status, err = s.CollectionStatus(ctx, o.Source, 42, 1, 1)
	if err != nil || status.UIDValidity != 43 || len(status.Receipts) != 1 || status.Receipts[0].UID != 2 {
		t.Fatal(status, err)
	}
	o.IdentitySHA256 = strings.Repeat("b", 64)
	if _, err = s.Collect(ctx, o, box.open); !errors.Is(err, feedback.ErrMailboxChanged) {
		t.Fatal(err)
	}
}
func TestFeedbackCollectionFailureSafety(t *testing.T) {
	for _, scenario := range []string{"size-mismatch", "empty", "quota", "audit-rollback", "commit-rollback", "busy", "sparse", "uid-regressed"} {
		t.Run(scenario, func(t *testing.T) {
			db := testDB(t)
			s := feedback.Service{DB: db}
			ctx := context.Background()
			o := collectionOptions()
			raw := []byte(feedbackRaw("two@example.test"))
			box := &testMailbox{epoch: 1, next: 1, raw: map[uint32][]byte{1: raw}, meta: []feedback.MailMetadata{{UID: 1, Size: int64(len(raw))}}}
			if _, err := s.Collect(ctx, o, box.open); err != nil {
				t.Fatal(err)
			}
			box.next = 2
			switch scenario {
			case "size-mismatch":
				box.raw[1] = []byte("truncated")
			case "empty":
				box.meta[0].Size = 0
			case "quota":
				if _, err := db.Exec(`UPDATE feedback_imap_sources SET stored_bytes=104857600 WHERE name=$1`, o.Source); err != nil {
					t.Fatal(err)
				}
			case "audit-rollback", "commit-rollback":
				// A source-specific failure avoids disrupting other fixtures. A deferred
				// constraint trigger exercises a failure at COMMIT, after all inserts.
				funcName := "collect_failure_" + strings.ReplaceAll(uuid.NewString(), "-", "")
				table := "maintenance_audit"
				condition := "NEW.action='FEEDBACK_IMAP_COLLECTED' AND NEW.data->>'source'='" + o.Source + "'"
				trigger := `CREATE TRIGGER ` + funcName + ` BEFORE INSERT ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION ` + funcName + `()`
				if scenario == "commit-rollback" {
					table = "feedback_imap_receipts"
					condition = "NEW.source='" + o.Source + "'"
					trigger = `CREATE CONSTRAINT TRIGGER ` + funcName + ` AFTER INSERT ON ` + table + ` DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ` + funcName + `()`
				}
				if _, err := db.Exec(`CREATE FUNCTION ` + funcName + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF ` + condition + ` THEN RAISE EXCEPTION 'injected'; END IF; RETURN NEW; END $$; ` + trigger); err != nil {
					t.Fatal(err)
				}
				defer db.Exec(`DROP TRIGGER ` + funcName + ` ON ` + table + `; DROP FUNCTION ` + funcName + `()`) // generated identifiers only

			case "busy":
				box.onSelect = func() {
					opened := false
					_, err := s.Collect(ctx, o, func(context.Context) (feedback.Mailbox, error) { opened = true; return box, nil })
					if !errors.Is(err, feedback.ErrCollectorBusy) || opened {
						t.Fatal("lock failed", err, opened)
					}
				}
			case "sparse":
				box.meta = nil
				box.next = 100
			case "uid-regressed":
				if _, err := db.Exec(`UPDATE feedback_imap_sources SET last_uid=3 WHERE name=$1`, o.Source); err != nil {
					t.Fatal(err)
				}
			}
			out, err := s.Collect(ctx, o, box.open)
			success := scenario == "busy" || scenario == "sparse" || scenario == "empty"
			if (err == nil) != success {
				t.Fatal(out, err)
			}
			st, e := s.CollectionStatus(ctx, o.Source, 0, 0, 100)
			if e != nil {
				t.Fatal(e)
			}
			if !success && scenario != "uid-regressed" && (st.LastUID != 0 || len(st.Receipts) != 0) {
				t.Fatal("failed batch advanced", st)
			}
			if scenario == "sparse" && st.LastUID != 20 {
				t.Fatal("unbounded sparse advance", st)
			}
			if scenario == "empty" && (len(box.calls) != 0 || st.Receipts[0].Outcome != "EMPTY") {
				t.Fatal(st)
			}
			if scenario == "audit-rollback" || scenario == "commit-rollback" {
				var n int
				if e = db.QueryRow(`SELECT count(*) FROM feedback_reports WHERE source=$1`, "imap:"+o.Source).Scan(&n); e != nil || n != 0 {
					t.Fatal("orphaned evidence", n, e)
				}
			}
		})
	}
}

func TestFeedbackCollectionCancellationReleasesLock(t *testing.T) {
	db := testDB(t)
	s := feedback.Service{DB: db}
	o := collectionOptions()
	ctx, cancel := context.WithCancel(context.Background())
	box := &testMailbox{epoch: 1, next: 1, onSelect: cancel}
	if _, err := s.Collect(ctx, o, box.open); err == nil || !box.closed {
		t.Fatal(err, box.closed)
	}
	box.onSelect = nil
	if _, err := s.Collect(context.Background(), o, box.open); err != nil {
		t.Fatal("lock leaked after cancellation", err)
	}
}
