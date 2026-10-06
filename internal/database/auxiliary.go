package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// AuxiliaryOwner has no TTL; last_seen_at never authorizes takeover.
type AuxiliaryOwner struct {
	RuntimeOwner
	Slot string `json:"slot"`
}

func CheckNoAuxiliary(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) error {
	var table sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT to_regclass('public.auxiliary_owner')::text`).Scan(&table); err != nil {
		return err
	}
	if !table.Valid {
		return nil
	}
	var occupied bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.auxiliary_owner)`).Scan(&occupied); err != nil {
		return err
	}
	if occupied {
		return ErrRuntimeOccupied
	}
	return nil
}

// AcquireAuxiliary permits online administration alongside Gateway, but holds
// migration admission shared for its entire lifetime. Persistent rows also
// block offline work after session loss. A slot prevents duplicate collectors.
func AcquireAuxiliary(ctx context.Context, db *sql.DB, slot string) (*RuntimeGuard, error) {
	if slot == "" || len(slot) > 255 || !utf8.ValidString(slot) || strings.ContainsRune(slot, 0) {
		return nil, errors.New("invalid auxiliary slot")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			discard(conn)
		}
	}()
	var ok bool
	if err = conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock_shared($1)`, MigrationLockID).Scan(&ok); err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrSchemaBusy
	}
	if err = CheckSchema(ctx, db); err != nil {
		return nil, err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.runtime_owner WHERE mode='offline')`).Scan(&ok); err != nil {
		return nil, err
	}
	if ok {
		return nil, ErrRuntimeOccupied
	}
	if _, err = tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return nil, err
	}
	host, err := os.Hostname()
	if err != nil || host == "" || len(host) > 255 {
		return nil, errors.New("auxiliary hostname unavailable")
	}
	id := uuid.NewString()
	digest := sha256.Sum256([]byte("relaytale.auxiliary/" + id))
	key := int64(binary.BigEndian.Uint64(digest[:8]))
	// A collision refuses acquisition, never weakens exclusion.
	if err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&ok); err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrRuntimeOccupied
	}
	var returned string
	err = tx.QueryRowContext(ctx, `INSERT INTO public.auxiliary_owner(owner_id,slot,lock_id,hostname,process_id,backend_pid) VALUES($1,$2,$3,$4,$5,pg_backend_pid()) ON CONFLICT DO NOTHING RETURNING owner_id`, id, slot, key, host, os.Getpid()).Scan(&returned)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRuntimeOccupied
	}
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	return startOwnershipGuard(conn, id, true), nil
}

func ListAuxiliaryOwners(ctx context.Context, db *sql.DB) ([]AuxiliaryOwner, error) {
	rows, err := db.QueryContext(ctx, `SELECT owner_id,slot,hostname,process_id,backend_pid,started_at,last_seen_at FROM public.auxiliary_owner ORDER BY started_at,owner_id LIMIT 1001`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuxiliaryOwner{}
	for rows.Next() {
		var o AuxiliaryOwner
		o.Mode = "auxiliary"
		if err = rows.Scan(&o.OwnerID, &o.Slot, &o.Hostname, &o.ProcessID, &o.BackendPID, &o.StartedAt, &o.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	if len(out) > 1000 {
		return nil, errors.New("too many auxiliary owners; inspect database offline")
	}
	return out, rows.Err()
}

func ReleaseStaleAuxiliary(ctx context.Context, db *sql.DB, id, actor, reason string) error {
	u, err := uuid.Parse(id)
	if err != nil || u == uuid.Nil || u.String() != id || strings.TrimSpace(actor) == "" || len(actor) > 128 || strings.TrimSpace(reason) == "" || len(reason) > 2048 || !utf8.ValidString(actor+reason) || strings.ContainsRune(actor+reason, 0) {
		return errors.New("invalid ownership release")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ok bool
	if err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock_shared($1)`, MigrationLockID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrSchemaBusy
	}
	var key int64
	if err = tx.QueryRowContext(ctx, `SELECT lock_id FROM public.auxiliary_owner WHERE owner_id=$1 FOR UPDATE`, id).Scan(&key); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1)`, key).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrRuntimeOccupied
	}
	if _, err = tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM public.auxiliary_owner WHERE owner_id=$1`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO public.maintenance_audit(id,action,actor,data) VALUES($1,'AUXILIARY_OWNER_RELEASED',$2,jsonb_build_object('owner_id',$3::text,'reason',$4::text))`, uuid.NewString(), actor, id, reason); err != nil {
		return err
	}
	return tx.Commit()
}
