package message

import (
	"relaytale/internal/storage"
	"strings"
	"testing"
	"time"
)

func TestExpiryFingerprintCanonicalAndLegacy(t *testing.T) {
	s := Submission{Envelope: Envelope{From: "a@example.test", Recipients: []string{"b@example.test"}}, Archive: storage.Archive{SHA256: strings.Repeat("a", 64), Size: 1}}
	old, err := RequestFingerprint(s)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s.ExpiresAt = at
	one, err := RequestFingerprint(s)
	if err != nil || one == old {
		t.Fatal("deadline absent from fingerprint", err)
	}
	s.ExpiresAt = at.In(time.FixedZone("offset", 3600))
	same, err := RequestFingerprint(s)
	if err != nil || same != one {
		t.Fatal("same instant differs", err)
	}
	s.ExpiresAt = at.Add(time.Second)
	other, err := RequestFingerprint(s)
	if err != nil || other == one {
		t.Fatal("deadlines collided", err)
	}
	s.ExpiresAt = at.Add(time.Nanosecond)
	if _, err = RequestFingerprint(s); err == nil {
		t.Fatal("subsecond deadline accepted")
	}
}
