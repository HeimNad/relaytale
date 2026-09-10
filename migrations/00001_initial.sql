-- +goose Up
CREATE TABLE providers (
 id UUID PRIMARY KEY, name TEXT NOT NULL UNIQUE, enabled BOOLEAN NOT NULL DEFAULT false,
 host TEXT NOT NULL, port INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
 security TEXT NOT NULL CHECK (security IN ('implicit_tls','starttls','plain')),
 username TEXT NOT NULL, password_ciphertext BYTEA NOT NULL, nonce BYTEA NOT NULL,
 priority INTEGER NOT NULL DEFAULT 10, weight INTEGER NOT NULL DEFAULT 1 CHECK (weight > 0),
 hourly_limit INTEGER CHECK (hourly_limit > 0), daily_limit INTEGER CHECK (daily_limit > 0),
 max_connections INTEGER NOT NULL DEFAULT 1 CHECK (max_connections > 0),
 timeout_seconds INTEGER NOT NULL DEFAULT 30 CHECK (timeout_seconds > 0),
 from_domains TEXT[] NOT NULL DEFAULT '{}', health_check_enabled BOOLEAN NOT NULL DEFAULT true,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE smtp_accounts (
 id UUID PRIMARY KEY, username TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL,
 allowed_from TEXT[] NOT NULL DEFAULT '{}', enabled BOOLEAN NOT NULL DEFAULT true,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE api_keys (
 id UUID PRIMARY KEY, name TEXT NOT NULL, key_hash TEXT NOT NULL UNIQUE, prefix TEXT NOT NULL,
 enabled BOOLEAN NOT NULL DEFAULT true, last_used_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE messages (
 id UUID PRIMARY KEY, message_id TEXT, source_type TEXT NOT NULL CHECK (source_type IN ('smtp','api','internal')),
 smtp_account_id UUID REFERENCES smtp_accounts(id), api_key_id UUID REFERENCES api_keys(id),
 envelope_from TEXT NOT NULL, header_from TEXT, subject TEXT,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(), queued_at TIMESTAMPTZ, completed_at TIMESTAMPTZ,
 status TEXT NOT NULL CHECK (status IN ('RECEIVED','ARCHIVED','QUEUED','SENDING','SMTP_ACCEPTED','PARTIAL_ACCEPTED','TEMP_FAILED','PERM_FAILED','DELIVERY_UNKNOWN','BOUNCED','DELIVERED','SUPPRESSED','CANCELLED')),
 eml_path TEXT NOT NULL UNIQUE, eml_size BIGINT NOT NULL CHECK (eml_size >= 0),
 eml_sha256 TEXT NOT NULL CHECK (eml_sha256 ~ '^[0-9a-f]{64}$'),
 idempotency_key TEXT, priority SMALLINT NOT NULL DEFAULT 0, metadata JSONB NOT NULL DEFAULT '{}',
 last_error TEXT, next_attempt_at TIMESTAMPTZ, attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
 locked_at TIMESTAMPTZ, locked_by TEXT, lease_expires_at TIMESTAMPTZ,
 CHECK (idempotency_key IS NULL OR api_key_id IS NOT NULL),
 CHECK ((locked_at IS NULL AND locked_by IS NULL AND lease_expires_at IS NULL) OR
        (locked_at IS NOT NULL AND locked_by IS NOT NULL AND lease_expires_at IS NOT NULL)),
 UNIQUE (api_key_id, idempotency_key)
);
CREATE TABLE recipients (
 id UUID PRIMARY KEY, message_id UUID NOT NULL REFERENCES messages(id), address TEXT NOT NULL,
 recipient_type TEXT NOT NULL CHECK (recipient_type IN ('to','cc','bcc','envelope')),
 status TEXT NOT NULL CHECK (status IN ('QUEUED','SENDING','SMTP_ACCEPTED','TEMP_FAILED','PERM_FAILED','DELIVERY_UNKNOWN','BOUNCED','DELIVERED','SUPPRESSED','CANCELLED')),
 provider_id UUID REFERENCES providers(id), smtp_response TEXT, smtp_code INTEGER,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(), completed_at TIMESTAMPTZ
);
CREATE TABLE delivery_attempts (
 id UUID PRIMARY KEY, message_id UUID NOT NULL REFERENCES messages(id), provider_id UUID NOT NULL REFERENCES providers(id),
 attempt_number INTEGER NOT NULL CHECK (attempt_number > 0), started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 connected_at TIMESTAMPTZ, tls_at TIMESTAMPTZ, authenticated_at TIMESTAMPTZ,
 mail_from_at TIMESTAMPTZ, rcpt_at TIMESTAMPTZ, data_started_at TIMESTAMPTZ,
 data_completed_at TIMESTAMPTZ, final_response_at TIMESTAMPTZ, finished_at TIMESTAMPTZ,
 result TEXT NOT NULL, smtp_code INTEGER, smtp_enhanced_code TEXT, smtp_response TEXT,
 error_class TEXT, error_message TEXT, bytes_sent BIGINT,
 connection_duration_ms INTEGER, tls_duration_ms INTEGER, auth_duration_ms INTEGER,
 data_duration_ms INTEGER, total_duration_ms INTEGER,
 provider_message_id TEXT, remote_host TEXT, remote_ip INET, raw_debug_log TEXT,
 UNIQUE (message_id, attempt_number)
);
-- Preserve RCPT outcomes for each attempt, not only the current recipient state.
CREATE TABLE attempt_recipients (
 attempt_id UUID NOT NULL REFERENCES delivery_attempts(id), recipient_id UUID NOT NULL REFERENCES recipients(id),
 status TEXT NOT NULL, smtp_code INTEGER, smtp_enhanced_code TEXT, smtp_response TEXT,
 PRIMARY KEY (attempt_id, recipient_id)
);
CREATE TABLE events (
 id UUID PRIMARY KEY, message_id UUID NOT NULL REFERENCES messages(id),
 recipient_id UUID REFERENCES recipients(id), attempt_id UUID REFERENCES delivery_attempts(id),
 event_type TEXT NOT NULL, event_time TIMESTAMPTZ NOT NULL DEFAULT now(),
 source TEXT NOT NULL, data JSONB NOT NULL DEFAULT '{}'
);
-- +goose StatementBegin
CREATE FUNCTION reject_event_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'event ledger is append-only';
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER events_append_only BEFORE UPDATE OR DELETE ON events FOR EACH ROW EXECUTE FUNCTION reject_event_mutation();
CREATE TABLE provider_health (
 provider_id UUID PRIMARY KEY REFERENCES providers(id),
 status TEXT NOT NULL CHECK (status IN ('HEALTHY','DEGRADED','UNHEALTHY','DISABLED')),
 last_success_at TIMESTAMPTZ, last_failure_at TIMESTAMPTZ, last_error TEXT,
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE suppression_entries (
 id UUID PRIMARY KEY, email TEXT NOT NULL, reason TEXT NOT NULL CHECK (reason IN ('hard_bounce','complaint','manual','invalid','unsubscribe')),
 source TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), expires_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX suppression_email_idx ON suppression_entries (lower(email));
CREATE INDEX messages_created_idx ON messages (created_at DESC);
CREATE INDEX messages_status_idx ON messages (status);
CREATE INDEX messages_rfc_id_idx ON messages (message_id);
CREATE INDEX messages_queue_idx ON messages (priority DESC, created_at, next_attempt_at) WHERE status IN ('QUEUED','TEMP_FAILED');
CREATE INDEX messages_lease_idx ON messages (lease_expires_at) WHERE status = 'SENDING';
CREATE INDEX recipients_address_idx ON recipients (address);
CREATE INDEX recipients_message_idx ON recipients (message_id);
CREATE INDEX events_timeline_idx ON events (message_id, event_time, id);
CREATE INDEX providers_enabled_idx ON providers (enabled, priority);

-- +goose Down
DROP TABLE suppression_entries, provider_health;
DROP TABLE events;
DROP FUNCTION reject_event_mutation();
DROP TABLE attempt_recipients, delivery_attempts, recipients, messages, api_keys, smtp_accounts, providers;
