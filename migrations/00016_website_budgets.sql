-- +goose Up
ALTER TABLE api_keys ADD COLUMN submission_minute_limit INTEGER NOT NULL DEFAULT 0 CHECK(submission_minute_limit>=0);
ALTER TABLE api_keys ADD COLUMN submission_hour_limit INTEGER NOT NULL DEFAULT 0 CHECK(submission_hour_limit>=0);
ALTER TABLE api_keys ADD COLUMN submission_day_limit INTEGER NOT NULL DEFAULT 0 CHECK(submission_day_limit>=0);
ALTER TABLE api_submission_ledger ADD CONSTRAINT api_submission_owner_message UNIQUE(api_key_id,message_id);
CREATE TABLE api_submission_quota (
 message_id UUID PRIMARY KEY REFERENCES messages(id) DEFERRABLE INITIALLY DEFERRED,
 api_key_id UUID NOT NULL REFERENCES api_keys(id),
 reserved_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 units INTEGER NOT NULL CHECK(units BETWEEN 1 AND 1000),
 FOREIGN KEY(api_key_id,message_id) REFERENCES api_submission_ledger(api_key_id,message_id)
);
CREATE INDEX api_submission_quota_window ON api_submission_quota(api_key_id,reserved_at);
-- Preserve recent usage when limits are enabled during an upgrade.
INSERT INTO api_submission_quota(message_id,api_key_id,reserved_at,units)
 SELECT l.message_id,l.api_key_id,l.created_at,count(r.id)::integer
 FROM api_submission_ledger l JOIN recipients r ON r.message_id=l.message_id
 WHERE l.created_at>clock_timestamp()-interval '24 hours'
 GROUP BY l.message_id,l.api_key_id,l.created_at;
CREATE TRIGGER api_submission_quota_append_only BEFORE UPDATE OR DELETE ON api_submission_quota
 FOR EACH ROW EXECUTE FUNCTION reject_event_mutation();
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'Website budget history requires an explicit downgrade plan'; END $$;
-- +goose StatementEnd
