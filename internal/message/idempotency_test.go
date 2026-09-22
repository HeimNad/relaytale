package message

import (
	"relaytale/internal/storage"
	"strings"
	"testing"
)

func TestRequestFingerprintIdentity(t *testing.T) {
	s := Submission{Envelope: Envelope{From: "from@example.test", Recipients: []string{"a@example.test", "b@example.test"}}, Archive: storage.Archive{SHA256: strings.Repeat("a", 64), Size: 42}}
	initial, e := RequestFingerprint(s)
	if e != nil {
		t.Fatal(e)
	}
	same := s
	same.ID = "other"
	same.Archive.Path = "another"
	same.Subject = "derived"
	same.MessageID = "derived"
	if got, e := RequestFingerprint(same); e != nil || got != initial {
		t.Fatal("non-request fields changed fingerprint", e)
	}
	for _, change := range []func(*Submission){
		func(s *Submission) { s.Envelope.From = "other@example.test" },
		func(s *Submission) { s.Envelope.Recipients = []string{"b@example.test", "a@example.test"} },
		func(s *Submission) { s.Archive.SHA256 = strings.Repeat("b", 64) },
		func(s *Submission) { s.Archive.Size++ },
	} {
		changed := s
		change(&changed)
		if got, e := RequestFingerprint(changed); e != nil || got == initial {
			t.Fatal("request change not detected", e)
		}
	}
	for _, address := range []string{"", "a\r\nb@example.test", "名@example.test"} {
		invalid := s
		invalid.Envelope.Recipients = []string{address}
		if _, e := RequestFingerprint(invalid); e == nil {
			t.Fatal("invalid address accepted")
		}
	}
}
