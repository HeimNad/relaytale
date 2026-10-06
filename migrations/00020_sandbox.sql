-- +goose Up
ALTER TABLE api_keys ADD COLUMN environment TEXT NOT NULL DEFAULT 'production'
 CHECK(environment IN ('production','sandbox'));
ALTER TABLE messages ADD COLUMN environment TEXT NOT NULL DEFAULT 'production'
 CHECK(environment IN ('production','sandbox')),
 ADD CONSTRAINT sandbox_api_only CHECK(environment='production' OR (source_type='api' AND api_key_id IS NOT NULL)),
 ADD CONSTRAINT sandbox_never_queued CHECK((environment='sandbox')=(status='SANDBOX_STORED'));
ALTER TABLE messages DROP CONSTRAINT messages_status_check;
ALTER TABLE messages ADD CONSTRAINT messages_status_check CHECK(status IN ('RECEIVED','ARCHIVED','QUEUED','SENDING','SMTP_ACCEPTED','PARTIAL_ACCEPTED','TEMP_FAILED','PERM_FAILED','DELIVERY_UNKNOWN','BOUNCED','DELIVERED','SUPPRESSED','CANCELLED','EXPIRED','SANDBOX_STORED'));
ALTER TABLE recipients DROP CONSTRAINT recipients_status_check;
ALTER TABLE recipients ADD CONSTRAINT recipients_status_check CHECK(status IN ('QUEUED','SENDING','SMTP_ACCEPTED','TEMP_FAILED','PERM_FAILED','DELIVERY_UNKNOWN','BOUNCED','DELIVERED','SUPPRESSED','CANCELLED','EXPIRED','SANDBOX_STORED'));
-- +goose StatementBegin
CREATE FUNCTION protect_api_environment() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.environment IS DISTINCT FROM OLD.environment THEN RAISE EXCEPTION 'API environment is immutable; create a separate identity'; END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER immutable_api_environment BEFORE UPDATE ON api_keys FOR EACH ROW EXECUTE FUNCTION protect_api_environment();
-- +goose StatementBegin
CREATE FUNCTION protect_message_environment() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE expected TEXT;
BEGIN
 IF TG_OP='UPDATE' THEN
  IF NEW.environment IS DISTINCT FROM OLD.environment OR (OLD.environment='sandbox' AND (NEW.source_type IS DISTINCT FROM OLD.source_type OR NEW.api_key_id IS DISTINCT FROM OLD.api_key_id)) THEN RAISE EXCEPTION 'message environment and ownership are immutable'; END IF;
 END IF;
 IF NEW.source_type='api' THEN
  SELECT environment INTO expected FROM api_keys WHERE id=NEW.api_key_id;
  IF expected IS DISTINCT FROM NEW.environment THEN RAISE EXCEPTION 'message environment differs from API identity'; END IF;
 END IF;
 IF NEW.environment='sandbox' AND (NEW.locked_by IS NOT NULL OR NEW.route_provider_id IS NOT NULL OR NEW.attempt_count<>0 OR NEW.next_attempt_at IS NOT NULL) THEN RAISE EXCEPTION 'sandbox cannot acquire a delivery route or lease'; END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER immutable_message_environment BEFORE INSERT OR UPDATE ON messages FOR EACH ROW EXECUTE FUNCTION protect_message_environment();
-- +goose StatementBegin
CREATE FUNCTION enforce_sandbox_child() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE env TEXT;
BEGIN
 SELECT environment INTO env FROM messages WHERE id=NEW.message_id;
 IF TG_TABLE_NAME='delivery_attempts' THEN
  IF env='sandbox' THEN RAISE EXCEPTION 'sandbox cannot create an SMTP attempt or authorize DATA'; END IF;
 ELSE
  IF (env='sandbox') IS DISTINCT FROM (NEW.status='SANDBOX_STORED') THEN RAISE EXCEPTION 'recipient state differs from message environment'; END IF;
  IF TG_OP='UPDATE' AND NEW.message_id IS DISTINCT FROM OLD.message_id THEN RAISE EXCEPTION 'recipient ownership is immutable'; END IF;
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER sandbox_recipient_guard BEFORE INSERT OR UPDATE ON recipients FOR EACH ROW EXECUTE FUNCTION enforce_sandbox_child();
CREATE TRIGGER sandbox_attempt_guard BEFORE INSERT OR UPDATE ON delivery_attempts FOR EACH ROW EXECUTE FUNCTION enforce_sandbox_child();
-- +goose StatementBegin
CREATE FUNCTION enforce_sandbox_event() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE env TEXT;
BEGIN
 SELECT environment INTO env FROM messages WHERE id=NEW.message_id;
 IF NEW.event_type='SANDBOX_STORED' AND env IS DISTINCT FROM 'sandbox' THEN RAISE EXCEPTION 'simulation event requires sandbox ownership'; END IF;
 IF env='sandbox' AND (NEW.attempt_id IS NOT NULL OR NEW.event_type IN ('SMTP_ACCEPTED','DELIVERED','BOUNCED','ATTEMPT_STARTED','ATTEMPT_FINISHED','ATTEMPT_FAILED','DELIVERY_DECIDED','DELIVERY_UNKNOWN','DATA_ARMED','RETRY_QUEUED','UNKNOWN_RESOLVED','PROVIDER_FAILOVER','MESSAGE_QUEUED')) THEN RAISE EXCEPTION 'sandbox cannot record SMTP delivery facts'; END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER sandbox_event_guard BEFORE INSERT ON events FOR EACH ROW EXECUTE FUNCTION enforce_sandbox_event();
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION capture_webhook_event() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE endpoint UUID; seq BIGINT; recipient UUID; attempt UUID; version INTEGER; reference TEXT; tags TEXT[]; body JSONB; environment TEXT;
BEGIN
 IF NEW.event_type NOT IN ('SANDBOX_STORED','RECIPIENT_EXPIRED','EXPIRY_GATE_BLOCKED','MESSAGE_QUEUED','MESSAGE_SUPPRESSED','SMTP_ACCEPTED','ATTEMPT_FINISHED','ATTEMPT_FAILED','DELIVERY_DECIDED','DELIVERY_UNKNOWN','UNKNOWN_RESOLVED','RETRY_EXHAUSTED','RETRY_QUEUED','RECIPIENT_SUPPRESSED','SUPPRESSION_GATE_BLOCKED','PROVIDER_FAILOVER','LEASE_RECOVERED') THEN RETURN NEW; END IF;
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
DO $$ BEGIN RAISE EXCEPTION 'Sandbox isolation requires an explicit downgrade plan'; END $$;
-- +goose StatementEnd
