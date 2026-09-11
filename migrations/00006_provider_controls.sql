-- +goose Up
ALTER TABLE provider_health ADD COLUMN circuit_state TEXT NOT NULL DEFAULT 'CLOSED' CHECK(circuit_state IN ('CLOSED','OPEN','HALF_OPEN'));
ALTER TABLE provider_health ADD COLUMN open_until TIMESTAMPTZ;
ALTER TABLE provider_health ADD COLUMN probe_attempt_id UUID REFERENCES delivery_attempts(id);
ALTER TABLE provider_health ADD COLUMN samples_since TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE delivery_attempts ADD COLUMN health_outcome TEXT CHECK(health_outcome IN ('SUCCESS','FAILURE','IGNORED'));
ALTER TABLE delivery_attempts ADD COLUMN health_observed_at TIMESTAMPTZ;
CREATE INDEX attempts_health_window ON delivery_attempts(provider_id,health_observed_at) WHERE health_outcome IS NOT NULL;
CREATE TABLE provider_quota (
 attempt_id UUID PRIMARY KEY REFERENCES delivery_attempts(id),
 provider_id UUID NOT NULL REFERENCES providers(id),
 reserved_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 units INTEGER NOT NULL CHECK(units>0)
);
CREATE INDEX provider_quota_window ON provider_quota(provider_id,reserved_at);
-- Existing recent attempts count conservatively when a configured limit becomes supported.
INSERT INTO provider_quota(attempt_id,provider_id,reserved_at,units)
 SELECT a.id,a.provider_id,a.started_at,count(ar.recipient_id)::int FROM delivery_attempts a
 JOIN attempt_recipients ar ON ar.attempt_id=a.id WHERE a.started_at>now()-interval '24 hours'
 GROUP BY a.id HAVING count(ar.recipient_id)>0;
-- +goose Down
DROP TABLE provider_quota;
DROP INDEX attempts_health_window;
ALTER TABLE delivery_attempts DROP COLUMN health_outcome;
ALTER TABLE delivery_attempts DROP COLUMN health_observed_at;
ALTER TABLE provider_health DROP COLUMN samples_since;
ALTER TABLE provider_health DROP COLUMN probe_attempt_id;
ALTER TABLE provider_health DROP COLUMN open_until;
ALTER TABLE provider_health DROP COLUMN circuit_state;
