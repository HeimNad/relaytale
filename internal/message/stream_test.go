package message

import (
	"relaytale/internal/storage"
	"strings"
	"testing"
)

func TestStreamPoliciesAndFingerprint(t *testing.T) {
	for _, bad := range [][]StreamPolicy{{{Name: "otp", Weight: 0}}, {{Name: "otp", Weight: 9}}, {{Name: "OTP", Weight: 1}}, {{Name: "otp", Weight: 1}, {Name: "otp", Weight: 2}}, {{Name: "otp", Weight: 1, Budget: SubmissionBudget{Hour: -1}}}} {
		if _, err := CanonicalStreams(bad); err == nil {
			t.Fatal("invalid stream policy", bad)
		}
	}
	policies, err := CanonicalStreams([]StreamPolicy{{Name: "otp", Weight: 3}})
	if err != nil || len(policies) != 2 || policies[1].Name != "normal" || policies[1].Weight != 1 {
		t.Fatal(policies, err)
	}
	s := Submission{Envelope: Envelope{From: "a@example.test", Recipients: []string{"b@example.test"}}, Archive: storage.Archive{SHA256: strings.Repeat("a", 64), Size: 20}}
	for _, business := range []Business{{}, {ClientReference: "order:1"}} {
		s.Business = business
		s.Stream = ""
		old, err := RequestFingerprint(s)
		if err != nil {
			t.Fatal(err)
		}
		s.Stream = "normal"
		normal, err := RequestFingerprint(s)
		if err != nil || normal != old {
			t.Fatal("legacy fingerprint changed", err)
		}
		s.Stream = "otp"
		otp, err := RequestFingerprint(s)
		if err != nil || otp == old {
			t.Fatal("stream missing from fingerprint", err)
		}
		s.Stream = "security"
		security, err := RequestFingerprint(s)
		if err != nil || security == otp {
			t.Fatal("streams collided", err)
		}
	}
}
