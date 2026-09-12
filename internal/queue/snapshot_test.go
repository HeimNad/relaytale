package queue

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"relaytale/internal/resource"
)

func TestSnapshotSurvivesOriginalReplacementAndReleasesName(t *testing.T) {
	root := t.TempDir()
	raw := []byte("From: one@example.test\r\n\r\n.original\r\n")
	path := filepath.Join(root, "original.eml")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	w := Worker{StorageRoot: root, SnapshotDir: root, MaxBytes: 1024}
	j := Job{Path: path, Size: int64(len(raw)), SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))}
	snapshot, err := w.readVerified(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	if _, err = os.Stat(snapshot.Name()); !os.IsNotExist(err) {
		t.Fatal("snapshot name still reachable")
	}
	if err = os.WriteFile(path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(snapshot)
	if err != nil || string(got) != string(raw) {
		t.Fatal("snapshot changed", err)
	}
	if _, err = w.readVerified(context.Background(), j); err == nil {
		t.Fatal("corrupt original accepted")
	}
	files, err := os.ReadDir(root)
	if err != nil || len(files) != 1 {
		t.Fatal("temporary files leaked", err)
	}
}
func TestSnapshotFailureAndResourceWaitNeverClaim(t *testing.T) {
	l := resource.New(resource.SendMemory, 1024)
	release, _ := l.Acquire(context.Background(), resource.SendMemory, 1024)
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	// Nil DB would panic if admission obtained a lease before waiting.
	worked, err := (Worker{Resources: l, MaxBytes: 1024}).RunOne(ctx)
	if worked || err == nil {
		t.Fatal("resource exhaustion claimed work")
	}
}

func TestSnapshotRejectsExtraBytesAndCancelledContext(t *testing.T) {
	root := t.TempDir()
	raw := []byte("x\r\n")
	path := filepath.Join(root, "original.eml")
	if err := os.WriteFile(path, append(raw, 'x'), 0600); err != nil {
		t.Fatal(err)
	}
	w := Worker{StorageRoot: root, SnapshotDir: root, MaxBytes: int64(len(raw))}
	j := Job{Path: path, Size: int64(len(raw)), SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))}
	if f, err := w.readVerified(context.Background(), j); err == nil {
		f.Close()
		t.Fatal("extra byte accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if f, err := w.readVerified(ctx, j); err == nil {
		f.Close()
		t.Fatal("cancelled verification accepted")
	}
	files, err := os.ReadDir(root)
	if err != nil || len(files) != 1 {
		t.Fatal("snapshot path leaked", err)
	}
}
