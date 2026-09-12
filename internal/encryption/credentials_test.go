package encryption

import (
	"os"
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

func TestEnvironmentKeyRenameCompatibility(t *testing.T) {
	t.Setenv("MAILGATEWAY_MASTER_KEY", "legacy-test-value")
	// Restore the original new-name variable without exposing its value.
	original, present := os.LookupEnv("RELAYTALE_MASTER_KEY")
	t.Cleanup(func() {
		if present {
			os.Setenv("RELAYTALE_MASTER_KEY", original)
		} else {
			os.Unsetenv("RELAYTALE_MASTER_KEY")
		}
	})
	os.Unsetenv("RELAYTALE_MASTER_KEY")
	if EnvironmentKey() != "legacy-test-value" {
		t.Fatal("legacy alias not loaded")
	}
	t.Setenv("RELAYTALE_MASTER_KEY", "new-test-value")
	if EnvironmentKey() != "new-test-value" {
		t.Fatal("new name must take precedence")
	}
	t.Setenv("RELAYTALE_MASTER_KEY", "")
	if EnvironmentKey() != "" {
		t.Fatal("explicit empty value must not revive a legacy key")
	}
}
