package operations

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicExport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "export.gz")
	if _, err := WriteAtomic(path, func(w io.Writer) error { io.WriteString(w, "partial"); return errors.New("injected") }); err == nil {
		t.Fatal("writer failure ignored")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial file published")
	}
	report, err := WriteAtomic(path, func(w io.Writer) error { _, err := io.WriteString(w, "hello"); return err })
	if err != nil {
		t.Fatal(err)
	}
	if report.Bytes != 5 || report.SHA256 != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatal(report)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("export permissions")
	}
	if _, err := WriteAtomic(path, func(w io.Writer) error { _, e := io.WriteString(w, "overwrite"); return e }); err == nil {
		t.Fatal("overwrote existing export")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "hello" {
		t.Fatal("original changed")
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".export-*"))
	if len(files) > 0 {
		t.Fatal("temporary files leaked")
	}
}
