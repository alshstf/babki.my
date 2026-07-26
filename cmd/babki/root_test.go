package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRootHasCommands(t *testing.T) {
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute --help: %v", err)
	}
	out := buf.String()
	for _, cmd := range []string{"all", "api", "worker", "migrate", "version"} {
		if !strings.Contains(out, cmd) {
			t.Errorf("help output missing command %q; got:\n%s", cmd, out)
		}
	}
}
