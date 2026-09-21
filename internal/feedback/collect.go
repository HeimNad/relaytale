package feedback

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"
)

var ErrCollectorBusy = errors.New("feedback collector already running for this source")
var ErrMailboxChanged = errors.New("mailbox identity or UID epoch changed; manual review required")
var ErrCapacity = errors.New("feedback evidence or collector storage quota reached")
var sourceName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

const CollectorMaxBytes = 100 << 20
const CollectorMaxRecords = 10000

type MailboxSnapshot struct{ UIDValidity, UIDNext uint32 }
type MailMetadata struct {
	UID  uint32
	Size int64
}

// Mailbox has deliberately no write, flag, delete or expunge operation.
type Mailbox interface {
	Select(context.Context) (MailboxSnapshot, error)
	Metadata(context.Context, uint32, uint32) ([]MailMetadata, error)
	Body(context.Context, uint32) ([]byte, bool, error) // false means expunged since metadata
	Close() error
}
type CollectOptions struct {
	Source, IdentitySHA256 string
	Batch                  int
	AcceptUIDValidity      uint32
}
type Collection struct {
	Source      string `json:"source"`
	UIDValidity uint32 `json:"uid_validity"`
	LastUID     uint32 `json:"last_uid"`
	Imported    int    `json:"imported"`
	Issues      int    `json:"issues"`
}

