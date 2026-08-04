// Package secretbox provides authenticated symmetric encryption for secrets
// this application has to keep at rest but also read back later — today the
// only consumer is the broker API token behind the T-Invest importer, which
// a background worker needs to decrypt on every sync run.
//
// AES-256-GCM, built from the standard library only (crypto/aes,
// crypto/cipher, crypto/rand): no third-party dependency for the one
// cryptographic primitive this codebase needs.
//
// Sealed output is one []byte: a fresh random nonce followed by the
// ciphertext (nonce||ciphertext), with no format version byte and no key
// rotation. Both are deliberately absent — the only consumer today is one
// secret column, and a rotation mechanism nobody has exercised is worse than
// no rotation mechanism at all.
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

// hexKeyLen is the required length of the BABKI_ENCRYPTION_KEY value: two
// hex characters per byte of KeySize.
const hexKeyLen = KeySize * 2

// keyHelp names the environment variable and the exact command that produces
// a value ParseKey accepts. Appended to every ParseKey error, because the
// startup log line an operator reads is the only place either fact reaches
// them.
const keyHelp = "set BABKI_ENCRYPTION_KEY to 64 hex characters (32 bytes); generate one with `openssl rand -hex 32`"

// ParseKey decodes s — the value of BABKI_ENCRYPTION_KEY — into a 32-byte
// AES-256 key. s must be exactly 64 hex characters. Hex rather than base64:
// it can be read aloud or typed by hand without a case-sensitivity mistake.
func ParseKey(s string) ([]byte, error) {
	if len(s) != hexKeyLen {
		return nil, fmt.Errorf("secretbox: BABKI_ENCRYPTION_KEY is %d characters long, want exactly %d; %s",
			len(s), hexKeyLen, keyHelp)
	}
	key, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("secretbox: BABKI_ENCRYPTION_KEY is not valid hex: %w; %s", err, keyHelp)
	}
	return key, nil
}

// Box seals and opens secrets with AES-256-GCM under one fixed key.
type Box struct {
	aead cipher.AEAD
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

// Seal encrypts and authenticates plaintext, returning nonce||ciphertext as
// a single slice. A fresh random nonce is drawn from crypto/rand on every
// call, so sealing the same plaintext twice never produces the same output.
func (b *Box) Seal(plaintext []byte) []byte {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		// crypto/rand.Read only fails when the OS entropy source itself is
		// broken — a condition this codebase has no recovery path for, and
		// Seal's signature (no error return, fixed by the callers this
		// primitive serves) leaves no way to report it upward. Every other
		// caller of crypto/rand.Read in the standard library treats this the
		// same way.
		panic("secretbox: reading a random nonce: " + err.Error())
	}
	return b.aead.Seal(nonce, nonce, plaintext, nil)
}

// Open authenticates and decrypts sealed — the output of Seal — returning
// the original plaintext. It reports an error, rather than panicking, on
// input shorter than a nonce, on a corrupted ciphertext, and on a Box built
// from a different key: GCM's authentication tag catches the latter two.
func (b *Box) Open(sealed []byte) ([]byte, error) {
	nonceSize := b.aead.NonceSize()
	if len(sealed) < nonceSize {
		return nil, fmt.Errorf("secretbox: sealed input is %d bytes, shorter than the %d-byte nonce",
			len(sealed), nonceSize)
	}
	nonce, ciphertext := sealed[:nonceSize], sealed[nonceSize:]
	plaintext, err := b.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("secretbox: open: %w", err)
	}
	return plaintext, nil
}
