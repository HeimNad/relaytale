package database

import (
	"context"
	"database/sql"
	"errors"
)

var ErrUnsafeRuntimeRole = errors.New("runtime database role is unsafe; use a restricted login with no ownership or role memberships")

// CheckRuntimePrivileges rejects administrative credentials before listeners,
// ownership registration, identity synchronization or any external operation.
// This is a guardrail, not a substitute for PostgreSQL ACLs or trusted DB admins.
func CheckRuntimePrivileges(ctx context.Context, db *sql.DB) error {
	var safe bool
	err := db.QueryRowContext(ctx, `SELECT
 NOT (r.rolsuper OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolbypassrls)
 AND r.rolcanlogin
 AND session_user=current_user
 AND NOT EXISTS(SELECT 1 FROM pg_auth_members WHERE member=r.oid)
 AND NOT pg_has_role(current_user,d.datdba,'MEMBER')
 AND NOT has_database_privilege(current_user,d.oid,'CREATE')
 AND NOT has_database_privilege(current_user,d.oid,'TEMP')
 AND NOT has_schema_privilege(current_user,'public','CREATE')
 AND NOT EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='public' AND pg_has_role(current_user,c.relowner,'USAGE'))
 AND NOT EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
 WHERE n.nspname='public' AND pg_has_role(current_user,p.proowner,'USAGE'))
 AND NOT EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
 WHERE n.nspname='public' AND p.prosecdef AND has_function_privilege(current_user,p.oid,'EXECUTE'))
 AND has_table_privilege(current_user,'public.goose_db_version','SELECT')
 AND NOT has_table_privilege(current_user,'public.goose_db_version','INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
 AND NOT has_any_column_privilege(current_user,'public.goose_db_version','INSERT,UPDATE,REFERENCES')
 AND NOT EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='public' AND c.relkind IN ('r','p') AND has_table_privilege(current_user,c.oid,'TRUNCATE,TRIGGER'))
 FROM pg_roles r JOIN pg_database d ON d.datname=current_database() WHERE r.rolname=current_user`).Scan(&safe)
	if err != nil {
		return errors.New("runtime privilege verification unavailable; provision schema and runtime grants offline")
	}
	if !safe {
		return ErrUnsafeRuntimeRole
	}
	return nil
}

// Offline DDL and restore need the dedicated database owner's authority. The
// runtime account is refused even when no migrations are currently pending.
func CheckOfflinePrivileges(ctx context.Context, db *sql.DB) error {
	var allowed bool
	if err := db.QueryRowContext(ctx, `SELECT pg_has_role(current_user,datdba,'USAGE') FROM pg_database WHERE datname=current_database()`).Scan(&allowed); err != nil {
		return errors.New("offline privilege verification unavailable")
	}
	if !allowed {
		return errors.New("offline commands require the dedicated database owner; runtime credentials refused")
	}
	return nil
}
