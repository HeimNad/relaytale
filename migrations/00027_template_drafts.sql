-- +goose Up
CREATE TABLE api_template_drafts (
 api_key_id UUID NOT NULL REFERENCES api_keys(id),
 template_id TEXT NOT NULL CHECK(template_id ~ '^[a-z][a-z0-9_]{0,31}$'),
 revision BIGINT NOT NULL CHECK(revision BETWEEN 1 AND 64),
 version TEXT NOT NULL CHECK(version ~ '^[0-9a-f]{64}$'),
 definition TEXT NOT NULL CHECK(octet_length(definition) BETWEEN 1 AND 32768),
 definition_sha256 TEXT NOT NULL CHECK(definition_sha256=encode(sha256(convert_to(definition,'UTF8')),'hex')),
 saved_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 audit_id UUID NOT NULL UNIQUE REFERENCES maintenance_audit(id),
 PRIMARY KEY(api_key_id,template_id,revision),
 CHECK(jsonb_typeof(definition::jsonb)='object'),
 CHECK((definition::jsonb->>'id') IS NOT DISTINCT FROM template_id),
 CHECK((definition::jsonb->>'version') IS NOT DISTINCT FROM version)
);
CREATE TRIGGER template_drafts_append_only BEFORE UPDATE OR DELETE ON api_template_drafts FOR EACH ROW EXECUTE FUNCTION reject_event_mutation();
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'template draft history requires an explicit downgrade plan'; END $$;
-- +goose StatementEnd
