package main

import (
	"os"
	"testing"
)

// The committed file is what the generator writes now: a rule changed on the
// server without `make gen` fails here, before CI does.
func TestTheWebConstantsAreFresh(t *testing.T) {
	committed, err := os.ReadFile("../../web/src/api/constants.gen.ts")
	if err != nil {
		t.Fatal(err)
	}
	if string(committed) != string(render()) {
		t.Error("web/src/api/constants.gen.ts is stale: run make gen")
	}
}
