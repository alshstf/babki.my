package secretbox_test

import (
	"bytes"
	"strings"
	"testing"

	"babki.my/babki/internal/platform/secretbox"
)

// validHexKey is a literal 64-character hex string (32 bytes): the 16-char
// sequence "0123456789abcdef" written out four times in a row (4*16=64), used
// everywhere a test just needs *a* valid key and does not care which bytes
// it decodes to.
const validHexKey = "0123456789abcdef" +
	"0123456789abcdef" +
	"0123456789abcdef" +
	"0123456789abcdef"

func mustBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key, err := secretbox.ParseKey(validHexKey)
	if err != nil {
		t.Fatalf("ParseKey(%q): %v", validHexKey, err)
	}
	b, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

// TestParseKeyDecodesHexToRawBytes pins the decoding itself against a literal
// expected byte slice, not against hex.DecodeString called a second time —
// that would just prove the two calls agree with each other, not that either
// is right.
func TestParseKeyDecodesHexToRawBytes(t *testing.T) {
	// "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f" is
	// the byte sequence 0x00..0x1f written out as hex, byte value equal to
	// byte index — chosen so the expected slice below can be checked by eye
	// against the input string rather than trusted blindly.
	got, err := secretbox.ParseKey("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	want := []byte{
		0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
		0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
		0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
		0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
	}
	if !bytes.Equal(got, want) {
		t.Errorf("ParseKey decoded to %x, want %x", got, want)
	}
	if len(got) != 32 {
		t.Errorf("ParseKey returned %d bytes, want 32 (AES-256)", len(got))
	}
}

// TestParseKeyWrongLength covers the too-short case (63 characters, one shy
// of the required 64). The error has to name the environment variable and
// the exact generation command, because that is the only place an operator
// staring at a failed startup will read either.
func TestParseKeyWrongLength(t *testing.T) {
	_, err := secretbox.ParseKey(strings.Repeat("a", 63))
	if err == nil {
		t.Fatal("ParseKey(63 hex chars) succeeded, want an error (64 exactly is required)")
	}
	if !strings.Contains(err.Error(), "BABKI_ENCRYPTION_KEY") {
		t.Errorf("error does not name BABKI_ENCRYPTION_KEY: %v", err)
	}
	if !strings.Contains(err.Error(), "openssl rand -hex 32") {
		t.Errorf("error does not give the generation command: %v", err)
	}
}

// TestParseKeyNotHex covers a string of the right LENGTH (64) that is not
// valid hex, so it exercises hex.DecodeString's own error path rather than
// the length check above.
func TestParseKeyNotHex(t *testing.T) {
	_, err := secretbox.ParseKey(strings.Repeat("g", 64))
	if err == nil {
		t.Fatal("ParseKey(64 non-hex chars) succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "BABKI_ENCRYPTION_KEY") {
		t.Errorf("error does not name BABKI_ENCRYPTION_KEY: %v", err)
	}
	if !strings.Contains(err.Error(), "openssl rand -hex 32") {
		t.Errorf("error does not give the generation command: %v", err)
	}
}

// TestParseKeyEmpty covers the unset-variable case: an empty string is what
// os.Getenv (and caarlos0/env) hand back when BABKI_ENCRYPTION_KEY is not
// set at all, and it must fail the same way a malformed value does rather
// than panic or silently produce a zero key.
func TestParseKeyEmpty(t *testing.T) {
	_, err := secretbox.ParseKey("")
	if err == nil {
		t.Fatal("ParseKey(\"\") succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "BABKI_ENCRYPTION_KEY") {
		t.Errorf("error does not name BABKI_ENCRYPTION_KEY: %v", err)
	}
}

// TestNewRejectsWrongKeySize guards New independently of ParseKey: a caller
// could build a key some other way (tests do, below), and New must not trust
// its length silently.
func TestNewRejectsWrongKeySize(t *testing.T) {
	if _, err := secretbox.New(make([]byte, 16)); err == nil {
		t.Fatal("New(16-byte key) succeeded, want an error (AES-256 needs 32)")
	}
}

// TestSealOpenRoundtrip is the basic contract: what Open returns is exactly
// what was sealed, byte for byte.
func TestSealOpenRoundtrip(t *testing.T) {
	b := mustBox(t)
	plaintext := []byte("t.XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX")

	sealed := b.Seal(plaintext)
	got, err := b.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("Open roundtrip = %q, want %q", got, plaintext)
	}
}

// TestSealOpenRoundtripEmptyPlaintext: an empty broker token should never
// happen in practice, but Seal/Open must not special-case length zero.
func TestSealOpenRoundtripEmptyPlaintext(t *testing.T) {
	b := mustBox(t)
	sealed := b.Seal([]byte{})
	got, err := b.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Open(Seal([])) = %q, want empty", got)
	}
}

