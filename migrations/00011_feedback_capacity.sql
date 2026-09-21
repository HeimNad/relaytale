-- +goose Up
-- Freeze writers while backfilling: an old binary's inserts must also be charged.
LOCK TABLE feedback_reports IN SHARE ROW EXCLUSIVE MODE;
CREATE TABLE feedback_capacity (
 singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK(singleton),
 records BIGINT NOT NULL CHECK(records >= 0),
 stored_bytes BIGINT NOT NULL CHECK(stored_bytes >= 0)
);
-- Existing evidence is retained even if it already exceeds the new ceiling.
INSERT INTO feedback_capacity(singleton,records,stored_bytes)
 SELECT true,count(*),coalesce(sum(octet_length(raw)::bigint+octet_length(parsed::text)::bigint),0)
 FROM feedback_reports;
-- +goose StatementBegin
CREATE FUNCTION charge_feedback_capacity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 UPDATE feedback_capacity
 SET records=records+1,stored_bytes=stored_bytes+octet_length(NEW.raw)::bigint+octet_length(NEW.parsed::text)::bigint
 WHERE singleton AND records<100000
 AND stored_bytes<=1073741824-octet_length(NEW.raw)::bigint-octet_length(NEW.parsed::text)::bigint;
 IF NOT FOUND THEN
  RAISE EXCEPTION USING ERRCODE='P7501', MESSAGE='feedback evidence capacity reached';
 END IF;
 RETURN NEW;
END;
$$;
-- +goose StatementEnd
-- AFTER INSERT excludes ON CONFLICT DO NOTHING duplicates. The charge rolls
-- back together with evidence, audit, mailbox receipt and UID checkpoint.
CREATE TRIGGER feedback_capacity_charge AFTER INSERT ON feedback_reports
 FOR EACH ROW EXECUTE FUNCTION charge_feedback_capacity();
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'feedback capacity requires an explicit retention and downgrade plan'; END $$;
-- +goose StatementEnd
