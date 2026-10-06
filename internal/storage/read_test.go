package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestReadVerifiedRejectsCorruptionSizeAndEscapingLinks(t *testing.T) {
	ctx := context.Background()
	store := LocalStore{Root: t.TempDir(), MaxBytes: 1024}
	archive, err := store.Save(ctx, uuid.NewString(), time.Now(), strings.NewReader("original\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := store.ReadVerified(ctx, archive, 1024); err != nil || string(raw) != "original\r\n" {
		t.Fatal(err)
	}
	if _, err = store.ReadVerified(ctx, archive, 1); err == nil {
		t.Fatal("size cap ignored")
	}
	if err = os.WriteFile(archive.Path, []byte("modified\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ReadVerified(ctx, archive, 1024); err == nil {
		t.Fatal("corruption accepted")
	}
	outside := filepath.Join(t.TempDir(), "outside.eml")
	if err = os.WriteFile(outside, []byte("original\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(archive.Path); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, archive.Path); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ReadVerified(ctx, archive, 1024); err == nil {
		t.Fatal("escaped archive root")
	}
}
