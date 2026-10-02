package customer

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// identityBox seals identity numbers with AES-256-GCM (BR-085).
//
// The nonce is random per seal and stored in front of the ciphertext, so the
// column is self-contained: nonce || ciphertext || tag. GCM authenticates, so a
// tampered row fails to open rather than decrypting to a wrong number.
//
// There is no open() yet, and that is the point rather than an omission: no
// endpoint in phase 1 returns the number. It lands with whatever first needs
// it, alongside the audit row that read must write.
type identityBox struct {
	aead cipher.AEAD
}

func newIdentityBox(key []byte) (*identityBox, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("identity key is %d bytes; AES-256 needs 32", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &identityBox{aead: aead}, nil
}

func (b *identityBox) seal(plain string) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("identity nonce: %w", err)
	}
	return b.aead.Seal(nonce, nonce, []byte(plain), nil), nil
}

// open exists for the round-trip test, which is what proves seal is not a
// one-way scramble.
func (b *identityBox) open(sealed []byte) (string, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return "", errors.New("identity ciphertext too short")
	}
	plain, err := b.aead.Open(nil, sealed[:n], sealed[n:], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func lastFour(s string) string {
	r := []rune(s)
	if len(r) <= 4 {
		return s
	}
	return string(r[len(r)-4:])
}
