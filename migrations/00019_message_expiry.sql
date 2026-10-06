-- +goose Up
ALTER TABLE messages ADD COLUMN expires_at TIMESTAMPTZ,
 ADD CONSTRAINT expiry_api_only CHECK(expires_at IS NULL OR (source_type='api' AND api_key_id IS NOT NULL)),
 ADD CONSTRAINT expiry_whole_seconds CHECK(expires_at IS NULL OR (isfinite(expires_at) AND expires_at=date_trunc('second',expires_at)));
CREATE INDEX messages_expiry_pending ON messages(expires_at,id) WHERE expires_at IS NOT NULL AND status IN ('QUEUED','TEMP_FAILED','PARTIAL_ACCEPTED','DELIVERY_UNKNOWN');
ALTER TABLE messages DROP CONSTRAINT messages_status_check;
ALTER TABLE messages ADD CONSTRAINT messages_status_check CHECK(status IN ('RECEIVED','ARCHIVED','QUEUED','SENDING','SMTP_ACCEPTED','PARTIAL_ACCEPTED','TEMP_FAILED','PERM_FAILED','DELIVERY_UNKNOWN','BOUNCED','DELIVERED','SUPPRESSED','CANCELLED','EXPIRED'));
ALTER TABLE recipients DROP CONSTRAINT recipients_status_check;
ALTER TABLE recipients ADD CONSTRAINT recipients_status_check CHECK(status IN ('QUEUED','SENDING','SMTP_ACCEPTED','TEMP_FAILED','PERM_FAILED','DELIVERY_UNKNOWN','BOUNCED','DELIVERED','SUPPRESSED','CANCELLED','EXPIRED'));
ALTER TABLE delivery_attempts ADD COLUMN expiry_blocked_at TIMESTAMPTZ,
 ADD CONSTRAINT expiry_before_data CHECK(expiry_blocked_at IS NULL OR (data_armed_at IS NULL AND suppression_blocked_at IS NULL AND data_started_at IS NULL AND final_response_at IS NULL AND coalesce(bytes_sent,0)=0));
-- +goose StatementBegin
CREATE FUNCTION protect_message_expiry() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.expires_at IS DISTINCT FROM OLD.expires_at THEN RAISE EXCEPTION 'message deadline is immutable'; END IF;
 IF OLD.expires_at IS NOT NULL AND (NEW.source_type IS DISTINCT FROM OLD.source_type OR NEW.api_key_id IS DISTINCT FROM OLD.api_key_id) THEN RAISE EXCEPTION 'expiring message ownership is immutable'; END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER immutable_message_expiry BEFORE UPDATE ON messages FOR EACH ROW EXECUTE FUNCTION protect_message_expiry();
-- +goose StatementBegin
CREATE FUNCTION enforce_expiry_authorization() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE deadline TIMESTAMPTZ;
BEGIN
 IF NEW.data_armed_at IS NOT NULL AND OLD.data_armed_at IS NULL THEN
  SELECT expires_at INTO deadline FROM messages WHERE id=NEW.message_id;
  IF deadline IS NOT NULL AND (NEW.data_armed_at>=deadline OR clock_timestamp()>=deadline) THEN
   RAISE EXCEPTION 'expired message cannot authorize DATA';
  END IF;
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER attempt_expiry_authorization BEFORE UPDATE ON delivery_attempts FOR EACH ROW EXECUTE FUNCTION enforce_expiry_authorization();
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION capture_webhook_event() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE endpoint UUID; seq BIGINT; recipient UUID; attempt UUID; version INTEGER; reference TEXT; tags TEXT[]; body JSONB;
BEGIN
 IF NEW.event_type NOT IN ('RECIPIENT_EXPIRED','EXPIRY_GATE_BLOCKED','MESSAGE_QUEUED','MESSAGE_SUPPRESSED','SMTP_ACCEPTED','ATTEMPT_FINISHED','ATTEMPT_FAILED','DELIVERY_DECIDED','DELIVERY_UNKNOWN','UNKNOWN_RESOLVED','RETRY_EXHAUSTED','RETRY_QUEUED','RECIPIENT_SUPPRESSED','SUPPRESSION_GATE_BLOCKED','PROVIDER_FAILOVER','LEASE_RECOVERED') THEN RETURN NEW; END IF;
 SELECT e.id,e.schema_version,m.client_reference,m.business_tags INTO endpoint,version,reference,tags FROM webhook_endpoints e JOIN messages m ON m.api_key_id=e.api_key_id
 WHERE m.id=NEW.message_id AND m.source_type='api' AND e.enabled;
 IF endpoint IS NULL THEN RETURN NEW; END IF;
 SELECT id INTO recipient FROM recipients WHERE id=NEW.recipient_id AND message_id=NEW.message_id;
 SELECT id INTO attempt FROM delivery_attempts WHERE id=NEW.attempt_id AND message_id=NEW.message_id;
 seq := nextval(pg_get_serial_sequence('webhook_outbox','sequence'));
 body:=jsonb_build_object('schema_version',version,'event_id',NEW.id,'type',NEW.event_type,'message_id',NEW.message_id,'recipient_id',recipient,'attempt_id',attempt,'occurred_at',NEW.event_time,'sequence',seq,'feedback_availability','NOT_EXPOSED');
 IF version=2 THEN body:=body||jsonb_build_object('business',jsonb_build_object('client_reference',reference,'tags',tags)); END IF;
 INSERT INTO webhook_outbox(sequence,endpoint_id,event_id,message_id,payload) OVERRIDING SYSTEM VALUE VALUES(seq,endpoint,NEW.id,NEW.message_id,body::text);
 RETURN NEW;
END $$;
-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'Expiry evidence requires an explicit downgrade plan'; END $$;
-- +goose StatementEnd
