-- +goose Up
CREATE TABLE auxiliary_owner (
    owner_id UUID PRIMARY KEY,
    slot TEXT NOT NULL UNIQUE CHECK(length(slot) BETWEEN 1 AND 255),
    lock_id BIGINT NOT NULL UNIQUE,
    hostname TEXT NOT NULL CHECK(length(hostname) BETWEEN 1 AND 255),
    process_id INTEGER NOT NULL CHECK(process_id > 0),
    backend_pid INTEGER NOT NULL CHECK(backend_pid > 0),
    started_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'auxiliary ownership requires an explicit offline downgrade plan'; END $$;
-- +goose StatementEnd
