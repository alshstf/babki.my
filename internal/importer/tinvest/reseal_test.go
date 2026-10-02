package tinvest

import (
	"bytes"
	"testing"

	"babki.my/babki/internal/platform/secretbox"
)

// Resealing re-encrypts every stored token with the current key, so that the
// previous one can be dropped; a token no key opens stops the run and leaves
// every token as it was.
func TestResealTokensMovesEveryTokenToTheCurrentKey(t *testing.T) {
	f := newFixture(t)
	oldKey, newKey := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	old, _ := secretbox.New(oldKey)
	if err := f.store.UpdateConnectionToken(f.ctx, f.spaceID, f.conn.ID, old.Seal([]byte("live-token")), "oken"); err != nil {
		t.Fatal(err)
	}
	rotated, _ := secretbox.New(newKey)
	rotated, _ = rotated.WithPrevious(oldKey)
	n, err := f.store.ResealTokens(f.ctx, rotated)
	if err != nil || n != 1 {
		t.Fatalf("ResealTokens = %d, %v", n, err)
	}
	conn, err := f.store.ConnectionByID(f.ctx, f.spaceID, f.conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	current, _ := secretbox.New(newKey)
	if got, err := current.Open(conn.TokenCiphertext); err != nil || string(got) != "live-token" {
		t.Errorf("after resealing, the current key alone opens = %q, %v", got, err)
	}

	stranger, _ := secretbox.New(bytes.Repeat([]byte{9}, 32))
	before := conn.TokenCiphertext
	if _, err := f.store.ResealTokens(f.ctx, stranger); err == nil {
		t.Fatal("resealed with a box that opens nothing")
	}
	after, _ := f.store.ConnectionByID(f.ctx, f.spaceID, f.conn.ID)
	if !bytes.Equal(after.TokenCiphertext, before) {
		t.Error("a refused reseal changed a stored token")
	}
}
