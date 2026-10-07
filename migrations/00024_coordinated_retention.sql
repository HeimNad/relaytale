-- +goose Up
-- Feedback is unscoped until uniquely associated; serialize its insertion with
-- AVAILABLE -> PURGE_PENDING intent, not with filesystem deletion afterwards.
-- +goose StatementBegin
CREATE FUNCTION retention_feedback_gate() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF current_setting('transaction_isolation') <> 'read committed' THEN
  RAISE EXCEPTION 'retention writes require read committed isolation';
 END IF;
 PERFORM pg_advisory_xact_lock(78320420);
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER feedback_retention_gate BEFORE INSERT ON feedback_reports FOR EACH ROW EXECUTE FUNCTION retention_feedback_gate();

-- +goose StatementBegin
CREATE FUNCTION retention_dependency_gate() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE state TEXT; kind TEXT;
BEGIN
 IF current_setting('transaction_isolation') <> 'read committed' THEN
  RAISE EXCEPTION 'retention writes require read committed isolation';
 END IF;
 IF TG_TABLE_NAME='recipients' THEN
  IF NOT (NEW.status IN ('QUEUED','SENDING','TEMP_FAILED','DELIVERY_UNKNOWN') OR (NEW.status='EXPIRED' AND COALESCE(NEW.decision->>'may_have_delivered','false')='true')) THEN RETURN NEW; END IF;
 END IF;
 IF TG_TABLE_NAME='delivery_attempts' THEN
  IF NEW.result<>'IN_PROGRESS' THEN RETURN NEW; END IF;
 END IF;
 IF TG_TABLE_NAME='webhook_outbox' THEN
  IF NEW.state='DELIVERED' THEN RETURN NEW; END IF;
  SELECT event_type INTO kind FROM events WHERE id=NEW.event_id;
  -- An archive-removal notification uses retained metadata, never the EML.
  IF kind='ARCHIVE_PURGED' THEN RETURN NEW; END IF;
 END IF;
 SELECT archive_state INTO state FROM messages WHERE id=NEW.message_id FOR KEY SHARE;
 -- The API ledger is inserted before its message in the same transaction.
 -- Its deferred FK and the message PK still prevent reusing an existing ID.
 IF FOUND AND state<>'AVAILABLE' THEN RAISE EXCEPTION 'archive already retiring; new retention dependency refused'; END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER ledger_retention_gate BEFORE INSERT ON api_submission_ledger FOR EACH ROW EXECUTE FUNCTION retention_dependency_gate();
CREATE TRIGGER webhook_retention_gate BEFORE INSERT OR UPDATE OF state ON webhook_outbox FOR EACH ROW EXECUTE FUNCTION retention_dependency_gate();
CREATE TRIGGER recipient_retention_gate BEFORE INSERT OR UPDATE OF message_id,status,decision ON recipients FOR EACH ROW EXECUTE FUNCTION retention_dependency_gate();
CREATE TRIGGER attempt_retention_gate BEFORE INSERT OR UPDATE OF message_id,result ON delivery_attempts FOR EACH ROW EXECUTE FUNCTION retention_dependency_gate();

-- +goose StatementBegin
CREATE FUNCTION retention_message_gate() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.archive_state<>'AVAILABLE' AND (NEW.archive_state='AVAILABLE' OR NEW.locked_by IS NOT NULL OR NEW.status NOT IN ('SMTP_ACCEPTED','PERM_FAILED','DELIVERED','BOUNCED','SUPPRESSED','CANCELLED','EXPIRED','SANDBOX_STORED') OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key) THEN
  RAISE EXCEPTION 'retiring archive cannot be reactivated';
 END IF;
 IF OLD.archive_state='AVAILABLE' AND NEW.archive_state<>'AVAILABLE' THEN
  IF current_setting('transaction_isolation') <> 'read committed' THEN RAISE EXCEPTION 'retention writes require read committed isolation'; END IF;
  PERFORM pg_advisory_xact_lock(78320420);
  -- UPDATE normally holds NO KEY UPDATE, which does not exclude KEY SHARE.
  -- Upgrade before checking dependencies, including for direct intent writes.
  PERFORM id FROM messages WHERE id=NEW.id FOR UPDATE;
  IF NEW.idempotency_key IS NOT NULL
   OR EXISTS(SELECT 1 FROM api_submission_ledger WHERE message_id=NEW.id)
   OR EXISTS(SELECT 1 FROM webhook_outbox WHERE message_id=NEW.id AND state IN ('PENDING','IN_FLIGHT','DEAD'))
   OR EXISTS(SELECT 1 FROM feedback_reports f WHERE NOT EXISTS(SELECT 1 FROM feedback_reviews r WHERE r.feedback_id=f.id))
   OR NEW.locked_by IS NOT NULL
   OR NEW.status NOT IN ('SMTP_ACCEPTED','PERM_FAILED','DELIVERED','BOUNCED','SUPPRESSED','CANCELLED','EXPIRED','SANDBOX_STORED')
   OR EXISTS(SELECT 1 FROM delivery_attempts WHERE message_id=NEW.id AND result='IN_PROGRESS')
   OR EXISTS(SELECT 1 FROM recipients WHERE message_id=NEW.id AND (status IN ('QUEUED','SENDING','TEMP_FAILED','DELIVERY_UNKNOWN') OR (status='EXPIRED' AND decision->>'may_have_delivered'='true'))) THEN
   RAISE EXCEPTION 'archive has protected retention dependencies';
  END IF;
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER message_retention_gate BEFORE UPDATE OF archive_state,status,locked_by,idempotency_key ON messages FOR EACH ROW EXECUTE FUNCTION retention_message_gate();
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'coordinated retention fences require an explicit offline downgrade plan'; END $$;
-- +goose StatementEnd
