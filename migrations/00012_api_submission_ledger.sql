-- +goose Up
CREATE TABLE api_submission_ledger (
 api_key_id UUID NOT NULL REFERENCES api_keys(id),
 key_sha256 TEXT NOT NULL CHECK(key_sha256 ~ '^[0-9a-f]{64}$'),
 request_sha256 TEXT NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 message_id UUID NOT NULL UNIQUE REFERENCES messages(id) DEFERRABLE INITIALLY DEFERRED,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(api_key_id,key_sha256)
);
CREATE TRIGGER api_submission_ledger_append_only BEFORE UPDATE OR DELETE ON api_submission_ledger
 FOR EACH ROW EXECUTE FUNCTION reject_event_mutation();
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'API idempotency history requires an explicit retention and downgrade plan'; END $$;
-- +goose StatementEnd
