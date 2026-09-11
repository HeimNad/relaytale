-- +goose Up
ALTER TABLE messages ADD COLUMN route_provider_id UUID REFERENCES providers(id);
ALTER TABLE recipients ADD COLUMN retry_started_at TIMESTAMPTZ;
ALTER TABLE recipients ADD COLUMN attempt_count INTEGER NOT NULL DEFAULT 0 CHECK(attempt_count>=0);
ALTER TABLE recipients ADD COLUMN retry_at TIMESTAMPTZ;
ALTER TABLE recipients ADD COLUMN retry_automatic BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE recipients ADD COLUMN decision JSONB;
ALTER TABLE attempt_recipients ADD COLUMN decision JSONB;
ALTER TABLE attempt_recipients ADD COLUMN protocol_stage TEXT;
ALTER TABLE delivery_attempts ADD COLUMN protocol_stage TEXT;
CREATE INDEX recipients_retry_due ON recipients(retry_at,message_id) WHERE retry_at IS NOT NULL;
-- Backfill evidence/counters only; never schedule paused historical messages.
UPDATE recipients r SET attempt_count=(SELECT count(*) FROM attempt_recipients ar WHERE ar.recipient_id=r.id),
 retry_started_at=(SELECT min(a.started_at) FROM attempt_recipients ar JOIN delivery_attempts a ON a.id=ar.attempt_id WHERE ar.recipient_id=r.id);
UPDATE messages m SET route_provider_id=(SELECT a.provider_id FROM delivery_attempts a WHERE a.message_id=m.id ORDER BY a.attempt_number DESC LIMIT 1);
-- +goose Down
DROP INDEX recipients_retry_due;
ALTER TABLE delivery_attempts DROP COLUMN protocol_stage;
ALTER TABLE attempt_recipients DROP COLUMN protocol_stage;
ALTER TABLE attempt_recipients DROP COLUMN decision;
ALTER TABLE recipients DROP COLUMN decision;
ALTER TABLE recipients DROP COLUMN retry_automatic;
ALTER TABLE recipients DROP COLUMN retry_at;
ALTER TABLE recipients DROP COLUMN attempt_count;
ALTER TABLE recipients DROP COLUMN retry_started_at;
ALTER TABLE messages DROP COLUMN route_provider_id;