// TestSealNonceIsFresh seals the same plaintext twice and requires different
// output. A hardcoded or reused nonce would make this test fail: with GCM, a
// repeated (key, nonce) pair for two different-looking calls would still
// produce output that starts with the same nonce prefix, and in the worse
// case of an all-zero/constant nonce implementation, encrypting the exact
// same plaintext twice under the exact same nonce yields byte-identical
// ciphertext — which is exactly what this test forbids.
func TestSealNonceIsFresh(t *testing.T) {
	b := mustBox(t)
	plaintext := []byte("t.same-plaintext-both-times")

	first := b.Seal(plaintext)
	second := b.Seal(plaintext)

	if bytes.Equal(first, second) {
		t.Fatal("two Seal calls on the same plaintext produced identical output; " +
			"the nonce is not being freshly randomized (crypto/rand) on every call")
	}
	// Both must still decrypt to the same plaintext independently — this
	// rules out a "fresh but never stored/used" nonce bug where Seal
	// randomizes something irrelevant while still leaving Open broken.
	got1, err := b.Open(first)
	if err != nil {
		t.Fatalf("Open(first): %v", err)
	}
	got2, err := b.Open(second)
	if err != nil {
		t.Fatalf("Open(second): %v", err)
	}
	if !bytes.Equal(got1, plaintext) || !bytes.Equal(got2, plaintext) {
		t.Fatal("both sealed outputs must independently decrypt back to the original plaintext")
	}
}

// TestOpenDetectsTampering flips every single byte of a sealed value, one at
// a time, and requires Open to reject every one of them. This is what
// actually proves the integrity check is wired in: GCM gives it for free,
// but a bug that swapped the AEAD open for a plain stream-cipher decrypt (no
// tag check) would still pass TestSealOpenRoundtrip and would only be caught
// here.
func TestOpenDetectsTampering(t *testing.T) {
	b := mustBox(t)
	sealed := b.Seal([]byte("t.the-quick-brown-fox-jumps"))

	for i := range sealed {
		tampered := append([]byte(nil), sealed...)
		tampered[i] ^= 0xFF
		if _, err := b.Open(tampered); err == nil {
			t.Errorf("Open accepted input with byte %d flipped; tampering was not detected", i)
		}
	}
}

// TestOpenRejectsWrongKey ensures a Box built from a different key cannot
// open another Box's output — the case a stray "single fixed nonce/key
// everywhere" bug would otherwise miss.
func TestOpenRejectsWrongKey(t *testing.T) {
	b1 := mustBox(t)
	// The bytes 0x1f down to 0x00 written out as hex — 64 characters, and a
	// different 32-byte key from validHexKey's.
	key2, err := secretbox.ParseKey("1f1e1d1c1b1a191817161514131211100f0e0d0c0b0a09080706050403020100")
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	b2, err := secretbox.New(key2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	sealed := b1.Seal([]byte("t.sealed-under-b1"))
	if _, err := b2.Open(sealed); err == nil {
		t.Fatal("b2.Open succeeded on a value sealed by b1 with a different key")
	}
}

// TestOpenTruncatedInputErrorsNotPanics covers both arithmetic boundaries
// inside Open, not just the first: 0, 1, 5, 11 are shorter than the 12-byte
// nonce itself, the case most likely to panic on a naive slice split
// (sealed[:nonceSize]). 12 and 20 are the second boundary — a full nonce but
// a ciphertext shorter than GCM's 16-byte authentication tag (12 leaves a
// zero-byte ciphertext, 20 leaves 8 bytes) — which sealed[:nonceSize] cannot
// panic on but b.aead.Open must still reject rather than accept or panic on.
func TestOpenTruncatedInputErrorsNotPanics(t *testing.T) {
	b := mustBox(t)

	for _, n := range []int{0, 1, 5, 11, 12, 20} {
		sealed := make([]byte, n)
		if _, err := b.Open(sealed); err == nil {
			t.Errorf("Open(%d-byte input) succeeded, want an error", n)
		}
	}
}

// TestOpenEmptyEverythingStillErrors is the degenerate case of the above:
// nil input.
func TestOpenEmptyEverythingStillErrors(t *testing.T) {
	b := mustBox(t)
	if _, err := b.Open(nil); err == nil {
		t.Fatal("Open(nil) succeeded, want an error")
	}
}
