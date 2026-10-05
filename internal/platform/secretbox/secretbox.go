// Package secretbox seals secrets kept at rest, such as broker tokens, with
// AES-256-GCM from the standard library. Sealed output is nonce||ciphertext.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// KeySize is the required raw key length in bytes: AES-256.
const KeySize = 32

// hexKeyLen is the length of BABKI_ENCRYPTION_KEY: two hex characters a byte.
const hexKeyLen = KeySize * 2

// keyHelp is appended to ParseKey errors: the startup log is where an operator
// learns how to make a valid key.
const keyHelp = "set BABKI_ENCRYPTION_KEY to 64 hex characters (32 bytes); generate one with `openssl rand -hex 32`"

// ParseKey decodes BABKI_ENCRYPTION_KEY, exactly 64 hex characters, into a
// 32-byte key. Hex rather than base64 so it can be typed without case errors.
func ParseKey(s string) ([]byte, error) {
	if len(s) != hexKeyLen {
		return nil, fmt.Errorf("secretbox: BABKI_ENCRYPTION_KEY is %d bytes long, want exactly %d; %s",
			len(s), hexKeyLen, keyHelp)
	}
	key, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("secretbox: BABKI_ENCRYPTION_KEY is not valid hex: %w; %s", err, keyHelp)
	}
	return key, nil
}

// Box seals with one key and opens with it or any previous key. It is safe for
// concurrent use: the standard library's GCM keeps no state between calls.
type Box struct {
	aead cipher.AEAD
	// previous open what earlier keys sealed and never seal.
	previous []cipher.AEAD
}

// New builds a Box from a 32-byte AES-256 key, typically ParseKey's output.
func New(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secretbox: key is %d bytes, want %d (AES-256)", len(key), KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}
	return &Box{aead: aead}, nil
}

// WithPrevious lets the box open what earlier keys sealed, so a key can be
// rotated before every secret is resealed.
func (b *Box) WithPrevious(keys ...[]byte) (*Box, error) {
	for _, key := range keys {
		older, err := New(key)
		if err != nil {
			return nil, fmt.Errorf("secretbox: previous key: %w", err)
		}
		b.previous = append(b.previous, older.aead)
	}
	return b, nil
}

// Seal encrypts and authenticates plaintext under a fresh random nonce and
// returns nonce||ciphertext.
func (b *Box) Seal(plaintext []byte) []byte {
	nonce := make([]byte, b.aead.NonceSize())
	// crypto/rand.Read never returns an error; it crashes the process instead.
	_, _ = rand.Read(nonce)
	return b.aead.Seal(nonce, nonce, plaintext, nil)
}

// Open decrypts the output of Seal. Short, corrupted or foreign-key input is
// an error.
func (b *Box) Open(sealed []byte) ([]byte, error) {
	nonceSize := b.aead.NonceSize()
	if len(sealed) < nonceSize {
		return nil, fmt.Errorf("secretbox: sealed input is %d bytes, shorter than the %d-byte nonce",
			len(sealed), nonceSize)
	}
	nonce, ciphertext := sealed[:nonceSize], sealed[nonceSize:]
	plaintext, err := b.aead.Open(nil, nonce, ciphertext, nil)
	if err == nil {
		return plaintext, nil
	}
	for _, older := range b.previous {
		if plaintext, perr := older.Open(nil, nonce, ciphertext, nil); perr == nil {
			return plaintext, nil
		}
	}
	return nil, fmt.Errorf("secretbox: open: %w", err)
}
