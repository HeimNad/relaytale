package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

var ErrTooLarge = errors.New("message exceeds size limit")

type Archive struct {
	Path   string
	Size   int64
	SHA256 string
}
type LocalStore struct {
	Root     string
	MaxBytes int64
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// durableDir syncs each parent, including when another receiver created the
// directory concurrently. A successful file fsync alone cannot persist a path.
func durableDir(path string) error {
	parent := filepath.Dir(path)
	if parent == path {
		return nil
	}
	if err := durableDir(parent); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return syncDir(parent)
}
func (s LocalStore) Save(ctx context.Context, id string, at time.Time, r io.Reader) (Archive, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Archive{}, err
	}
	if s.MaxBytes <= 0 {
		return Archive{}, errors.New("positive archive size limit required")
	}
	dir := filepath.Join(s.Root, at.UTC().Format("2006/01/02"))
	if err := durableDir(dir); err != nil {
		return Archive{}, fmt.Errorf("create archive directory: %w", err)
	}
	f, err := os.CreateTemp(dir, ".incoming-*")
	if err != nil {
		return Archive{}, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(contextReader{ctx, r}, s.MaxBytes+1))
	if err != nil {
		return Archive{}, err
	}
	if n > s.MaxBytes {
		return Archive{}, ErrTooLarge
	}
	if err := ctx.Err(); err != nil {
		return Archive{}, err
	}
	if err := f.Sync(); err != nil {
		return Archive{}, err
	}
	if err := f.Close(); err != nil {
		return Archive{}, err
	}
	path := filepath.Join(dir, id+".eml")
	// Hard-link publication is atomic and cannot overwrite an existing archive.
	// Both names reside in the same filesystem; unlink then fsync persists it.
	if err := os.Link(f.Name(), path); err != nil {
		return Archive{}, err
	}
	if err := os.Remove(f.Name()); err != nil {
		return Archive{}, err
	}
	if err := syncDir(dir); err != nil {
		return Archive{}, err
	}
	return Archive{Path: path, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
