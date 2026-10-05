package message

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"relaytale/internal/auth"
)

var ErrIdempotencyConflict = errors.New("idempotency key already belongs to a different request")
var ErrInvalidSubmission = errors.New("invalid idempotent submission")

type EnqueueResult struct {
	MessageID string `json:"message_id"`
	Replayed  bool   `json:"replayed"`
}

// RequestFingerprint covers the exact MIME digest/size and ordered SMTP envelope.
// Nonempty business fields opt into v2; absent fields retain the exact v1 hash.
// Paths, timestamps, generated IDs and parsed MIME metadata are not request identity.
// Callers must derive Archive from durable server-side storage, never client claims.
func RequestFingerprint(s Submission) (string, error) {
	b, e := hex.DecodeString(s.Archive.SHA256)
	if e != nil || len(b) != 32 || hex.EncodeToString(b) != s.Archive.SHA256 || s.Archive.Size < 1 || s.Envelope.From == "" || len(s.Envelope.Recipients) == 0 || len(s.Envelope.Recipients) > 1000 {
		return "", ErrInvalidSubmission
	}
	if _, e := auth.CanonicalAddress(s.Envelope.From); e != nil {
		return "", ErrInvalidSubmission
	}
	for _, address := range s.Envelope.Recipients {
		if _, e := auth.CanonicalAddress(address); e != nil {
			return "", ErrInvalidSubmission
		}
	}
	business, err := s.Business.Canonical()
	if err != nil {
		return "", err
	}
	version := 1
	var association *Business
	if business.Present() {
		version = 2
		association = &business
	}
	stream, err := CanonicalStream(s.Stream)
	if err != nil {
		return "", err
	}
	if stream == "normal" {
		stream = ""
	} else {
		version = 3
	}
	encoded, e := json.Marshal(struct {
		Version    int       `json:"version"`
		From       string    `json:"from"`
		Recipients []string  `json:"recipients"`
		SHA256     string    `json:"sha256"`
		Size       int64     `json:"size"`
		Business   *Business `json:"business,omitempty"`
		Stream     string    `json:"stream,omitempty"`
	}{version, s.Envelope.From, s.Envelope.Recipients, s.Archive.SHA256, s.Archive.Size, association, stream})
	if e != nil {
		return "", e
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// EnqueueIdempotent is an internal persistence primitive, not authentication or
// MIME validation. Callers must authenticate and authorize every request
// (including replays) and bound/archive its validated payload first.
// Concurrent identical requests return the original committed ID; commit errors
// are never converted into success, and the ledger makes a later retry safe.
func (p Postgres) EnqueueIdempotent(ctx context.Context, s Submission, apiKeyID, key string) (EnqueueResult, error) {
	return p.enqueueIdempotent(ctx, s, apiKeyID, key, nil)
}

// publish runs only for a new, budget-admitted request. Archive failures roll
// back the ledger and reservation. Published files survive uncertain commits.
func (p Postgres) enqueueIdempotent(ctx context.Context, s Submission, apiKeyID, key string, publish func(*Submission) error) (EnqueueResult, error) {
	if _, e := uuid.Parse(apiKeyID); e != nil {
		return EnqueueResult{}, ErrInvalidSubmission
	}
	if _, e := uuid.Parse(s.ID); e != nil {
		return EnqueueResult{}, ErrInvalidSubmission
	}
	if len(key) < 1 || len(key) > 128 {
		return EnqueueResult{}, ErrInvalidSubmission
	}
	for _, c := range key {
		if c < 33 || c > 126 {
			return EnqueueResult{}, ErrInvalidSubmission
		}
	}
	fingerprint, e := RequestFingerprint(s)
	if e != nil {
		return EnqueueResult{}, e
	}
	digest := sha256.Sum256([]byte(key))
	keyHash := hex.EncodeToString(digest[:])
	tx, e := p.DB.BeginTx(ctx, nil)
	if e != nil {
		return EnqueueResult{}, e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SET LOCAL synchronous_commit=on`); e != nil {
		return EnqueueResult{}, e
	}
	res, e := tx.ExecContext(ctx, `INSERT INTO api_submission_ledger(api_key_id,key_sha256,request_sha256,message_id) VALUES($1,$2,$3,$4) ON CONFLICT(api_key_id,key_sha256) DO NOTHING`, apiKeyID, keyHash, fingerprint, s.ID)
	if e != nil {
		return EnqueueResult{}, e
	}
	n, e := res.RowsAffected()
	if e != nil {
		return EnqueueResult{}, e
	}
	if n == 0 {
		var original, stored string
		if e = tx.QueryRowContext(ctx, `SELECT message_id,request_sha256 FROM api_submission_ledger WHERE api_key_id=$1 AND key_sha256=$2`, apiKeyID, keyHash).Scan(&original, &stored); e != nil {
			return EnqueueResult{}, e
		}
		if stored != fingerprint {
			return EnqueueResult{}, ErrIdempotencyConflict
		}
		// No writes occurred; return only an already committed ledger entry.
		return EnqueueResult{MessageID: original, Replayed: true}, nil
	}
	if e = reserveSubmission(ctx, tx, apiKeyID, s.ID, len(s.Envelope.Recipients), s.Stream); e != nil {
		return EnqueueResult{}, e
	}
	if publish != nil {
		if e = publish(&s); e != nil {
			return EnqueueResult{}, e
		}
	}
	// API identity is independent of SMTP credentials, including in the ledger.
	s.Envelope.Account.ID = ""
	if e = enqueueTx(ctx, tx, s, "api", apiKeyID, keyHash); e != nil {
		return EnqueueResult{}, e
	}
	if e = tx.Commit(); e != nil {
		return EnqueueResult{}, fmt.Errorf("commit: %w", e)
	}
	return EnqueueResult{MessageID: s.ID}, nil
}
