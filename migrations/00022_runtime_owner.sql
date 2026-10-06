-- +goose Up
-- Deliberately no expiry: loss of a DB session does not prove SMTP has stopped.
CREATE TABLE runtime_owner (
 singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK(singleton),
 owner_id UUID NOT NULL UNIQUE,
 mode TEXT NOT NULL CHECK(mode IN ('runtime','offline')),
 hostname TEXT NOT NULL CHECK(length(hostname) BETWEEN 1 AND 255),
 process_id INTEGER NOT NULL CHECK(process_id>0),
 backend_pid INTEGER NOT NULL CHECK(backend_pid>0),
 started_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 last_seen_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'Runtime ownership requires an explicit offline downgrade plan'; END $$;
-- +goose StatementEnd
