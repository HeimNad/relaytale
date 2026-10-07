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

func TestVersionedCredentials(t *testing.T) {
	old, _ := New(strings.Repeat("ab", 32))
	ring, err := NewRing("v2", map[string]string{"legacy": strings.Repeat("ab", 32), "v2": strings.Repeat("cd", 32), "v3": strings.Repeat("cd", 32)})
	if err != nil {
		t.Fatal(err)
	}
	c, n, _ := old.Seal("id", "password")
	if p, e := ring.Open("id", c, n); e != nil || p != "password" {
		t.Fatal("legacy read failed")
	}
	c, n, _ = ring.Seal("id", "password")
	if v, e := Version(n); e != nil || v != "v2" {
		t.Fatal("version missing")
	}
	if p, e := ring.Open("id", c, n); e != nil || p != "password" {
		t.Fatal("versioned read failed")
	}
	if _, e := old.Open("id", c, n); e == nil {
		t.Fatal("old reader accepted new format")
	}
	n[6] = '3' // Same key material under another name must still fail authentication.
	if _, e := ring.Open("id", c, n); e == nil {
		t.Fatal("version substitution accepted")
	}
	for _, nonce := range [][]byte{nil, {}, []byte("RTK1"), make([]byte, 18)} {
		if _, e := ring.Open("id", c, nonce); e == nil {
			t.Fatal("malformed nonce accepted")
		}
	}
}

func TestKeyringEnvironmentStrict(t *testing.T) {
	key := strings.Repeat("ab", 32)
	for _, raw := range []string{"", `null`, `{"Active":"v2","keys":{}}`, `{"active":"v2","Active":"v3","keys":{}}`, `{"active":"v2","active":"v3","keys":{}}`, `{"active":"v2","keys":{"v2":"` + key + `","v2":"` + key + `"}}`, `{"active":"v2","keys":{"v2":"` + key + `"},"extra":true}`, `{"active":"v2","keys":{"v2":"` + key + `"}} {}`} {
		t.Setenv("RELAYTALE_KEYRING", raw)
		if _, err := NewEnvironment(key); err == nil {
			t.Fatal("invalid ring fell back to master")
		}
	}
	t.Setenv("RELAYTALE_KEYRING", `{"active":"v2","keys":{"v2":"`+key+`"}}`)
	if box, err := NewEnvironment("invalid"); err != nil || box.ActiveVersion() != "v2" {
		t.Fatal("ring not selected")
	}
}
