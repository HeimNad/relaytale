package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRestoreGateBeforeDatabaseAndListeners(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".restore-pending"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	args := os.Args
	defer func() { os.Args = args }()
	os.Args = []string{"relaytale", "--storage-dir", root, "--smtp-addr=", "--database-url=postgres://invalid:invalid@127.0.0.1:1/invalid?sslmode=disable"}
	err := run(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "restore gate") {
		t.Fatal("did not stop before database startup", err)
	}
}
