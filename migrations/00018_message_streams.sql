-- +goose Up
CREATE TABLE api_message_streams (
 api_key_id UUID NOT NULL REFERENCES api_keys(id),
 name TEXT NOT NULL CHECK(name IN ('normal','otp','security','receipt')),
 enabled BOOLEAN NOT NULL DEFAULT true,
 weight INTEGER NOT NULL CHECK(weight BETWEEN 1 AND 8),
 minute_limit INTEGER NOT NULL DEFAULT 0 CHECK(minute_limit>=0),
 hour_limit INTEGER NOT NULL DEFAULT 0 CHECK(hour_limit>=0),
 day_limit INTEGER NOT NULL DEFAULT 0 CHECK(day_limit>=0),
 PRIMARY KEY(api_key_id,name),
 CHECK(name<>'normal' OR enabled)
);
INSERT INTO api_message_streams(api_key_id,name,weight) SELECT id,'normal',1 FROM api_keys;
-- +goose StatementBegin
CREATE FUNCTION initialize_normal_stream() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN INSERT INTO api_message_streams(api_key_id,name,weight) VALUES(NEW.id,'normal',1); RETURN NEW; END;
$$;
-- +goose StatementEnd
CREATE TRIGGER api_normal_stream AFTER INSERT ON api_keys FOR EACH ROW EXECUTE FUNCTION initialize_normal_stream();
ALTER TABLE messages ADD COLUMN message_stream TEXT NOT NULL DEFAULT 'normal' CHECK(message_stream IN ('normal','otp','security','receipt'));
ALTER TABLE messages ADD CONSTRAINT smtp_normal_stream CHECK(source_type='api' OR message_stream='normal');
ALTER TABLE api_submission_quota ADD COLUMN message_stream TEXT NOT NULL DEFAULT 'normal' CHECK(message_stream IN ('normal','otp','security','receipt'));
CREATE INDEX api_stream_quota_window ON api_submission_quota(api_key_id,message_stream,reserved_at);
-- +goose StatementBegin
CREATE FUNCTION protect_message_stream() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF NEW.message_stream IS DISTINCT FROM OLD.message_stream THEN RAISE EXCEPTION 'message stream is immutable'; END IF; RETURN NEW; END;
$$;
-- +goose StatementEnd
CREATE TRIGGER immutable_message_stream BEFORE UPDATE ON messages FOR EACH ROW EXECUTE FUNCTION protect_message_stream();
-- Durable per-identity turns. A sequence allocates order, not delivery IDs; gaps
-- on rollback are harmless and do not consume a committed owner turn.
CREATE SEQUENCE queue_fair_ticket;
CREATE TABLE queue_fair_owners (
 owner_key TEXT PRIMARY KEY,
 last_ticket BIGINT NOT NULL DEFAULT 0 CHECK(last_ticket>=0),
 stream_cursor INTEGER NOT NULL DEFAULT 0 CHECK(stream_cursor BETWEEN 0 AND 31)
);
-- +goose StatementBegin
CREATE FUNCTION queue_owner(source TEXT,api_id UUID,smtp_id UUID) RETURNS TEXT
 LANGUAGE sql IMMUTABLE AS $$ SELECT source||':'||coalesce(api_id::text,smtp_id::text,'legacy') $$;
-- +goose StatementEnd
INSERT INTO queue_fair_owners(owner_key) SELECT DISTINCT queue_owner(source_type,api_key_id,smtp_account_id) FROM messages;
-- +goose StatementBegin
CREATE FUNCTION initialize_queue_owner() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN INSERT INTO queue_fair_owners(owner_key) VALUES(queue_owner(NEW.source_type,NEW.api_key_id,NEW.smtp_account_id)) ON CONFLICT DO NOTHING; RETURN NEW; END;
$$;
-- +goose StatementEnd
CREATE TRIGGER message_queue_owner AFTER INSERT OR UPDATE OF source_type,api_key_id,smtp_account_id ON messages FOR EACH ROW EXECUTE FUNCTION initialize_queue_owner();
CREATE INDEX messages_fair_queue ON messages(queue_owner(source_type,api_key_id,smtp_account_id),message_stream,created_at,id) WHERE status='QUEUED';
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'Message stream history requires an explicit downgrade plan'; END $$;
-- +goose StatementEnd
