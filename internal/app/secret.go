package app

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
)

// sealer encrypts the credentials users store (API keys, mailbox passwords) with a key
// derived from SECRET_KEY. Without SECRET_KEY there is none, and such credentials can't be saved.
type sealer struct{ aead cipher.AEAD }

func newSealer(secret string) *sealer {
	if secret == "" {
		return nil
	}
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		panic(err) // a 32-byte key is always valid
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return &sealer{aead}
}

func (s *sealer) seal(plain string) []byte {
	nonce := make([]byte, s.aead.NonceSize())
	rand.Read(nonce)
	return s.aead.Seal(nonce, nonce, []byte(plain), nil)
}

func (s *sealer) open(sealed []byte) (string, error) {
	if s == nil {
		return "", errors.New("SECRET_KEY is not set")
	}
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return "", errors.New("sealed value too short")
	}
	plain, err := s.aead.Open(nil, sealed[:n], sealed[n:], nil)
	if err != nil {
		return "", errors.New("can't decrypt a stored credential; was SECRET_KEY changed?")
	}
	return string(plain), nil
}
