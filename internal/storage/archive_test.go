package storage

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/google/uuid"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func TestArchive(t *testing.T) {
	root := filepath.Join(t.TempDir(), "new", "eml")
	store := LocalStore{Root: root, MaxBytes: 100}
	id := uuid.Must(uuid.NewV7()).String()
	raw := "From: a@example.com\r\n\r\n.line\r\n"
	at := time.Date(2026, 9, 10, 23, 0, 0, 0, time.FixedZone("local", -4*3600))
	a, err := store.Save(context.Background(), id, at, strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if a.Path != filepath.Join(root, "2026/09/11", id+".eml") {
		t.Fatal("archive not UTC partitioned")
	}
	b, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != raw || a.Size != int64(len(raw)) || a.SHA256 != fmt.Sprintf("%x", sha256.Sum256(b)) {
		t.Fatal("raw bytes or integrity metadata changed")
	}
	if _, err := store.Save(context.Background(), id, at, strings.NewReader("overwrite")); err == nil {
		t.Fatal("archive overwritten")
	}
	for _, r := range []io.Reader{strings.NewReader(strings.Repeat("x", 101)), brokenReader{}} {
		if _, err := store.Save(context.Background(), uuid.NewString(), at, r); err == nil {
			t.Fatal("failed input accepted")
		}
	}
	entries, err := os.ReadDir(filepath.Dir(a.Path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatal("partial archive leaked")
	}
}
