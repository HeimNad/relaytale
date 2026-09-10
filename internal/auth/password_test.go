package auth

import (
	"strings"
	"testing"
)

func TestPassword(t *testing.T) {
	hash, err := Hash("a-long-test-password")
	if err != nil {
		t.Fatal(err)
	}
	if !Verify(hash, "a-long-test-password") || Verify(hash, "wrong") {
		t.Fatal("password verification incorrect")
	}
	if Verify(strings.Replace(hash, "m=65536", "m=999999999", 1), "a-long-test-password") {
		t.Fatal("unbounded parameters accepted")
	}
	if _, err := Hash("short"); err == nil {
		t.Fatal("short password accepted")
	}
}
func TestSenderRestrictions(t *testing.T) {
	a := Account{AllowedFrom: []string{"Sender@example.com"}}
	for _, tc := range []struct {
		from string
		want bool
	}{{"Sender@EXAMPLE.COM", true}, {"sender@example.com", false}, {"Other@example.com", false}, {"Name <Sender@example.com>", false}, {"", false}, {"Sender@example.com\r\nX: 1", false}} {
		if a.Allows(tc.from) != tc.want {
			t.Errorf("unexpected authorization for %q", tc.from)
		}
	}
}
