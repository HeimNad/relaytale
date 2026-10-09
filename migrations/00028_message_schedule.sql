-- +goose Up
ALTER TABLE messages ADD COLUMN schedule_at TIMESTAMPTZ,
 ADD CONSTRAINT schedule_api_only CHECK(schedule_at IS NULL OR (source_type='api' AND api_key_id IS NOT NULL)),
 ADD CONSTRAINT schedule_whole_seconds CHECK(schedule_at IS NULL OR (isfinite(schedule_at) AND schedule_at=date_trunc('second',schedule_at))),
 ADD CONSTRAINT schedule_before_expiry CHECK(schedule_at IS NULL OR expires_at IS NULL OR schedule_at<expires_at);
CREATE INDEX messages_future_schedule ON messages(api_key_id,schedule_at,id) WHERE schedule_at IS NOT NULL AND environment='production';
-- +goose StatementBegin
CREATE FUNCTION protect_message_schedule() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' AND NEW.schedule_at IS DISTINCT FROM OLD.schedule_at THEN RAISE EXCEPTION 'message schedule is immutable'; END IF;
 IF TG_OP='UPDATE' AND OLD.schedule_at IS NOT NULL AND (NEW.source_type IS DISTINCT FROM OLD.source_type OR NEW.api_key_id IS DISTINCT FROM OLD.api_key_id) THEN RAISE EXCEPTION 'scheduled ownership is immutable'; END IF;
 IF NEW.schedule_at IS NOT NULL AND NEW.status='SENDING' AND NEW.schedule_at>clock_timestamp() THEN RAISE EXCEPTION 'scheduled message is not due'; END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER immutable_message_schedule BEFORE INSERT OR UPDATE ON messages FOR EACH ROW EXECUTE FUNCTION protect_message_schedule();
-- +goose StatementBegin
CREATE FUNCTION enforce_schedule_authorization() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE earliest TIMESTAMPTZ;
BEGIN
 IF NEW.data_armed_at IS NOT NULL AND (TG_OP='INSERT' OR OLD.data_armed_at IS NULL) THEN
  SELECT schedule_at INTO earliest FROM messages WHERE id=NEW.message_id;
  IF earliest IS NOT NULL AND (NEW.data_armed_at<earliest OR clock_timestamp()<earliest) THEN RAISE EXCEPTION 'scheduled message cannot authorize early DATA'; END IF;
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER attempt_schedule_authorization BEFORE INSERT OR UPDATE ON delivery_attempts FOR EACH ROW EXECUTE FUNCTION enforce_schedule_authorization();
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'schedule evidence requires an explicit downgrade plan'; END $$;
-- +goose StatementEnd
