-- +goose Up
ALTER TABLE delivery_attempts ADD COLUMN data_armed_at TIMESTAMPTZ;
ALTER TABLE delivery_attempts ADD COLUMN claim_token TEXT;
CREATE UNIQUE INDEX one_active_attempt_per_message ON delivery_attempts(message_id) WHERE result='IN_PROGRESS';
CREATE INDEX attempts_provider_active ON delivery_attempts(provider_id,started_at) WHERE result='IN_PROGRESS';
-- +goose Down
DROP INDEX attempts_provider_active;
DROP INDEX one_active_attempt_per_message;
ALTER TABLE delivery_attempts DROP COLUMN claim_token;
ALTER TABLE delivery_attempts DROP COLUMN data_armed_at;
