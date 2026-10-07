-- Run offline as the dedicated database owner using psql -v runtime_role=...
-- Passwords are never accepted by this script. It does not create roles.
\set ON_ERROR_STOP on
BEGIN;
DO $$ BEGIN
 IF NOT pg_try_advisory_xact_lock(734219509) THEN RAISE EXCEPTION 'offline admission busy'; END IF;
 IF EXISTS(SELECT 1 FROM public.runtime_owner) OR EXISTS(SELECT 1 FROM public.auxiliary_owner) THEN RAISE EXCEPTION 'stop and reconcile all owners first'; END IF;
END $$;
SELECT set_config('relaytale.runtime_role', :'runtime_role', true);
DO $$
DECLARE runtime_name text := current_setting('relaytale.runtime_role'); runtime_oid oid;
BEGIN
 SELECT oid INTO runtime_oid FROM pg_roles WHERE rolname=runtime_name
 AND rolcanlogin AND NOT(rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls);
 IF runtime_oid IS NULL OR runtime_name=current_user
 OR EXISTS(SELECT 1 FROM pg_auth_members WHERE member=runtime_oid)
 OR EXISTS(SELECT 1 FROM pg_database WHERE datname=current_database() AND datdba=runtime_oid)
 OR EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relowner=runtime_oid)
 OR EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public' AND p.proowner=runtime_oid)
 THEN RAISE EXCEPTION 'runtime role must be a separate restricted login without memberships or ownership'; END IF;
 IF NOT EXISTS(SELECT 1 FROM pg_database WHERE datname=current_database() AND pg_has_role(current_user,datdba,'USAGE'))
 THEN RAISE EXCEPTION 'use the dedicated database owner'; END IF;
 IF EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public' AND p.prosecdef)
 THEN RAISE EXCEPTION 'unexpected SECURITY DEFINER function; review explicitly before granting runtime access'; END IF;
END $$;
-- PostgreSQL grants CONNECT/TEMP and function EXECUTE to PUBLIC by default.
SELECT format('REVOKE ALL ON DATABASE %I FROM PUBLIC',current_database()) \gexec
SELECT format('REVOKE ALL ON DATABASE %I FROM %I',current_database(),:'runtime_role') \gexec
SELECT format('GRANT CONNECT ON DATABASE %I TO %I',current_database(),:'runtime_role') \gexec
SELECT format('GRANT CONNECT ON DATABASE %I TO %I',current_database(),:'recovery_role') \gexec
REVOKE ALL ON SCHEMA public FROM PUBLIC;
REVOKE ALL ON SCHEMA public FROM :"runtime_role";
GRANT USAGE ON SCHEMA public TO :"runtime_role";
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM PUBLIC, :"runtime_role";
REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM PUBLIC, :"runtime_role";
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA public FROM PUBLIC, :"runtime_role";
-- No automatic runtime grant to future objects: reapply after offline upgrade.
ALTER DEFAULT PRIVILEGES REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC, :"runtime_role";
ALTER DEFAULT PRIVILEGES REVOKE ALL ON TABLES FROM PUBLIC, :"runtime_role";
ALTER DEFAULT PRIVILEGES REVOKE ALL ON SEQUENCES FROM PUBLIC, :"runtime_role";
ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON TABLES FROM :"runtime_role";
ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON SEQUENCES FROM :"runtime_role";
ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE EXECUTE ON FUNCTIONS FROM :"runtime_role";
GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA public TO :"runtime_role";
GRANT SELECT,USAGE ON ALL SEQUENCES IN SCHEMA public TO :"runtime_role";
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA public TO :"runtime_role";
-- History stays read-only; append-only ledgers need no mutation privilege.
REVOKE ALL ON public.goose_db_version FROM :"runtime_role";
GRANT SELECT ON public.goose_db_version TO :"runtime_role";
REVOKE ALL ON public.goose_db_version_id_seq FROM :"runtime_role";
REVOKE UPDATE,DELETE ON public.events,public.maintenance_audit FROM :"runtime_role";
COMMIT;
