package encryption

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
)

type Box struct{ aead cipher.AEAD }

func New(encoded string) (*Box, error) {
	key, err := hex.DecodeString(encoded)
	if err != nil || len(key) != 32 {
		return nil, errors.New("MAILGATEWAY_MASTER_KEY must be 64 hexadecimal characters")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead}, nil
}
func (b *Box) Seal(id, password string) ([]byte, []byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return b.aead.Seal(nil, nonce, []byte(password), []byte(id)), nonce, nil
}
func (b *Box) Open(id string, ciphertext, nonce []byte) (string, error) {
	if len(nonce) != b.aead.NonceSize() {
		return "", errors.New("invalid credential nonce")
	}
	raw, err := b.aead.Open(nil, nonce, ciphertext, []byte(id))
	if err != nil {
		return "", errors.New("cannot decrypt provider credentials")
	}
	return string(raw), nil
}
