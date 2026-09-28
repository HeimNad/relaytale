-- +goose Up
CREATE TABLE webhook_endpoints (
 id UUID PRIMARY KEY,
 api_key_id UUID NOT NULL REFERENCES api_keys(id),
 url TEXT NOT NULL CHECK(length(url)<=2048),
 config_hash TEXT NOT NULL CHECK(config_hash ~ '^[0-9a-f]{64}$'),
 enabled BOOLEAN NOT NULL DEFAULT false
);
CREATE TABLE webhook_outbox (
 sequence BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 endpoint_id UUID NOT NULL REFERENCES webhook_endpoints(id),
 event_id UUID NOT NULL REFERENCES events(id),
 message_id UUID NOT NULL REFERENCES messages(id),
 payload TEXT NOT NULL CHECK(octet_length(payload)<=4096),
 state TEXT NOT NULL DEFAULT 'PENDING' CHECK(state IN ('PENDING','IN_FLIGHT','DELIVERED','DEAD')),
 attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts>=0),
 generation INTEGER NOT NULL DEFAULT 0 CHECK(generation>=0),
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 deadline TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()+interval '24 hours',
 next_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 lease_token UUID,
 lease_until TIMESTAMPTZ,
 UNIQUE(endpoint_id,event_id),
 CHECK((state='IN_FLIGHT')=(lease_token IS NOT NULL AND lease_until IS NOT NULL))
);
CREATE INDEX webhook_outbox_due ON webhook_outbox(endpoint_id,next_at,sequence) WHERE state IN ('PENDING','IN_FLIGHT');
CREATE TABLE webhook_delivery_log (
 id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 sequence BIGINT NOT NULL REFERENCES webhook_outbox(sequence),
 at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 generation INTEGER NOT NULL,
 attempt INTEGER NOT NULL,
 outcome TEXT NOT NULL,
 http_code INTEGER,
 actor TEXT,
 reason TEXT
);
CREATE TRIGGER webhook_log_append_only BEFORE UPDATE OR DELETE ON webhook_delivery_log FOR EACH ROW EXECUTE FUNCTION reject_event_mutation();
-- +goose StatementBegin
CREATE FUNCTION capture_webhook_event() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE endpoint UUID; seq BIGINT; recipient UUID; attempt UUID;
BEGIN
 IF NEW.event_type NOT IN ('MESSAGE_QUEUED','MESSAGE_SUPPRESSED','SMTP_ACCEPTED','ATTEMPT_FINISHED','ATTEMPT_FAILED','DELIVERY_DECIDED','DELIVERY_UNKNOWN','UNKNOWN_RESOLVED','RETRY_EXHAUSTED','RETRY_QUEUED','RECIPIENT_SUPPRESSED','SUPPRESSION_GATE_BLOCKED','PROVIDER_FAILOVER','LEASE_RECOVERED') THEN RETURN NEW; END IF;
 SELECT e.id INTO endpoint FROM webhook_endpoints e JOIN messages m ON m.api_key_id=e.api_key_id
 WHERE m.id=NEW.message_id AND m.source_type='api' AND e.enabled;
 IF endpoint IS NULL THEN RETURN NEW; END IF;
 SELECT id INTO recipient FROM recipients WHERE id=NEW.recipient_id AND message_id=NEW.message_id;
 SELECT id INTO attempt FROM delivery_attempts WHERE id=NEW.attempt_id AND message_id=NEW.message_id;
 seq := nextval(pg_get_serial_sequence('webhook_outbox','sequence'));
 INSERT INTO webhook_outbox(sequence,endpoint_id,event_id,message_id,payload) OVERRIDING SYSTEM VALUE VALUES(seq,endpoint,NEW.id,NEW.message_id,jsonb_build_object('schema_version',1,'event_id',NEW.id,'type',NEW.event_type,'message_id',NEW.message_id,'recipient_id',recipient,'attempt_id',attempt,'occurred_at',NEW.event_time,'sequence',seq,'feedback_availability','NOT_EXPOSED')::text);
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE UNIQUE INDEX webhook_one_endpoint_per_identity ON webhook_endpoints(api_key_id) WHERE enabled;
CREATE TRIGGER events_webhook AFTER INSERT ON events FOR EACH ROW EXECUTE FUNCTION capture_webhook_event();
-- +goose StatementBegin
CREATE FUNCTION protect_webhook_payload() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.endpoint_id<>OLD.endpoint_id OR NEW.event_id<>OLD.event_id OR NEW.message_id<>OLD.message_id OR NEW.sequence<>OLD.sequence OR NEW.created_at<>OLD.created_at OR NEW.payload<>OLD.payload THEN RAISE EXCEPTION 'webhook identity and payload are immutable'; END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER webhook_payload_immutable BEFORE UPDATE ON webhook_outbox FOR EACH ROW EXECUTE FUNCTION protect_webhook_payload();
CREATE TRIGGER webhook_outbox_no_delete BEFORE DELETE ON webhook_outbox FOR EACH ROW EXECUTE FUNCTION reject_event_mutation();
-- +goose StatementBegin
CREATE FUNCTION protect_webhook_endpoint() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.id<>OLD.id OR NEW.api_key_id<>OLD.api_key_id OR NEW.url<>OLD.url THEN RAISE EXCEPTION 'webhook destination and owner are immutable'; END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER webhook_endpoint_immutable BEFORE UPDATE ON webhook_endpoints FOR EACH ROW EXECUTE FUNCTION protect_webhook_endpoint();
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'webhook delivery history requires an explicit downgrade plan'; END $$;
-- +goose StatementEnd
