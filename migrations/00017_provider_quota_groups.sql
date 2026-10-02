-- +goose Up
CREATE TABLE provider_quota_groups (
 id UUID PRIMARY KEY,
 name TEXT NOT NULL UNIQUE CHECK(length(name) BETWEEN 1 AND 128),
 hourly_limit INTEGER NOT NULL DEFAULT 0 CHECK(hourly_limit>=0),
 daily_limit INTEGER NOT NULL DEFAULT 0 CHECK(daily_limit>=0),
 revision BIGINT NOT NULL DEFAULT 1 CHECK(revision>0),
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE TRIGGER provider_quota_group_revision BEFORE UPDATE ON provider_quota_groups
 FOR EACH ROW EXECUTE FUNCTION advance_provider_revision();
ALTER TABLE providers ADD COLUMN quota_group_id UUID REFERENCES provider_quota_groups(id);
CREATE INDEX providers_quota_group ON providers(quota_group_id) WHERE quota_group_id IS NOT NULL;
-- Membership is append-only. Existing reservations automatically join the group;
-- changing ports, credentials, health or enabled status cannot remove usage.
-- +goose StatementBegin
CREATE FUNCTION guard_provider_quota_group() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' AND OLD.quota_group_id IS NOT NULL AND NEW.quota_group_id IS DISTINCT FROM OLD.quota_group_id THEN
  RAISE EXCEPTION 'provider quota membership cannot be removed or reassigned';
 END IF;
 IF NEW.quota_group_id IS NOT NULL THEN
  IF TG_OP='INSERT' OR OLD.quota_group_id IS DISTINCT FROM NEW.quota_group_id THEN
   PERFORM id FROM provider_quota_groups WHERE id=NEW.quota_group_id FOR NO KEY UPDATE;
  END IF;
 END IF;
 RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER provider_quota_membership BEFORE INSERT OR UPDATE ON providers
 FOR EACH ROW EXECUTE FUNCTION guard_provider_quota_group();
CREATE VIEW provider_quota_group_usage AS
 SELECT g.*,q.hourly_reserved,q.daily_reserved,'recipient_attempt'::text AS quota_unit
 FROM provider_quota_groups g CROSS JOIN LATERAL (
  SELECT coalesce(sum(q.units) FILTER(WHERE q.reserved_at>clock_timestamp()-interval '1 hour'),0) AS hourly_reserved,
   coalesce(sum(q.units),0) AS daily_reserved
  FROM providers p JOIN provider_quota q ON q.provider_id=p.id
  WHERE p.quota_group_id=g.id AND q.reserved_at>clock_timestamp()-interval '24 hours'
 ) q;
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'Shared provider budget history requires an explicit downgrade plan'; END $$;
-- +goose StatementEnd
