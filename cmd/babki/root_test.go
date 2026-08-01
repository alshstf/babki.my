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

// TestNewCbrHTTPClientHasABoundedTimeout guards against startJobClient going
// back to an unbounded client (cbr.New(nil, "") falls back to
// http.DefaultClient, whose Timeout is 0): the backfill job fires one
// request per currency in use under a 15-minute job Timeout, so one stalled
// TCP connection on an unbounded client could pin a worker slot for that
// whole budget.
func TestNewCbrHTTPClientHasABoundedTimeout(t *testing.T) {
	c := newCbrHTTPClient()
	if c.Timeout != cbrHTTPTimeout {
		t.Fatalf("newCbrHTTPClient().Timeout = %s, want %s", c.Timeout, cbrHTTPTimeout)
	}
	if c.Timeout <= 0 {
		t.Fatalf("newCbrHTTPClient().Timeout = %s, want a positive bound (0 means unbounded)", c.Timeout)
	}
}
