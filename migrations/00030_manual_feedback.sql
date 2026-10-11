-- +goose Up
CREATE TABLE feedback_links (
 id UUID PRIMARY KEY,
 feedback_id UUID NOT NULL REFERENCES feedback_reports(id),
 report_index INTEGER NOT NULL CHECK(report_index BETWEEN 0 AND 99),
 message_id UUID NOT NULL REFERENCES messages(id),
 attempt_id UUID NOT NULL REFERENCES delivery_attempts(id),
 recipient_id UUID NOT NULL REFERENCES recipients(id),
 actor TEXT NOT NULL CHECK(length(actor) BETWEEN 1 AND 128),
 reason TEXT NOT NULL CHECK(length(reason) BETWEEN 1 AND 2048),
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(feedback_id,report_index)
);
CREATE INDEX feedback_links_message ON feedback_links(message_id,id);
CREATE TRIGGER feedback_link_append_only BEFORE UPDATE OR DELETE ON feedback_links FOR EACH ROW EXECUTE FUNCTION reject_event_mutation();
CREATE TABLE feedback_link_reviews (
 link_id UUID NOT NULL REFERENCES feedback_links(id),
 revision INTEGER NOT NULL CHECK(revision IN (1,2)),
 action TEXT NOT NULL CHECK(action IN ('CONFIRM_FAILURE','WITHDRAW')),
 actor TEXT NOT NULL CHECK(length(actor) BETWEEN 1 AND 128),
 reason TEXT NOT NULL CHECK(length(reason) BETWEEN 1 AND 2048),
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(link_id,revision),
 CHECK(revision=1 OR action='WITHDRAW')
);
CREATE TRIGGER feedback_link_review_append_only BEFORE UPDATE OR DELETE ON feedback_link_reviews FOR EACH ROW EXECUTE FUNCTION reject_event_mutation();
-- Serialize import, dismissal and website publication without locking workers.
-- +goose StatementBegin
CREATE FUNCTION feedback_publication_fence() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 PERFORM pg_advisory_xact_lock(78320421);
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER feedback_publication_fence BEFORE INSERT ON feedback_reports FOR EACH ROW EXECUTE FUNCTION feedback_publication_fence();
CREATE TRIGGER feedback_dismissal_fence BEFORE INSERT ON feedback_reviews FOR EACH ROW EXECUTE FUNCTION feedback_publication_fence();
-- +goose StatementBegin
CREATE FUNCTION validate_feedback_link() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 PERFORM pg_advisory_xact_lock(78320421);
 IF (SELECT count(*) FROM feedback_links WHERE message_id=NEW.message_id)>=100 THEN RAISE EXCEPTION 'message feedback capacity reached'; END IF;
 IF NOT EXISTS(SELECT 1 FROM messages m JOIN recipients r ON r.message_id=m.id JOIN attempt_recipients ar ON ar.recipient_id=r.id JOIN delivery_attempts a ON a.id=ar.attempt_id
 WHERE m.id=NEW.message_id AND m.source_type='api' AND m.environment='production' AND m.archive_state='AVAILABLE' AND r.id=NEW.recipient_id AND a.id=NEW.attempt_id AND a.message_id=m.id AND ar.status='SMTP_ACCEPTED' AND a.finished_at IS NOT NULL AND a.final_response_at IS NOT NULL AND a.smtp_code BETWEEN 200 AND 299)
 THEN RAISE EXCEPTION 'feedback relation requires production SMTP acceptance'; END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER feedback_link_guard BEFORE INSERT ON feedback_links FOR EACH ROW EXECUTE FUNCTION validate_feedback_link();
