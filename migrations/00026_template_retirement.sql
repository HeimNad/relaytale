-- +goose Up
CREATE TABLE api_template_retirements (
 api_key_id UUID NOT NULL,
 template_id TEXT NOT NULL,
 version TEXT NOT NULL,
 retired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 audit_id UUID NOT NULL UNIQUE REFERENCES maintenance_audit(id),
 PRIMARY KEY(api_key_id,template_id,version),
 FOREIGN KEY(api_key_id,template_id,version) REFERENCES api_template_versions(api_key_id,template_id,version)
);
CREATE TRIGGER template_retirements_append_only BEFORE UPDATE OR DELETE ON api_template_retirements FOR EACH ROW EXECUTE FUNCTION reject_event_mutation();
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'template retirement history requires an explicit downgrade plan'; END $$;
-- +goose StatementEnd
