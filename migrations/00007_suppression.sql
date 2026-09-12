-- +goose Up
ALTER TABLE suppression_entries ADD COLUMN released_at TIMESTAMPTZ;
ALTER TABLE suppression_entries ADD COLUMN created_by TEXT NOT NULL DEFAULT 'legacy_unknown';
ALTER TABLE suppression_entries ADD COLUMN note TEXT NOT NULL DEFAULT '';

DROP INDEX suppression_email_idx;
CREATE UNIQUE INDEX suppression_email_idx ON suppression_entries(lower(email)) WHERE released_at IS NULL;

ALTER TABLE delivery_attempts ADD COLUMN suppression_blocked_at TIMESTAMPTZ;
ALTER TABLE delivery_attempts ADD CONSTRAINT suppression_before_data CHECK (suppression_blocked_at IS NULL OR data_armed_at IS NULL);

-- +goose Down
-- Historical entries may share an address; an automatic downgrade would lose history.
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'suppression history requires an explicit downgrade plan'; END $$;

-- +goose StatementEnd
