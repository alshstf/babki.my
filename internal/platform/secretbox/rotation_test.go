package secretbox_test

import (
	"bytes"
	"testing"

	"babki.my/babki/internal/platform/secretbox"
)

// A box given the key it replaced opens what that key sealed and seals only
// with its own; a box without it cannot open the old secrets.
func TestABoxOpensWhatItsPreviousKeySealed(t *testing.T) {
	oldKey, newKey, other := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32), bytes.Repeat([]byte{3}, 32)
	old, _ := secretbox.New(oldKey)
	sealedOld := old.Seal([]byte("token"))

	rotated, _ := secretbox.New(newKey)
	if _, err := rotated.Open(sealedOld); err == nil {
		t.Fatal("a box without the previous key opened what it sealed")
	}
	rotated, err := rotated.WithPrevious(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := rotated.Open(sealedOld); err != nil || string(got) != "token" {
		t.Fatalf("open with the previous key = %q, %v", got, err)
	}
	sealedNew := rotated.Seal([]byte("token"))
	if _, err := old.Open(sealedNew); err == nil {
		t.Error("a new secret was sealed with the previous key")
	}
	fresh, _ := secretbox.New(newKey)
	if got, err := fresh.Open(sealedNew); err != nil || string(got) != "token" {
		t.Errorf("the current key alone opens the new secret = %q, %v", got, err)
	}
	stranger, _ := secretbox.New(other)
	if _, err := stranger.WithPrevious(bytes.Repeat([]byte{4}, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := stranger.Open(sealedOld); err == nil {
		t.Error("a box that never had the key opened the secret")
	}
	if _, err := fresh.WithPrevious([]byte("short")); err == nil {
		t.Error("a previous key of the wrong size was accepted")
	}
}
