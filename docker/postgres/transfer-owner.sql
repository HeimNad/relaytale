-- Dedicated RelayTale database only; no passwords and no cluster-wide
-- REASSIGN OWNED. Run offline as the existing owner/admin with -v owner_role=...
\set ON_ERROR_STOP on
BEGIN;
SELECT set_config('relaytale.owner_role', :'owner_role', true);
DO $$
DECLARE owner_name text := current_setting('relaytale.owner_role'); obj record; occupied boolean;
BEGIN
 IF NOT pg_try_advisory_xact_lock(734219509) THEN RAISE EXCEPTION 'offline admission busy'; END IF;
 IF to_regclass('public.runtime_owner') IS NOT NULL THEN
  EXECUTE 'SELECT EXISTS(SELECT 1 FROM public.runtime_owner)' INTO occupied;
  IF occupied THEN RAISE EXCEPTION 'runtime occupancy must be stopped and reconciled'; END IF;
 END IF;
 IF to_regclass('public.auxiliary_owner') IS NOT NULL THEN
  EXECUTE 'SELECT EXISTS(SELECT 1 FROM public.auxiliary_owner)' INTO occupied;
  IF occupied THEN RAISE EXCEPTION 'auxiliary occupancy must be stopped and reconciled'; END IF;
 END IF;
 IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=owner_name AND rolcanlogin
 AND NOT(rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls))
 THEN RAISE EXCEPTION 'owner must be a dedicated nonadministrative login'; END IF;
 FOR obj IN SELECT c.relname,c.relkind FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='public' AND c.relkind IN ('r','p','S','v','m') ORDER BY CASE WHEN c.relkind='S' THEN 1 ELSE 0 END,c.relname LOOP
  EXECUTE format('ALTER %s public.%I OWNER TO %I',
   CASE obj.relkind WHEN 'S' THEN 'SEQUENCE' WHEN 'v' THEN 'VIEW' WHEN 'm' THEN 'MATERIALIZED VIEW' ELSE 'TABLE' END,
   obj.relname,owner_name);
 END LOOP;
 FOR obj IN SELECT p.proname,pg_get_function_identity_arguments(p.oid) args FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
 WHERE n.nspname='public' AND p.prokind='f' LOOP
  EXECUTE format('ALTER FUNCTION public.%I(%s) OWNER TO %I',obj.proname,obj.args,owner_name);
 END LOOP;
 EXECUTE format('ALTER SCHEMA public OWNER TO %I',owner_name);
 EXECUTE format('ALTER DATABASE %I OWNER TO %I',current_database(),owner_name);
END $$;
COMMIT;
