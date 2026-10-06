package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const runtimeSessionLockID int64 = 734219512

var ErrRuntimeOccupied = errors.New("runtime ownership is occupied; inspect and reconcile before takeover")
var ErrOwnerLost = errors.New("runtime ownership connection lost; ownership retained")

// RuntimeOwner persists across session loss and never expires automatically.
type RuntimeOwner struct {
	Mode       string    `json:"mode"`
	OwnerID    string    `json:"owner_id"`
	Hostname   string    `json:"hostname"`
	ProcessID  int       `json:"process_id"`
	BackendPID int       `json:"backend_pid"`
	StartedAt  time.Time `json:"started_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
}
type RuntimeGuard struct {
	conn   *sql.Conn
	id     string
	cancel context.CancelFunc
	done   chan struct{}
	lost   chan struct{}
}

func discard(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}

func AcquireRuntime(ctx context.Context, db *sql.DB) (*RuntimeGuard, error) {
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
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var locked bool
	if err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock_shared($1)`, MigrationLockID).Scan(&locked); err != nil {
		return nil, err
	}
	if !locked {
		return nil, ErrSchemaBusy
	}
	id, err := insertOwner(ctx, tx, "runtime")
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	} // An uncertain commit intentionally leaves possible ownership behind.
	committed = true
	return startGuard(conn, id), nil
}
func startGuard(conn *sql.Conn, id string) *RuntimeGuard {
	monitor, cancel := context.WithCancel(context.Background())
	g := &RuntimeGuard{conn: conn, id: id, cancel: cancel, done: make(chan struct{}), lost: make(chan struct{})}
	go g.watch(monitor)
	return g
}
func insertOwner(ctx context.Context, tx *sql.Tx, mode string) (string, error) {
	var locked bool
	if err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, runtimeSessionLockID).Scan(&locked); err != nil {
		return "", err
	}
	if !locked {
		return "", ErrRuntimeOccupied
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return "", err
	}
	host, err := os.Hostname()
	if err != nil || host == "" || len(host) > 255 {
		return "", errors.New("runtime hostname unavailable")
	}
	id := uuid.NewString()
	var returned string
	err = tx.QueryRowContext(ctx, `INSERT INTO public.runtime_owner(singleton,owner_id,mode,hostname,process_id,backend_pid) VALUES(true,$1,$2,$3,$4,pg_backend_pid()) ON CONFLICT(singleton) DO NOTHING RETURNING owner_id`, id, mode, host, os.Getpid()).Scan(&returned)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrRuntimeOccupied
	}
	if err != nil {
		return "", err
	}

	return id, nil
}

func (g *RuntimeGuard) Lost() <-chan struct{} { return g.lost }
func (g *RuntimeGuard) watch(ctx context.Context) {
	defer close(g.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		check, cancel := context.WithTimeout(ctx, 2*time.Second)
		var ok bool
		var err error
		if g.id == "" {
			err = g.conn.PingContext(check)
			ok = err == nil
		} else {
			err = g.conn.QueryRowContext(check, `UPDATE public.runtime_owner SET last_seen_at=clock_timestamp() WHERE owner_id=$1 AND backend_pid=pg_backend_pid() RETURNING true`, g.id).Scan(&ok)
		}
		canceled := ctx.Err() != nil
		cancel()
		if canceled {
			return
		}
		if err != nil || !ok {
			close(g.lost)
			return
		}
	}
}
func (g *RuntimeGuard) Retain() { g.cancel(); <-g.done; discard(g.conn) }