// Collect takes a session-scoped source lock, never a SQL transaction across
// network I/O. Each receipt + evidence + audit + checkpoint commits atomically.
func (s Service) Collect(ctx context.Context, o CollectOptions, open func(context.Context) (Mailbox, error)) (Collection, error) {
	out := Collection{Source: o.Source}
	digest, err := hex.DecodeString(o.IdentitySHA256)
	if !sourceName.MatchString(o.Source) || err != nil || len(digest) != 32 || hex.EncodeToString(digest) != o.IdentitySHA256 || o.Batch < 1 || o.Batch > 100 {
		return out, errors.New("invalid collection identity or batch")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return out, err
	}
	defer conn.Close()
	h := sha256.Sum256([]byte("relaytale.imap.lock/" + o.Source))
	key := int64(binary.BigEndian.Uint64(h[:8]))
	var locked bool
	if err = conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&locked); err != nil {
		return out, err
	}
	if !locked {
		return out, ErrCollectorBusy
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, e := conn.ExecContext(cleanup, `SELECT pg_advisory_unlock($1)`, key); e != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	box, err := open(ctx)
	if err != nil {
		return out, errors.New("IMAP connection/authentication failed")
	}
	defer box.Close()
	snapshot, err := box.Select(ctx)
	if err != nil {
		return out, errors.New("IMAP read-only mailbox selection failed")
	}
	if snapshot.UIDValidity == 0 || snapshot.UIDNext == 0 {
		return out, errors.New("mailbox lacks valid UIDVALIDITY/UIDNEXT")
	}
	out.UIDValidity = snapshot.UIDValidity
	if o.AcceptUIDValidity != 0 && o.AcceptUIDValidity != snapshot.UIDValidity {
		return out, ErrMailboxChanged
	}
	if err = initializeSource(ctx, conn, o, snapshot, &out); err != nil {
		return out, err
	}
	if uint64(snapshot.UIDNext) <= uint64(out.LastUID) {
		if err = collectorAlert(ctx, conn, o.Source, snapshot.UIDValidity, "UIDNEXT_REGRESSED"); err != nil {
			return out, err
		}
		return out, ErrMailboxChanged
	}
	end := uint64(out.LastUID) + uint64(o.Batch)
	if end >= uint64(snapshot.UIDNext) {
		end = uint64(snapshot.UIDNext) - 1
	}
	if end == uint64(out.LastUID) {
		return out, nil
	}
	start := out.LastUID + 1
	metadata, err := box.Metadata(ctx, start, uint32(end))
	if err != nil {
		return out, errors.New("IMAP metadata fetch failed")
	}
	if len(metadata) > o.Batch {
		return out, errors.New("IMAP metadata exceeds requested window")
	}
	sort.Slice(metadata, func(i, j int) bool { return metadata[i].UID < metadata[j].UID })
	for i, m := range metadata {
		if m.UID < start || uint64(m.UID) > end || m.Size < 0 || (i > 0 && m.UID == metadata[i-1].UID) {
			return out, errors.New("invalid IMAP metadata")
		}
	}
	for _, m := range metadata {
		var raw []byte
		outcome := "IMPORTED"
		switch {
		case m.Size > MaxBytes:
			outcome = "OVERSIZE"
		case m.Size == 0:
			outcome = "EMPTY"
		default:
			var present bool
			raw, present, err = box.Body(ctx, m.UID)
			if err != nil {
				return out, errors.New("IMAP body fetch failed; checkpoint retained")
			}
			if !present {
				outcome = "VANISHED"
				raw = nil
			} else if len(raw) > MaxBytes {
				outcome = "OVERSIZE"
				raw = nil
			} else if int64(len(raw)) != m.Size {
				return out, errors.New("IMAP body size mismatch; checkpoint retained")
			}
		}
		if err = saveCollected(ctx, conn, o, snapshot.UIDValidity, m, raw, outcome); err != nil {
			return out, err
		}
		out.LastUID = m.UID
		if outcome == "IMPORTED" {
			out.Imported++
		} else {
			out.Issues++
		}
	}
	// Sparse UID ranges are allowed. Advance only after successful complete metadata
	// and all observed messages committed; never use sequence numbers or SEARCH *.
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE feedback_imap_sources SET last_uid=$2 WHERE name=$1`, o.Source, end); err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	out.LastUID = uint32(end)
	return out, nil
}
func initializeSource(ctx context.Context, conn *sql.Conn, o CollectOptions, snapshot MailboxSnapshot, out *Collection) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO feedback_imap_sources(name,identity_sha256,uid_validity) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, o.Source, o.IdentitySHA256, snapshot.UIDValidity)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	var identity string
	var validity uint32
	var records int
	if err = tx.QueryRowContext(ctx, `SELECT identity_sha256,uid_validity,last_uid,records FROM feedback_imap_sources WHERE name=$1`, o.Source).Scan(&identity, &validity, &out.LastUID, &records); err != nil {
		return err
	}
	if identity != o.IdentitySHA256 {
		return ErrMailboxChanged
	}
	if records >= CollectorMaxRecords {
		return ErrCapacity
	}
	if validity != snapshot.UIDValidity {
		if o.AcceptUIDValidity != snapshot.UIDValidity {
			if err = tx.Rollback(); err != nil {
				return err
			}
			if err = collectorAlert(ctx, conn, o.Source, snapshot.UIDValidity, "UIDVALIDITY_CHANGED"); err != nil {
				return err
			}
			return ErrMailboxChanged
		}
		if _, err = tx.ExecContext(ctx, `UPDATE feedback_imap_sources SET uid_validity=$2,last_uid=0 WHERE name=$1`, o.Source, snapshot.UIDValidity); err != nil {
			return err
		}
		out.LastUID = 0
		if err = audit(ctx, tx, "FEEDBACK_IMAP_EPOCH_RESET", "local_cli", map[string]any{"source": o.Source, "old": validity, "new": snapshot.UIDValidity}); err != nil {
			return err
		}
	}
	if n == 1 {
		if err = audit(ctx, tx, "FEEDBACK_IMAP_SOURCE_CREATED", "local_cli", map[string]string{"source": o.Source, "identity_sha256": identity}); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func collectorAlert(ctx context.Context, conn *sql.Conn, source string, validity uint32, kind string) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO feedback_imap_alerts(source,uid_validity,kind) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, source, validity, kind)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		res, err = tx.ExecContext(ctx, `UPDATE feedback_imap_sources SET records=records+1 WHERE name=$1 AND records<10000`, source)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrCapacity
		}
		if err = audit(ctx, tx, "FEEDBACK_IMAP_ALERT", "imap_collector", map[string]any{"source": source, "uid_validity": validity, "kind": kind}); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func saveCollected(ctx context.Context, conn *sql.Conn, o CollectOptions, validity uint32, m MailMetadata, raw []byte, outcome string) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return err
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM feedback_imap_receipts WHERE source=$1 AND uid_validity=$2 AND uid=$3)`, o.Source, validity, m.UID).Scan(&exists); err != nil {
		return err
	}
	if exists { // Explicitly reaccepted historical epoch: do not duplicate or recharge.
		_, err = tx.ExecContext(ctx, `UPDATE feedback_imap_sources SET last_uid=$2 WHERE name=$1`, o.Source, m.UID)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	res, err := tx.ExecContext(ctx, `UPDATE feedback_imap_sources SET stored_bytes=stored_bytes+$2,records=records+1,last_uid=$3 WHERE name=$1 AND records<10000 AND stored_bytes+$2<=104857600`, o.Source, len(raw), m.UID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrCapacity
	}
	id := ""
	if outcome == "IMPORTED" {
		id, _, err = importRaw(ctx, tx, ImportRequest{Source: "imap:" + o.Source, ExternalID: fmt.Sprintf("%s:%d:%d", o.IdentitySHA256, validity, m.UID), Actor: "imap_collector", Reason: "authenticated mailbox read; feedback source remains unverified"}, raw)
		if err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO feedback_imap_receipts(source,uid_validity,uid,outcome,reported_size,feedback_id) VALUES($1,$2,$3,$4,$5,NULLIF($6,'')::uuid)`, o.Source, validity, m.UID, outcome, m.Size, id); err != nil {
		return err
	}
	if err = audit(ctx, tx, "FEEDBACK_IMAP_COLLECTED", "imap_collector", map[string]any{"source": o.Source, "uid_validity": validity, "uid": m.UID, "outcome": outcome, "feedback_id": id}); err != nil {
		return err
	}
	return tx.Commit()
}
