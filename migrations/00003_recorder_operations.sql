-- +goose Up
ALTER TABLE events ADD COLUMN attempt_sequence INTEGER;
CREATE UNIQUE INDEX events_attempt_sequence ON events(attempt_id,attempt_sequence) WHERE attempt_sequence IS NOT NULL;
ALTER TABLE delivery_attempts ADD COLUMN dns_started_at TIMESTAMPTZ;
ALTER TABLE delivery_attempts ADD COLUMN dns_completed_at TIMESTAMPTZ;
ALTER TABLE delivery_attempts ADD COLUMN timings JSONB NOT NULL DEFAULT '{}';
ALTER TABLE messages ADD COLUMN archive_state TEXT NOT NULL DEFAULT 'AVAILABLE' CHECK (archive_state IN ('AVAILABLE','PURGE_PENDING','PURGED'));
ALTER TABLE messages ADD COLUMN eml_purged_at TIMESTAMPTZ;
CREATE INDEX messages_archive_retention ON messages(completed_at) WHERE archive_state<>'PURGED';
CREATE TABLE maintenance_audit (
 id UUID PRIMARY KEY,
 action TEXT NOT NULL,
 actor TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 data JSONB NOT NULL DEFAULT '{}'
);
CREATE TRIGGER maintenance_append_only BEFORE UPDATE OR DELETE ON maintenance_audit FOR EACH ROW EXECUTE FUNCTION reject_event_mutation();
CREATE INDEX maintenance_audit_time ON maintenance_audit(created_at,id);
-- +goose Down
DROP TABLE maintenance_audit;
DROP INDEX messages_archive_retention;
ALTER TABLE messages DROP COLUMN eml_purged_at;
ALTER TABLE messages DROP COLUMN archive_state;
ALTER TABLE delivery_attempts DROP COLUMN timings;
ALTER TABLE delivery_attempts DROP COLUMN dns_completed_at;
ALTER TABLE delivery_attempts DROP COLUMN dns_started_at;
DROP INDEX events_attempt_sequence;
ALTER TABLE events DROP COLUMN attempt_sequence;