-- +goose StatementBegin
CREATE FUNCTION validate_feedback_link_review() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE previous TEXT;
BEGIN
 PERFORM pg_advisory_xact_lock(78320421);
 SELECT action INTO previous FROM feedback_link_reviews WHERE link_id=NEW.link_id ORDER BY revision DESC LIMIT 1;
 IF (NEW.revision=1 AND previous IS NOT NULL) OR (NEW.revision=2 AND previous IS DISTINCT FROM 'CONFIRM_FAILURE') THEN RAISE EXCEPTION 'feedback review revision conflict'; END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER feedback_link_review_guard BEFORE INSERT ON feedback_link_reviews FOR EACH ROW EXECUTE FUNCTION validate_feedback_link_review();
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION capture_webhook_event() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE endpoint UUID; seq BIGINT; recipient UUID; attempt UUID; version INTEGER; reference TEXT; tags TEXT[]; body JSONB; environment TEXT;
BEGIN
 IF NEW.event_type NOT IN ('FEEDBACK_LINKED','FEEDBACK_REVIEWED','FEEDBACK_INVALIDATED','MESSAGE_CANCELLED','SANDBOX_SIMULATED_ACCEPTED','SANDBOX_SIMULATED_TEMP_FAILED','SANDBOX_SIMULATED_PERM_FAILED','SANDBOX_SIMULATED_UNKNOWN','SANDBOX_SIMULATED_BOUNCE','SANDBOX_STORED','RECIPIENT_EXPIRED','EXPIRY_GATE_BLOCKED','MESSAGE_QUEUED','MESSAGE_SUPPRESSED','SMTP_ACCEPTED','ATTEMPT_FINISHED','ATTEMPT_FAILED','DELIVERY_DECIDED','DELIVERY_UNKNOWN','UNKNOWN_RESOLVED','RETRY_EXHAUSTED','RETRY_QUEUED','RECIPIENT_SUPPRESSED','SUPPRESSION_GATE_BLOCKED','PROVIDER_FAILOVER','LEASE_RECOVERED') THEN RETURN NEW; END IF;
 SELECT e.id,e.schema_version,m.client_reference,m.business_tags,m.environment INTO endpoint,version,reference,tags,environment FROM webhook_endpoints e JOIN messages m ON m.api_key_id=e.api_key_id
 WHERE m.id=NEW.message_id AND m.source_type='api' AND e.enabled;
 IF endpoint IS NULL THEN RETURN NEW; END IF;
 SELECT id INTO recipient FROM recipients WHERE id=NEW.recipient_id AND message_id=NEW.message_id;
 SELECT id INTO attempt FROM delivery_attempts WHERE id=NEW.attempt_id AND message_id=NEW.message_id;
 seq := nextval(pg_get_serial_sequence('webhook_outbox','sequence'));
 body:=jsonb_build_object('schema_version',version,'event_id',NEW.id,'type',NEW.event_type,'message_id',NEW.message_id,'recipient_id',recipient,'attempt_id',attempt,'occurred_at',NEW.event_time,'sequence',seq,'feedback_availability','NOT_EXPOSED');
 IF NEW.event_type IN ('FEEDBACK_LINKED','FEEDBACK_REVIEWED','FEEDBACK_INVALIDATED') THEN body:=body||jsonb_build_object('feedback_availability','MANUAL_REVIEW','feedback_id',NEW.data->>'feedback_link_id'); END IF;
 IF environment='sandbox' THEN body:=body||jsonb_build_object('environment','sandbox','simulation',true); END IF;
 IF version=2 THEN body:=body||jsonb_build_object('business',jsonb_build_object('client_reference',reference,'tags',tags)); END IF;
 INSERT INTO webhook_outbox(sequence,endpoint_id,event_id,message_id,payload) OVERRIDING SYSTEM VALUE VALUES(seq,endpoint,NEW.id,NEW.message_id,body::text);
 RETURN NEW;
END $$;
-- +goose StatementEnd

-- Late conflicting evidence or global dismissal invalidates the human conclusion.
-- No recipient status or suppression row is mutated.
-- +goose StatementBegin
CREATE FUNCTION invalidate_feedback_publication() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE linked RECORD;
BEGIN
 FOR linked IN SELECT l.* FROM feedback_links l JOIN feedback_reports f ON f.id=l.feedback_id
 WHERE (TG_TABLE_NAME='feedback_reviews' AND f.id=(to_jsonb(NEW)->>'feedback_id')::uuid)
 OR (TG_TABLE_NAME='feedback_reports' AND f.source=to_jsonb(NEW)->>'source' AND f.external_id=to_jsonb(NEW)->>'external_id' AND f.sha256<>to_jsonb(NEW)->>'sha256')
 LOOP
  INSERT INTO events(id,message_id,attempt_id,recipient_id,event_type,source,data)
  VALUES(gen_random_uuid(),linked.message_id,linked.attempt_id,linked.recipient_id,'FEEDBACK_INVALIDATED','operator',jsonb_build_object('feedback_link_id',linked.id,'source_trust','UNVERIFIED'));
 END LOOP;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER feedback_conflict_notification AFTER INSERT ON feedback_reports FOR EACH ROW EXECUTE FUNCTION invalidate_feedback_publication();
CREATE TRIGGER feedback_dismissal_notification AFTER INSERT ON feedback_reviews FOR EACH ROW EXECUTE FUNCTION invalidate_feedback_publication();
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION retention_message_gate() RETURNS trigger LANGUAGE plpgsql AS $$
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
  IF EXISTS(SELECT 1 FROM feedback_links WHERE message_id=NEW.id) OR NEW.idempotency_key IS NOT NULL
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

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'manual feedback history requires an explicit downgrade plan'; END $$;
-- +goose StatementEnd
