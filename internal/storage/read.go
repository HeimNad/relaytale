package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// ReadVerified is for small, bounded inspection payloads, never SMTP delivery.
// OpenRoot prevents an archive pointer or symlink escaping the configured root.
// No bytes are returned until exact size and digest have been verified.
func (s LocalStore) ReadVerified(ctx context.Context, a Archive, limit int64) ([]byte, error) {
	if limit < 1 || a.Size < 1 || a.Size > limit {
		return nil, ErrTooLarge
	}
	rootPath, err := filepath.Abs(s.Root)
	if err != nil {
		return nil, err
	}
	archivePath, err := filepath.Abs(a.Path)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(rootPath, archivePath)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(relative)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("archive must be a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(contextReader{ctx, f}, a.Size+1))
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(raw)
	if int64(len(raw)) != a.Size || hex.EncodeToString(digest[:]) != a.SHA256 {
		return nil, errors.New("archive integrity mismatch")
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return raw, nil
}
