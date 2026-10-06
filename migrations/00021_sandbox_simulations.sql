-- +goose Up
CREATE TABLE sandbox_simulations (
 message_id UUID NOT NULL REFERENCES messages(id),
 key_sha256 TEXT NOT NULL CHECK(key_sha256 ~ '^[0-9a-f]{64}$'),
 recipient_id UUID NOT NULL REFERENCES recipients(id),
 outcome TEXT NOT NULL CHECK(outcome IN ('accepted','temporary_failure','permanent_failure','delivery_unknown','bounce')),
 event_id UUID NOT NULL UNIQUE REFERENCES events(id),
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(message_id,key_sha256)
);
CREATE TRIGGER sandbox_simulations_append_only BEFORE UPDATE OR DELETE ON sandbox_simulations FOR EACH ROW EXECUTE FUNCTION reject_event_mutation();
-- +goose StatementBegin
CREATE FUNCTION validate_sandbox_simulation() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE env TEXT; kind TEXT; n INTEGER;
BEGIN
 SELECT environment INTO env FROM messages WHERE id=NEW.message_id FOR NO KEY UPDATE;
 IF env IS DISTINCT FROM 'sandbox' THEN RAISE EXCEPTION 'simulation requires sandbox ownership'; END IF;
 SELECT count(*) INTO n FROM sandbox_simulations WHERE message_id=NEW.message_id;
 IF n>=100 THEN RAISE EXCEPTION 'simulation limit reached'; END IF;
 kind:=CASE NEW.outcome WHEN 'accepted' THEN 'SANDBOX_SIMULATED_ACCEPTED' WHEN 'temporary_failure' THEN 'SANDBOX_SIMULATED_TEMP_FAILED' WHEN 'permanent_failure' THEN 'SANDBOX_SIMULATED_PERM_FAILED' WHEN 'delivery_unknown' THEN 'SANDBOX_SIMULATED_UNKNOWN' WHEN 'bounce' THEN 'SANDBOX_SIMULATED_BOUNCE' END;
 IF NOT EXISTS(SELECT 1 FROM recipients r JOIN events e ON e.recipient_id=r.id WHERE r.id=NEW.recipient_id AND r.message_id=NEW.message_id AND e.id=NEW.event_id AND e.message_id=NEW.message_id AND e.event_type=kind AND e.source='sandbox' AND e.data=jsonb_build_object('simulation',true,'environment','sandbox','outcome',NEW.outcome)) THEN RAISE EXCEPTION 'simulation evidence mismatch'; END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER sandbox_simulation_membership BEFORE INSERT ON sandbox_simulations FOR EACH ROW EXECUTE FUNCTION validate_sandbox_simulation();
-- +goose StatementBegin
CREATE FUNCTION require_simulation_ledger() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.event_type LIKE 'SANDBOX_SIMULATED_%' AND NOT EXISTS(SELECT 1 FROM sandbox_simulations WHERE event_id=NEW.id) THEN RAISE EXCEPTION 'simulation event needs a committed ledger'; END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE CONSTRAINT TRIGGER sandbox_event_ledger AFTER INSERT ON events DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION require_simulation_ledger();
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION capture_webhook_event() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE endpoint UUID; seq BIGINT; recipient UUID; attempt UUID; version INTEGER; reference TEXT; tags TEXT[]; body JSONB; environment TEXT;
BEGIN
 IF NEW.event_type NOT IN ('SANDBOX_SIMULATED_ACCEPTED','SANDBOX_SIMULATED_TEMP_FAILED','SANDBOX_SIMULATED_PERM_FAILED','SANDBOX_SIMULATED_UNKNOWN','SANDBOX_SIMULATED_BOUNCE','SANDBOX_STORED','RECIPIENT_EXPIRED','EXPIRY_GATE_BLOCKED','MESSAGE_QUEUED','MESSAGE_SUPPRESSED','SMTP_ACCEPTED','ATTEMPT_FINISHED','ATTEMPT_FAILED','DELIVERY_DECIDED','DELIVERY_UNKNOWN','UNKNOWN_RESOLVED','RETRY_EXHAUSTED','RETRY_QUEUED','RECIPIENT_SUPPRESSED','SUPPRESSION_GATE_BLOCKED','PROVIDER_FAILOVER','LEASE_RECOVERED') THEN RETURN NEW; END IF;
 SELECT e.id,e.schema_version,m.client_reference,m.business_tags,m.environment INTO endpoint,version,reference,tags,environment FROM webhook_endpoints e JOIN messages m ON m.api_key_id=e.api_key_id
 WHERE m.id=NEW.message_id AND m.source_type='api' AND e.enabled;
 IF endpoint IS NULL THEN RETURN NEW; END IF;
 SELECT id INTO recipient FROM recipients WHERE id=NEW.recipient_id AND message_id=NEW.message_id;
 SELECT id INTO attempt FROM delivery_attempts WHERE id=NEW.attempt_id AND message_id=NEW.message_id;
 seq := nextval(pg_get_serial_sequence('webhook_outbox','sequence'));
 body:=jsonb_build_object('schema_version',version,'event_id',NEW.id,'type',NEW.event_type,'message_id',NEW.message_id,'recipient_id',recipient,'attempt_id',attempt,'occurred_at',NEW.event_time,'sequence',seq,'feedback_availability','NOT_EXPOSED');
 IF environment='sandbox' THEN body:=body||jsonb_build_object('environment','sandbox','simulation',true); END IF;
 IF version=2 THEN body:=body||jsonb_build_object('business',jsonb_build_object('client_reference',reference,'tags',tags)); END IF;
 INSERT INTO webhook_outbox(sequence,endpoint_id,event_id,message_id,payload) OVERRIDING SYSTEM VALUE VALUES(seq,endpoint,NEW.id,NEW.message_id,body::text);
 RETURN NEW;
END $$;
-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'Simulation audit history requires an explicit downgrade plan'; END $$;
-- +goose StatementEnd
