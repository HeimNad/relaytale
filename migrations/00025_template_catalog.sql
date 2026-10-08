-- +goose Up
CREATE TABLE api_template_versions (
 api_key_id UUID NOT NULL REFERENCES api_keys(id),
 template_id TEXT NOT NULL CHECK (template_id ~ '^[a-z][a-z0-9_]{0,31}$'),
 version TEXT NOT NULL CHECK (version ~ '^[0-9a-f]{64}$'),
 language TEXT NOT NULL DEFAULT '' CHECK (length(language)<=35 AND (language='' OR language ~ '^[a-z]{1,8}(-[a-z0-9]{1,8})*$')),
 definition TEXT NOT NULL CHECK (octet_length(definition) BETWEEN 1 AND 32768),
 definition_sha256 TEXT NOT NULL CHECK (definition_sha256=encode(sha256(convert_to(definition,'UTF8')),'hex')),
 published_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 audit_id UUID NOT NULL UNIQUE REFERENCES maintenance_audit(id),
 PRIMARY KEY (api_key_id,template_id,version),
 CHECK (jsonb_typeof(definition::jsonb)='object'),
 CHECK ((definition::jsonb->>'id') IS NOT DISTINCT FROM template_id),
 CHECK ((definition::jsonb->>'version') IS NOT DISTINCT FROM version),
 CHECK (coalesce(definition::jsonb->>'language','')=language)
);
CREATE TRIGGER template_versions_append_only BEFORE UPDATE OR DELETE ON api_template_versions FOR EACH ROW EXECUTE FUNCTION reject_event_mutation();
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'published template history requires an explicit downgrade plan'; END $$;
-- +goose StatementEnd
