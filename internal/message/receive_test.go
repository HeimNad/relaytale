package message

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"relaytale/internal/auth"
	"relaytale/internal/storage"
	"strings"
	"testing"
	"time"
)

type repoFunc func(context.Context, Submission) error

func (f repoFunc) Enqueue(ctx context.Context, s Submission) error { return f(ctx, s) }

type badStore struct{}

func (badStore) Save(context.Context, string, time.Time, io.Reader) (storage.Archive, error) {
	return storage.Archive{}, errors.New("disk failure")
}
func envelope() Envelope {
	return Envelope{Account: auth.Account{ID: "test", AllowedFrom: []string{"a@example.com"}}, From: "a@example.com", Recipients: []string{"b@example.com"}}
}
func TestReceiveDurability(t *testing.T) {
	raw := "From: A <a@example.com>\r\nMessage-ID: <original@example.com>\r\nSubject: Test\r\n\r\nbody\r\n"
	root := t.TempDir()
	called := false
	r := Receiver{Store: storage.LocalStore{Root: root, MaxBytes: 1024}, Repo: repoFunc(func(ctx context.Context, s Submission) error {
		called = true
		b, err := os.ReadFile(s.Archive.Path)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != raw || s.MessageID != "<original@example.com>" {
			t.Fatal("archive not available unchanged before transaction")
		}
		return errors.New("commit outcome unknown")
	})}
	if _, err := r.Receive(context.Background(), envelope(), strings.NewReader(raw)); err == nil || !called {
		t.Fatal("failed commit acknowledged")
	}
	n := 0
	_ = filepath.Walk(root, func(p string, i os.FileInfo, err error) error {
		if err == nil && !i.IsDir() {
			n++
		}
		return err
	})
	if n != 1 {
		t.Fatal("archive removed despite uncertain commit")
	}
	called = false
	r.Store = badStore{}
	if _, err := r.Receive(context.Background(), envelope(), strings.NewReader(raw)); err == nil || called {
		t.Fatal("transaction ran after archive failure")
	}
}
func TestRejectHeaderSpoof(t *testing.T) {
	r := Receiver{Store: badStore{}, Repo: repoFunc(func(context.Context, Submission) error { t.Fatal("invalid message enqueued"); return nil })}
	for _, raw := range []string{"From: other@example.com\r\n\r\nbody", "From: a@example.com\r\nFrom: other@example.com\r\n\r\nbody", "From: a@example.com\r\nSender: other@example.com\r\n\r\nbody"} {
		_, err := r.Receive(context.Background(), envelope(), strings.NewReader(raw))
		if !errors.Is(err, ErrSenderDenied) && !errors.Is(err, ErrInvalidMessage) {
			t.Fatalf("spoof not rejected: %v", err)
		}
	}
}
