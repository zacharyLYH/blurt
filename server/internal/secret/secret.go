// Package secret is the one cipher Blurt keeps model API keys behind. A user's key
// is entered once on the setup page, stored encrypted at rest, and opened only to
// build that user's model transport. The key material is config's encryption key:
// AES-256 with a fresh nonce per message, so the same key never seals the same way
// twice.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// KeyLen is the AES-256 key size config validates at boot.
const KeyLen = 32

// ErrKey reports a key that is not the size the cipher takes.
var ErrKey = errors.New("secret: key must be 32 bytes")

// Encrypt seals one plaintext and returns base64(nonce || ciphertext). An empty
// plaintext stays empty, so an unconfigured user stores nothing rather than an
// encryption of nothing.
func Encrypt(key []byte, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	gcm, err := cipherFor(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("secret: nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt opens one value Encrypt produced. An empty value decrypts to an empty
// string: a user with no key is a configuration to complete, not a decryption
// failure.
func Decrypt(key []byte, encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	gcm, err := cipherFor(key)
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("secret: decode: %w", err)
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("secret: ciphertext is shorter than a nonce")
	}
	nonce, sealed := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return "", fmt.Errorf("secret: open: %w", err)
	}
	return string(plain), nil
}

func cipherFor(key []byte) (cipher.AEAD, error) {
	if len(key) != KeyLen {
		return nil, ErrKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: gcm: %w", err)
	}
	return gcm, nil
}
