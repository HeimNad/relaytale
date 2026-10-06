package message

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"time"

	"github.com/google/uuid"
	"relaytale/internal/resource"
	"relaytale/internal/storage"
)

var ErrSubmissionBusy = errors.New("submission is already in progress")

type APIReceiver struct {
	Store       ArchiveStore
	Repo        Postgres
	Resources   *resource.Limiter
	SnapshotDir string
	MaxBytes    int64
}

// Receive snapshots the bounded request before publishing any archive. A
// cross-process, per-key lock prevents concurrent HTTP retries publishing files.
func (s APIReceiver) Receive(ctx context.Context, e Envelope, r io.Reader, identity, key string, business ...Business) (EnqueueResult, error) {
	return s.ReceiveStream(ctx, e, r, identity, key, "normal", business...)
}
func (s APIReceiver) ReceiveStream(ctx context.Context, e Envelope, r io.Reader, identity, key, stream string, business ...Business) (EnqueueResult, error) {
	return s.ReceiveExpiring(ctx, e, r, identity, key, stream, time.Time{}, business...)
}

func (s APIReceiver) ReceiveExpiring(ctx context.Context, e Envelope, r io.Reader, identity, key, stream string, expiresAt time.Time, business ...Business) (EnqueueResult, error) {
	if _, err := CanonicalExpiry(expiresAt); err != nil {
		return EnqueueResult{}, err
	}
	stream, streamErr := CanonicalStream(stream)
	if streamErr != nil {
		return EnqueueResult{}, streamErr
	}
	if len(business) > 1 {
		return EnqueueResult{}, ErrInvalidSubmission
	}
	var correlation Business
	if len(business) == 1 {
		correlation = business[0]
	}
	correlation, validationErr := correlation.Canonical()
	if validationErr != nil {
		return EnqueueResult{}, validationErr
	}
	if _, err := uuid.Parse(identity); err != nil {
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
	if !e.Account.Allows(e.From) {
		return EnqueueResult{}, ErrSenderDenied
	}
	if s.MaxBytes < 1 || s.MaxBytes > 100<<20 {
		return EnqueueResult{}, ErrInvalidSubmission
	}
	release, err := s.Resources.Acquire(ctx, resource.ReceiveMemory, 2*(s.MaxBytes+1))
	if err != nil {
		return EnqueueResult{}, err
	}
	defer release()
	f, err := os.CreateTemp(s.SnapshotDir, "relaytale-http-*")
	if err != nil {
		return EnqueueResult{}, err
	}
	defer f.Close()
	// Open-unlink keeps private transient content out of directory listings and
	// guarantees cleanup on process exit as well as ordinary request completion.
	if err = os.Remove(f.Name()); err != nil {
		return EnqueueResult{}, err
	}
	h := sha256.New()
	w := io.MultiWriter(f, h)
	buf := make([]byte, 32<<10)
	var size int64
	previousCR := false
	last := byte(0)
	limited := io.LimitReader(r, s.MaxBytes+1)
	for {
		if err = ctx.Err(); err != nil {
			return EnqueueResult{}, err
		}
		n, readErr := limited.Read(buf)
		if n > 0 {
			size += int64(n)
			if size > s.MaxBytes {
				return EnqueueResult{}, storage.ErrTooLarge
			}
			for _, b := range buf[:n] {
				if (previousCR && b != '\n') || (b == '\n' && !previousCR) {
					return EnqueueResult{}, ErrInvalidMessage
				}
				previousCR = b == '\r'
				last = b
			}
			if _, err = w.Write(buf[:n]); err != nil {
				return EnqueueResult{}, err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return EnqueueResult{}, readErr
		}
	}
	if previousCR || last != '\n' {
		return EnqueueResult{}, ErrInvalidMessage
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return EnqueueResult{}, err
	}
	header, err := readHeader(bufio.NewReader(f))
	if err != nil {
		return EnqueueResult{}, err
	}
	mid, from, subject, err := metadata(header, e.Account)
	if err != nil {
		return EnqueueResult{}, err
	}
	sub := Submission{ExpiresAt: expiresAt, Stream: stream, Business: correlation, Envelope: e, MessageID: mid, HeaderFrom: from, Subject: subject, Archive: storage.Archive{SHA256: hex.EncodeToString(h.Sum(nil)), Size: size}}
	fingerprint, err := RequestFingerprint(sub)
	if err != nil {
		return EnqueueResult{}, err
	}
	keyDigest := sha256.Sum256([]byte(key))
	keyHash := hex.EncodeToString(keyDigest[:])
	lockDigest := sha256.Sum256([]byte("relaytale.http/" + identity + "/" + keyHash))
	lockID := int64(binary.BigEndian.Uint64(lockDigest[:8]))
	conn, err := s.Repo.DB.Conn(ctx)
	if err != nil {
		return EnqueueResult{}, err
	}
	defer conn.Close()
	var locked bool
	if err = conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, lockID).Scan(&locked); err != nil {
		return EnqueueResult{}, err
	}
	if !locked {
		return EnqueueResult{}, ErrSubmissionBusy
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, e := conn.ExecContext(cleanup, `SELECT pg_advisory_unlock($1)`, lockID); e != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	var original, stored string
	err = conn.QueryRowContext(ctx, `SELECT message_id,request_sha256 FROM api_submission_ledger WHERE api_key_id=$1 AND key_sha256=$2`, identity, keyHash).Scan(&original, &stored)
	if err == nil {
		if stored != fingerprint {
			return EnqueueResult{}, ErrIdempotencyConflict
		}
		return EnqueueResult{MessageID: original, Replayed: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return EnqueueResult{}, err
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return EnqueueResult{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return EnqueueResult{}, err
	}
	sub.ID = id.String()
	sub.ReceivedAt = time.Now().UTC()
	// Keep the identity budget lock through archive publication and queue commit.
	// Over-budget requests therefore never publish an archive. Transient snapshots
	// remain bounded and open-unlinked; uncertainty after publication retains EML.
	return s.Repo.enqueueIdempotent(ctx, sub, identity, key, func(current *Submission) error {
		archive, saveErr := s.Store.Save(ctx, current.ID, current.ReceivedAt, f)
		if saveErr != nil {
			return saveErr
		}
		current.Archive = archive
		current.ArchivedAt = time.Now().UTC()
		return nil
	})
}
