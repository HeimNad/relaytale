-- +goose Up
-- Small, operator-imported evidence is atomic with its audit record. Not an
-- inbound mailbox or a general-purpose EML store. Never drives delivery policy.
CREATE TABLE feedback_reports (
 id UUID PRIMARY KEY,
 source TEXT NOT NULL CHECK(length(source) BETWEEN 1 AND 128),
 external_id TEXT NOT NULL CHECK(length(external_id) BETWEEN 1 AND 256),
 sha256 TEXT NOT NULL CHECK(sha256 ~ '^[0-9a-f]{64}$'),
 raw BYTEA NOT NULL CHECK(octet_length(raw) BETWEEN 1 AND 1048576),
 parsed JSONB NOT NULL,
 parse_state TEXT NOT NULL CHECK(parse_state IN ('PARSED','UNSUPPORTED')),
 trust TEXT NOT NULL DEFAULT 'UNVERIFIED' CHECK(trust='UNVERIFIED'),
 actor TEXT NOT NULL,
 reason TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(source,external_id,sha256)
);
CREATE TRIGGER feedback_append_only BEFORE UPDATE OR DELETE ON feedback_reports FOR EACH ROW EXECUTE FUNCTION reject_event_mutation();
CREATE TABLE feedback_reviews (
 feedback_id UUID PRIMARY KEY REFERENCES feedback_reports(id),
 action TEXT NOT NULL CHECK(action='DISMISS'),
 actor TEXT NOT NULL,
 reason TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE TRIGGER feedback_review_append_only BEFORE UPDATE OR DELETE ON feedback_reviews FOR EACH ROW EXECUTE FUNCTION reject_event_mutation();
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'feedback evidence requires an explicit downgrade and retention plan'; END $$;
-- +goose StatementEnd
