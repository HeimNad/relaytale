-- +goose Up
ALTER TABLE providers ADD COLUMN revision BIGINT NOT NULL DEFAULT 1 CHECK(revision>0);
-- All writers, including local provisioning SQL, invalidate stale API revisions.
-- +goose StatementBegin
CREATE FUNCTION advance_provider_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 NEW.revision := OLD.revision + 1;
 RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER provider_revision BEFORE UPDATE ON providers FOR EACH ROW EXECUTE FUNCTION advance_provider_revision();
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'management revisions require an explicit downgrade plan'; END $$;
-- +goose StatementEnd
