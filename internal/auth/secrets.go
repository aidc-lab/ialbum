package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
)

type Sealer struct{ aead cipher.AEAD }

func NewSealer(key []byte) (*Sealer, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create secret cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create secret sealer: %w", err)
	}
	return &Sealer{aead: aead}, nil
}

func (s *Sealer) Seal(plaintext []byte, aad string) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, s.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("generate secret nonce: %w", err)
	}
	ciphertext = s.aead.Seal(nil, nonce, plaintext, []byte(aad))
	return ciphertext, nonce, nil
}

func (s *Sealer) Open(ciphertext, nonce []byte, aad string) ([]byte, error) {
	plaintext, err := s.aead.Open(nil, nonce, ciphertext, []byte(aad))
	if err != nil {
		return nil, fmt.Errorf("decrypt secret: %w", err)
	}
	return plaintext, nil
}
