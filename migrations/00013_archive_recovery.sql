-- +goose Up
CREATE TABLE archive_observations (
 id UUID PRIMARY KEY, archive_id UUID NOT NULL, root TEXT NOT NULL, path TEXT NOT NULL,
 size BIGINT NOT NULL CHECK(size BETWEEN 0 AND 104857600),
 sha256 TEXT NOT NULL CHECK(sha256 ~ '^[0-9a-f]{64}$'),
 observed_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(root,path,size,sha256), UNIQUE(id,archive_id)
);
CREATE TRIGGER archive_observations_immutable BEFORE UPDATE OR DELETE ON archive_observations
 FOR EACH ROW EXECUTE FUNCTION reject_event_mutation();
CREATE TABLE archive_retirements (
 archive_id UUID PRIMARY KEY, observation_id UUID NOT NULL UNIQUE REFERENCES archive_observations(id),
 actor TEXT NOT NULL CHECK(length(actor) BETWEEN 1 AND 128),
 reason TEXT NOT NULL CHECK(length(reason) BETWEEN 1 AND 1000),
 retired_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY(observation_id,archive_id) REFERENCES archive_observations(id,archive_id)
);
CREATE TRIGGER archive_retirements_immutable BEFORE UPDATE OR DELETE ON archive_retirements
 FOR EACH ROW EXECUTE FUNCTION reject_event_mutation();
CREATE TABLE archive_reclaims (
 archive_id UUID PRIMARY KEY REFERENCES archive_retirements(archive_id),
 completed_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE TRIGGER archive_reclaims_immutable BEFORE UPDATE OR DELETE ON archive_reclaims
 FOR EACH ROW EXECUTE FUNCTION reject_event_mutation();
-- +goose StatementBegin
CREATE FUNCTION reject_retired_archive() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE filename TEXT; path_id UUID;
BEGIN
 IF current_setting('transaction_isolation') <> 'read committed' THEN
  RAISE EXCEPTION 'archive identity writes require read committed isolation';
 END IF;
 filename := regexp_replace(NEW.eml_path, '^.*/', '');
 IF filename ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.eml$' THEN
  path_id := left(filename,36)::uuid;
 END IF;
 IF EXISTS(SELECT 1 FROM archive_retirements WHERE archive_id IN (NEW.id,path_id)) THEN
  RAISE EXCEPTION 'archive identity is retired';
 END IF;
 RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER messages_reject_retired_archive BEFORE INSERT OR UPDATE OF id,eml_path ON messages
 FOR EACH ROW EXECUTE FUNCTION reject_retired_archive();
-- +goose StatementBegin
CREATE FUNCTION fence_archive_retirement() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF current_setting('transaction_isolation') <> 'read committed' THEN
  RAISE EXCEPTION 'archive retirement requires read committed isolation';
 END IF;
 LOCK TABLE messages IN SHARE ROW EXCLUSIVE MODE;
 IF EXISTS(SELECT 1 FROM messages WHERE id=NEW.archive_id) THEN
  RAISE EXCEPTION 'referenced archive cannot be retired';
 END IF;
 RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER archive_retirement_fence BEFORE INSERT ON archive_retirements
 FOR EACH ROW EXECUTE FUNCTION fence_archive_retirement();
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'archive retirement fences require an explicit downgrade plan'; END $$;
-- +goose StatementEnd
