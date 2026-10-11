-- +goose Up
ALTER TABLE messages ADD COLUMN cancelled_at TIMESTAMPTZ,
 ADD COLUMN replaces_message_id UUID REFERENCES messages(id),
 ADD CONSTRAINT cancelled_message_terminal CHECK(cancelled_at IS NULL OR (source_type='api' AND environment='production' AND status='CANCELLED' AND locked_by IS NULL AND next_attempt_at IS NULL));
CREATE UNIQUE INDEX messages_one_replacement ON messages(replaces_message_id) WHERE replaces_message_id IS NOT NULL;
-- +goose StatementBegin
CREATE FUNCTION protect_pending_control() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' AND OLD.cancelled_at IS NOT NULL AND (NEW.cancelled_at IS DISTINCT FROM OLD.cancelled_at OR NEW.api_key_id IS DISTINCT FROM OLD.api_key_id OR NEW.source_type IS DISTINCT FROM OLD.source_type) THEN RAISE EXCEPTION 'cancellation is immutable'; END IF;
 IF TG_OP='UPDATE' AND OLD.replaces_message_id IS NOT NULL AND NEW.replaces_message_id IS DISTINCT FROM OLD.replaces_message_id THEN RAISE EXCEPTION 'replacement is immutable'; END IF;
 IF NEW.cancelled_at IS NOT NULL AND EXISTS(SELECT 1 FROM delivery_attempts WHERE message_id=NEW.id AND (result='IN_PROGRESS' OR data_armed_at IS NOT NULL OR data_started_at IS NOT NULL OR final_response_at IS NOT NULL)) THEN RAISE EXCEPTION 'cancellation contradicts delivery evidence'; END IF;
 IF NEW.replaces_message_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM messages prior WHERE prior.id=NEW.replaces_message_id AND prior.cancelled_at IS NOT NULL AND prior.api_key_id=NEW.api_key_id AND NEW.source_type='api' AND NEW.environment='production') THEN RAISE EXCEPTION 'replacement requires same-owner cancellation'; END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER pending_control_guard BEFORE INSERT OR UPDATE ON messages FOR EACH ROW EXECUTE FUNCTION protect_pending_control();
-- +goose StatementBegin
CREATE FUNCTION block_cancelled_attempt() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE cancelled BOOLEAN;
BEGIN
 -- INSERT serializes a new attempt/recipient with cancellation. UPDATE must
 -- not acquire message after an existing child row: recorder/finish use
 -- different child rows and must retain the message -> child lock order.
 IF TG_OP='INSERT' THEN
  SELECT cancelled_at IS NOT NULL INTO cancelled FROM messages WHERE id=NEW.message_id FOR UPDATE;
 ELSE
  SELECT cancelled_at IS NOT NULL INTO cancelled FROM messages WHERE id=NEW.message_id;
 END IF;
 IF cancelled THEN RAISE EXCEPTION 'cancelled message cannot create or change delivery attempts'; END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER cancelled_attempt_guard BEFORE INSERT OR UPDATE OF message_id,result,data_armed_at,data_started_at,data_completed_at,final_response_at,bytes_sent ON delivery_attempts FOR EACH ROW EXECUTE FUNCTION block_cancelled_attempt();
-- +goose StatementBegin
CREATE FUNCTION block_cancelled_recipient() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE cancelled BOOLEAN;
BEGIN
 -- INSERT serializes a new attempt/recipient with cancellation. UPDATE must
 -- not acquire message after an existing child row: recorder/finish use
 -- different child rows and must retain the message -> child lock order.
 IF TG_OP='INSERT' THEN
  SELECT cancelled_at IS NOT NULL INTO cancelled FROM messages WHERE id=NEW.message_id FOR UPDATE;
 ELSE
  SELECT cancelled_at IS NOT NULL INTO cancelled FROM messages WHERE id=NEW.message_id;
 END IF;
 IF cancelled AND NEW.status IN ('QUEUED','TEMP_FAILED','SENDING','SMTP_ACCEPTED','DELIVERY_UNKNOWN','DELIVERED','BOUNCED') THEN RAISE EXCEPTION 'cancelled recipient cannot resume delivery'; END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER cancelled_recipient_guard BEFORE INSERT OR UPDATE ON recipients FOR EACH ROW EXECUTE FUNCTION block_cancelled_recipient();
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION capture_webhook_event() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE endpoint UUID; seq BIGINT; recipient UUID; attempt UUID; version INTEGER; reference TEXT; tags TEXT[]; body JSONB; environment TEXT;
BEGIN
 IF NEW.event_type NOT IN ('MESSAGE_CANCELLED','SANDBOX_SIMULATED_ACCEPTED','SANDBOX_SIMULATED_TEMP_FAILED','SANDBOX_SIMULATED_PERM_FAILED','SANDBOX_SIMULATED_UNKNOWN','SANDBOX_SIMULATED_BOUNCE','SANDBOX_STORED','RECIPIENT_EXPIRED','EXPIRY_GATE_BLOCKED','MESSAGE_QUEUED','MESSAGE_SUPPRESSED','SMTP_ACCEPTED','ATTEMPT_FINISHED','ATTEMPT_FAILED','DELIVERY_DECIDED','DELIVERY_UNKNOWN','UNKNOWN_RESOLVED','RETRY_EXHAUSTED','RETRY_QUEUED','RECIPIENT_SUPPRESSED','SUPPRESSION_GATE_BLOCKED','PROVIDER_FAILOVER','LEASE_RECOVERED') THEN RETURN NEW; END IF;
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
DO $$ BEGIN RAISE EXCEPTION 'cancellation and replacement history requires an explicit downgrade plan'; END $$;
-- +goose StatementEnd
