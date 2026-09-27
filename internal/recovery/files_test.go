package recovery

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestBundleRejectsUnsafeOrIncompleteContent(t *testing.T) {
	for _, kind := range []string{"missing-manifest", "traversal", "duplicate", "symlink", "extra", "checksum", "version"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			dump := filepath.Join(dir, "database.dump")
			if err := os.WriteFile(dump, []byte("test"), 0600); err != nil {
				t.Fatal(err)
			}
			file, err := digest(context.Background(), dump)
			if err != nil {
				t.Fatal(err)
			}
			file.Path = "database.dump"
			m := Manifest{Version: 1, Schema: schemaHash(), StorageRoot: "/data/eml", WorkingDir: "/", Files: []File{file}}
			switch kind {
			case "traversal":
				m.Files[0].Path = "../escape"
			case "duplicate":
				m.Files = append(m.Files, file)
			case "symlink":
				os.Remove(dump)
				os.Symlink("/etc/hosts", dump)
			case "extra":
				os.WriteFile(filepath.Join(dir, "extra"), nil, 0600)
			case "checksum":
				m.Files[0].SHA256 = "wrong"
			case "version":
				m.Version = 2
			}
			if kind != "missing-manifest" {
				b, _ := json.Marshal(m)
				os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0600)
			}
			if _, err = VerifyBundle(context.Background(), dir); err == nil {
				t.Fatal("unsafe bundle accepted")
			}
		})
	}
}

func TestStartupGate(t *testing.T) {
	root := t.TempDir()
	if err := CheckStartup(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, Marker), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckStartup(root); err == nil {
		t.Fatal("gate ignored")
	}
	os.Remove(filepath.Join(root, Marker))
	os.Symlink("/nonexistent", filepath.Join(root, Marker))
	if err := CheckStartup(root); err == nil {
		t.Fatal("dangling gate ignored")
	}
}
