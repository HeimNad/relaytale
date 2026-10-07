package encryption

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
)

var keyName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var credentialError = errors.New("cannot decrypt provider credentials")

type Box struct {
	keys         map[string]cipher.AEAD
	active       string
	legacyWriter bool
}

func aead(encoded string) (cipher.AEAD, error) {
	key, err := hex.DecodeString(encoded)
	if err != nil || len(key) != 32 {
		return nil, errors.New("master keys must be 64 hexadecimal characters")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// New retains the legacy format for existing single-key configurations.
func New(encoded string) (*Box, error) {
	a, err := aead(encoded)
	if err != nil {
		return nil, err
	}
	return &Box{keys: map[string]cipher.AEAD{"legacy": a}, active: "legacy", legacyWriter: true}, nil
}
func NewRing(active string, keys map[string]string) (*Box, error) {
	if !keyName.MatchString(active) || active == "legacy" || len(keys) < 1 || len(keys) > 16 {
		return nil, errors.New("invalid keyring")
	}
	b := &Box{keys: map[string]cipher.AEAD{}, active: active}
	for id, encoded := range keys {
		if !keyName.MatchString(id) {
			return nil, errors.New("invalid keyring")
		}
		a, err := aead(encoded)
		if err != nil {
			return nil, errors.New("invalid keyring")
		}
		b.keys[id] = a
	}
	if b.keys[active] == nil {
		return nil, errors.New("active key missing")
	}
	return b, nil
}
func (b *Box) ActiveVersion() string { return b.active }
func Version(nonce []byte) (string, error) {
	if len(nonce) == 12 {
		return "legacy", nil
	}
	if len(nonce) < 18 || !bytes.Equal(nonce[:4], []byte("RTK1")) {
		return "", credentialError
	}
	n := int(nonce[4])
	if n < 1 || n > 64 || len(nonce) != 5+n+12 {
		return "", credentialError
	}
	id := string(nonce[5 : 5+n])
	if !keyName.MatchString(id) || id == "legacy" {
		return "", credentialError
	}
	return id, nil
}
func (b *Box) Seal(id, password string) ([]byte, []byte, error) {
	a := b.keys[b.active]
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	aad := []byte(id)
	if !b.legacyWriter {
		aad = []byte(id + "\x00relaytale.credential/RTK1/" + b.active)
	}
	encrypted := a.Seal(nil, nonce, []byte(password), aad)
	if b.legacyWriter {
		return encrypted, nonce, nil
	}
	envelope := append([]byte("RTK1"), byte(len(b.active)))
	envelope = append(envelope, []byte(b.active)...)
	envelope = append(envelope, nonce...)
	return encrypted, envelope, nil
}
func (b *Box) Open(id string, encrypted, nonce []byte) (string, error) {
	version, err := Version(nonce)
	if err != nil {
		return "", credentialError
	}
	a := b.keys[version]
	if a == nil {
		return "", credentialError
	}
	aad := []byte(id)
	if version != "legacy" {
		aad = []byte(id + "\x00relaytale.credential/RTK1/" + version)
		nonce = nonce[len(nonce)-12:]
	}
	raw, err := a.Open(nil, nonce, encrypted, aad)
	if err != nil {
		return "", credentialError
	}
	return string(raw), nil
}
func EnvironmentKey() string {
	if value, ok := os.LookupEnv("RELAYTALE_MASTER_KEY"); ok {
		return value
	}
	return os.Getenv("MAILGATEWAY_MASTER_KEY")
}
func Configured() bool {
	_, ring := os.LookupEnv("RELAYTALE_KEYRING")
	return ring || EnvironmentKey() != ""
}

// Explicit presence (including empty/invalid JSON) never falls back to a key.
func NewEnvironment(fallback string) (*Box, error) {
	raw, present := os.LookupEnv("RELAYTALE_KEYRING")
	if !present {
		return New(fallback)
	}
	if len(raw) > 16384 {
		return nil, errors.New("invalid keyring configuration")
	}
	// Reject duplicate object members at every depth before decoding secrets.
	d := json.NewDecoder(bytes.NewBufferString(raw))
	if err := uniqueJSON(d); err != nil {
		return nil, errors.New("invalid keyring configuration")
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("invalid keyring configuration")
	}
	// encoding/json matches struct fields case-insensitively; require exact names.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil || len(fields) != 2 || fields["active"] == nil || fields["keys"] == nil {
		return nil, errors.New("invalid keyring configuration")
	}
	var cfg struct {
		Active string            `json:"active"`
		Keys   map[string]string `json:"keys"`
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return nil, errors.New("invalid keyring configuration")
	}
	return NewRing(cfg.Active, cfg.Keys)
}
func uniqueJSON(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delim == '{' {
		seen := map[string]bool{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := k.(string)
			if !ok || seen[name] {
				return errors.New("duplicate member")
			}
			seen[name] = true
			if err := uniqueJSON(d); err != nil {
				return err
			}
		}
	} else if delim == '[' {
		for d.More() {
			if err := uniqueJSON(d); err != nil {
				return err
			}
		}
	} else {
		return errors.New("invalid delimiter")
	}
	_, err = d.Token()
	return err
}
