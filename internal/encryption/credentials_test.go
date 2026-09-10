package encryption

import (
	"strings"
	"testing"
)

func TestCredentials(t *testing.T) {
	box, err := New(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, nonce, err := box.Seal("provider-id", "secret")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := box.Open("provider-id", ciphertext, nonce)
	if err != nil || plain != "secret" {
		t.Fatal("roundtrip failed")
	}
	if _, err := box.Open("other-provider", ciphertext, nonce); err == nil {
		t.Fatal("credential substitution allowed")
	}
	ciphertext[0] ^= 1
	if _, err := box.Open("provider-id", ciphertext, nonce); err == nil {
		t.Fatal("tampering undetected")
	}
	if _, err := New("short"); err == nil {
		t.Fatal("short key accepted")
	}
}
