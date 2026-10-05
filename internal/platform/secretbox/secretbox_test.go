package secretbox_test

import (
	"bytes"
	"strings"
	"testing"

	"babki.my/babki/internal/platform/secretbox"
)

// validHexKey is any valid 64-character key.
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

// Decoding is checked against literal bytes, not a second hex.DecodeString.
func TestParseKeyDecodesHexToRawBytes(t *testing.T) {
	// Bytes 0x00..0x1f, so the expected slice can be checked by eye.
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

// A key of the wrong length or not in hex is refused, and the error names the
// variable and the command that makes a key.
func TestParseKeyRefusesAMalformedKey(t *testing.T) {
	for name, key := range map[string]string{
		"63 hex characters": strings.Repeat("a", 63),
		"64, but not hex":   strings.Repeat("g", 64),
	} {
		_, err := secretbox.ParseKey(key)
		if err == nil {
			t.Errorf("%s: ParseKey succeeded, want an error", name)
			continue
		}
		if !strings.Contains(err.Error(), "BABKI_ENCRYPTION_KEY") {
			t.Errorf("%s: error does not name BABKI_ENCRYPTION_KEY: %v", name, err)
		}
		if !strings.Contains(err.Error(), "openssl rand -hex 32") {
			t.Errorf("%s: error does not give the generation command: %v", name, err)
		}
	}
}

// An unset variable reads as empty and must fail like a malformed one.
func TestParseKeyEmpty(t *testing.T) {
	_, err := secretbox.ParseKey("")
	if err == nil {
		t.Fatal("ParseKey(\"\") succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "BABKI_ENCRYPTION_KEY") {
		t.Errorf("error does not name BABKI_ENCRYPTION_KEY: %v", err)
	}
}

func TestNewRejectsWrongKeySize(t *testing.T) {
	if _, err := secretbox.New(make([]byte, 16)); err == nil {
		t.Fatal("New(16-byte key) succeeded, want an error (AES-256 needs 32)")
	}
}

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

// Sealing one plaintext twice gives different output: the nonce is fresh.
func TestSealNonceIsFresh(t *testing.T) {
	b := mustBox(t)
	plaintext := []byte("t.same-plaintext-both-times")

	first := b.Seal(plaintext)
	second := b.Seal(plaintext)

	if bytes.Equal(first, second) {
		t.Fatal("two Seal calls on the same plaintext produced identical output; " +
			"the nonce is not being freshly randomized (crypto/rand) on every call")
	}
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

// Flipping any byte of sealed output makes Open fail: the GCM tag is
// checked.
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

func TestOpenRejectsWrongKey(t *testing.T) {
	b1 := mustBox(t)
	// Bytes 0x1f..0x00: a different key.
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

// Inputs shorter than the 12-byte nonce, and inputs with a nonce but less
// than the 16-byte tag, are errors, not panics.
func TestOpenTruncatedInputErrorsNotPanics(t *testing.T) {
	b := mustBox(t)

	for _, n := range []int{0, 1, 5, 11, 12, 20} {
		sealed := make([]byte, n)
		if _, err := b.Open(sealed); err == nil {
			t.Errorf("Open(%d-byte input) succeeded, want an error", n)
		}
	}
}

func TestOpenEmptyEverythingStillErrors(t *testing.T) {
	b := mustBox(t)
	if _, err := b.Open(nil); err == nil {
		t.Fatal("Open(nil) succeeded, want an error")
	}
}
