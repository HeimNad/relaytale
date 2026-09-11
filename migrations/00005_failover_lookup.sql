-- +goose Up
-- Resolve a recipient's latest immutable attempt without scanning all recipients.
CREATE INDEX attempt_recipients_history ON attempt_recipients(recipient_id,attempt_id);

-- +goose Down
DROP INDEX attempt_recipients_history;
