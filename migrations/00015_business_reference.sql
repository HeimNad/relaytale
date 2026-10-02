-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION valid_business_tags(tags TEXT[]) RETURNS boolean LANGUAGE sql IMMUTABLE AS $$
 SELECT tags IS NOT NULL AND cardinality(tags)<=8
 AND (cardinality(tags)=0 OR array_ndims(tags)=1)
 AND NOT EXISTS(SELECT 1 FROM unnest(tags) t WHERE t IS NULL OR t !~ '^[a-z0-9][a-z0-9_-]{0,31}$')
 AND tags=ARRAY(SELECT DISTINCT t COLLATE "C" FROM unnest(tags) t ORDER BY 1)
$$;
-- +goose StatementEnd
ALTER TABLE messages ADD COLUMN client_reference TEXT NOT NULL DEFAULT '',
 ADD COLUMN business_tags TEXT[] NOT NULL DEFAULT '{}',
 ADD CONSTRAINT client_reference_bound CHECK(client_reference='' OR client_reference ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'),
 ADD CONSTRAINT business_tags_bound CHECK(valid_business_tags(business_tags)),
 ADD CONSTRAINT business_api_only CHECK((source_type='api' AND api_key_id IS NOT NULL) OR (client_reference='' AND cardinality(business_tags)=0));
CREATE INDEX messages_business_reference ON messages(api_key_id,client_reference,created_at DESC,id DESC) WHERE source_type='api';
CREATE INDEX messages_business_owner_time ON messages(api_key_id,created_at DESC,id DESC) WHERE source_type='api';
CREATE INDEX messages_business_tags ON messages USING gin(business_tags) WHERE source_type='api';
-- +goose StatementBegin
CREATE FUNCTION protect_business_reference() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.client_reference IS DISTINCT FROM OLD.client_reference OR NEW.business_tags IS DISTINCT FROM OLD.business_tags
 OR ((OLD.client_reference<>'' OR cardinality(OLD.business_tags)>0) AND (NEW.api_key_id IS DISTINCT FROM OLD.api_key_id OR NEW.source_type IS DISTINCT FROM OLD.source_type)) THEN
 RAISE EXCEPTION 'business reference and ownership are immutable'; END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER messages_business_immutable BEFORE UPDATE ON messages FOR EACH ROW EXECUTE FUNCTION protect_business_reference();
ALTER TABLE webhook_endpoints ADD COLUMN schema_version INTEGER NOT NULL DEFAULT 1 CHECK(schema_version IN (1,2));
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION capture_webhook_event() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE endpoint UUID; seq BIGINT; recipient UUID; attempt UUID; version INTEGER; reference TEXT; tags TEXT[]; body JSONB;
BEGIN
 IF NEW.event_type NOT IN ('MESSAGE_QUEUED','MESSAGE_SUPPRESSED','SMTP_ACCEPTED','ATTEMPT_FINISHED','ATTEMPT_FAILED','DELIVERY_DECIDED','DELIVERY_UNKNOWN','UNKNOWN_RESOLVED','RETRY_EXHAUSTED','RETRY_QUEUED','RECIPIENT_SUPPRESSED','SUPPRESSION_GATE_BLOCKED','PROVIDER_FAILOVER','LEASE_RECOVERED') THEN RETURN NEW; END IF;
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
DO $$ BEGIN RAISE EXCEPTION 'business references and webhook schemas require an explicit downgrade plan'; END $$;
-- +goose StatementEnd
