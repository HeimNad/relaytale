-- +goose Up
CREATE TABLE feedback_imap_sources (
 name TEXT PRIMARY KEY CHECK(length(name) BETWEEN 1 AND 64),
 identity_sha256 TEXT NOT NULL CHECK(identity_sha256 ~ '^[0-9a-f]{64}$'),
 uid_validity BIGINT NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
 last_uid BIGINT NOT NULL DEFAULT 0 CHECK(last_uid BETWEEN 0 AND 4294967295),
 stored_bytes BIGINT NOT NULL DEFAULT 0 CHECK(stored_bytes BETWEEN 0 AND 104857600),
 records INTEGER NOT NULL DEFAULT 0 CHECK(records BETWEEN 0 AND 10000)
);
CREATE TABLE feedback_imap_receipts (
 source TEXT NOT NULL REFERENCES feedback_imap_sources(name),
 uid_validity BIGINT NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
 uid BIGINT NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
 outcome TEXT NOT NULL CHECK(outcome IN ('IMPORTED','OVERSIZE','EMPTY','VANISHED')),
 reported_size BIGINT NOT NULL CHECK(reported_size>=0),
 feedback_id UUID REFERENCES feedback_reports(id),
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 CHECK((outcome='IMPORTED')=(feedback_id IS NOT NULL)),
 PRIMARY KEY(source,uid_validity,uid)
);
CREATE TRIGGER feedback_imap_receipts_append_only BEFORE UPDATE OR DELETE ON feedback_imap_receipts FOR EACH ROW EXECUTE FUNCTION reject_event_mutation();
CREATE TABLE feedback_imap_alerts (
 source TEXT NOT NULL REFERENCES feedback_imap_sources(name),
 uid_validity BIGINT NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('UIDVALIDITY_CHANGED','UIDNEXT_REGRESSED')),
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(source,uid_validity,kind)
);
CREATE TRIGGER feedback_imap_alerts_append_only BEFORE UPDATE OR DELETE ON feedback_imap_alerts FOR EACH ROW EXECUTE FUNCTION reject_event_mutation();
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'IMAP checkpoints and evidence require an explicit downgrade plan'; END $$;
-- +goose StatementEnd