// Release is only safe after every listener, operation and worker has stopped.
// If the monitor lost ownership, even graceful shutdown does not authorize takeover.
func (g *RuntimeGuard) Release(ctx context.Context) error {
	g.cancel()
	<-g.done
	defer discard(g.conn)
	select {
	case <-g.lost:
		return ErrOwnerLost
	default:
	}
	if g.id == "" {
		return g.conn.PingContext(ctx)
	}
	tx, err := g.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM public.runtime_owner WHERE owner_id=$1 AND backend_pid=pg_backend_pid()`, g.id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrOwnerLost
	}
	return tx.Commit()
}

// CheckNoRuntime must be called while holding migration admission for a strong
// guarantee. Missing ownership metadata permits first migration / empty restore.
func CheckNoRuntime(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) error {
	var table sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT to_regclass('public.runtime_owner')::text`).Scan(&table); err != nil {
		return err
	}
	if !table.Valid {
		return nil
	}
	var occupied bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.runtime_owner WHERE owner_id::text<>$1 OR mode<>'offline')`, offlineOwner(ctx)).Scan(&occupied); err != nil {
		return err
	}
	if occupied {
		return ErrRuntimeOccupied
	}
	return nil
}

type offlineContextKey struct{}

func offlineOwner(ctx context.Context) string {
	v, _ := ctx.Value(offlineContextKey{}).(string)
	return v
}

// OfflineContext also stops the operation if its ownership session is lost.
func (g *RuntimeGuard) OfflineContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.WithValue(parent, offlineContextKey{}, g.id))
	go func() {
		select {
		case <-g.lost:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

type OfflineGuard struct{ guard *RuntimeGuard }

func AcquireOffline(ctx context.Context, db *sql.DB) (*OfflineGuard, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	acquired := false
	defer func() {
		if !acquired {
			discard(conn)
		}
	}()
	var ok bool
	if err = conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, MigrationLockID).Scan(&ok); err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrSchemaBusy
	}
	if err = CheckNoRuntime(ctx, conn); err != nil {
		return nil, err
	}
	g, err := registerOffline(ctx, conn)
	if err != nil {
		return nil, err
	}
	acquired = true
	return &OfflineGuard{guard: g}, nil
}

// Call only after the migration admission session lock has been acquired.
func registerOffline(ctx context.Context, conn *sql.Conn) (*RuntimeGuard, error) {
	var table sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT to_regclass('public.runtime_owner')::text`).Scan(&table); err != nil {
		return nil, err
	}
	if !table.Valid {
		return startGuard(conn, ""), nil
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	id, err := insertOwner(ctx, tx, "offline")
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return startGuard(conn, id), nil
}
func (g *OfflineGuard) Context(parent context.Context) (context.Context, context.CancelFunc) {
	return g.guard.OfflineContext(parent)
}
func (g *OfflineGuard) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return g.guard.Release(ctx)
}

func ListRuntimeOwner(ctx context.Context, db *sql.DB) (*RuntimeOwner, error) {
	var o RuntimeOwner
	err := db.QueryRowContext(ctx, `SELECT mode,owner_id,hostname,process_id,backend_pid,started_at,last_seen_at FROM public.runtime_owner WHERE singleton`).Scan(&o.Mode, &o.OwnerID, &o.Hostname, &o.ProcessID, &o.BackendPID, &o.StartedAt, &o.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &o, err
}

// ReleaseStaleOwner requires explicit operator proof that the process stopped.
// A still-live dedicated DB session independently prevents accidental release.
func ReleaseStaleOwner(ctx context.Context, db *sql.DB, id, actor, reason string) error {
	u, err := uuid.Parse(id)
	if err != nil || u == uuid.Nil || u.String() != id || strings.TrimSpace(actor) == "" || len(actor) > 128 || strings.TrimSpace(reason) == "" || len(reason) > 2048 || !utf8.ValidString(actor+reason) || strings.ContainsRune(actor+reason, 0) {
		return errors.New("invalid ownership release")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return err
	}
	for _, key := range []int64{MigrationLockID, runtimeSessionLockID} {
		var ok bool
		if err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1)`, key).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return ErrRuntimeOccupied
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM public.runtime_owner WHERE owner_id=$1`, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("ownership changed or not found")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO public.maintenance_audit(id,action,actor,data) VALUES($1,'RUNTIME_OWNER_RELEASED',$2,jsonb_build_object('owner_id',$3::text,'reason',$4::text))`, uuid.NewString(), actor, id, reason); err != nil {
		return err
	}
	return tx.Commit()
}
