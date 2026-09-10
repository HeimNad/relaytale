package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStorageProbe(t *testing.T) {
	dir := t.TempDir()
	if err := CheckWritable(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("probe leaves files behind")
	}
	if err := CheckWritable(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing directory accepted")
	}
}
