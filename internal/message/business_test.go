package message

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"relaytale/internal/storage"
)

func TestBusinessBoundsAndFingerprintCompatibility(t *testing.T) {
	for _, b := range []Business{{ClientReference: strings.Repeat("x", 129)}, {ClientReference: "user@example.com"}, {ClientReference: "订单"}, {ClientReference: "a\nb"}, {ClientReference: " a"}, {Tags: []string{"A"}}, {Tags: []string{"a", "a"}}, {Tags: []string{"a,b"}}, {Tags: []string{""}}, {Tags: []string{strings.Repeat("a", 33)}}, {Tags: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}}} {
		if _, err := b.Canonical(); err == nil {
			t.Fatal("invalid business accepted", b)
		}
	}
	original := Business{ClientReference: strings.Repeat("R", 128), Tags: []string{"z", "a"}}
	canon, err := original.Canonical()
	if err != nil || !slices.Equal(canon.Tags, []string{"a", "z"}) || original.Tags[0] != "z" {
		t.Fatal(canon, err)
	}
	s := Submission{Envelope: Envelope{From: "from@example.test", Recipients: []string{"a@example.test"}}, Archive: storage.Archive{SHA256: strings.Repeat("a", 64), Size: 42}}
	// Frozen pre-8E.1 encoding, not produced using the new fingerprint struct.
	legacy := `{"version":1,"from":"from@example.test","recipients":["a@example.test"],"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":42}`
	digest := sha256.Sum256([]byte(legacy))
	want := hex.EncodeToString(digest[:])
	got, err := RequestFingerprint(s)
	if err != nil || got != want {
		t.Fatal("legacy fingerprint changed", got, err)
	}
	s.Business = Business{Tags: []string{}}
	got, err = RequestFingerprint(s)
	if err != nil || got != want {
		t.Fatal("empty tags broke replay")
	}
	s.Business = original
	with, err := RequestFingerprint(s)
	if err != nil || with == want {
		t.Fatal("business omitted from fingerprint")
	}
	s.Business = canon
	reordered, err := RequestFingerprint(s)
	if err != nil || reordered != with {
		t.Fatal("tag order changed identity")
	}
	s.Business.ClientReference = "changed"
	changed, err := RequestFingerprint(s)
	if err != nil || changed == with {
		t.Fatal("reference not included")
	}
	s.Business = Business{ClientReference: canon.ClientReference, Tags: []string{"a"}}
	changed, err = RequestFingerprint(s)
	if err != nil || changed == with {
		t.Fatal("tags not included")
	}
}
