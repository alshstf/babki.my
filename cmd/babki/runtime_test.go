package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
)

// TestSetupInstallsTheConfiguredLoggerAsTheDefault pins the slog.SetDefault
// call in setup. Nothing else pinned it: deleting the line left the whole
// suite green, while family.WriteError's doc claims the error behind every 500
// "lands in the same stream, level and format as every other" — and that claim
// rests entirely on this one statement. WriteError takes no logger (it is
// called from forty-odd places across five packages) and reads slog.Default,
// so without the install its lines would go to Go's built-in default: a text
// handler at level info, which is neither the configured format nor the
// configured level. On an instance running at level error that turns the only
// diagnosis of a 500 into noise in a second, differently shaped stream.
//
// All three of stream, level and format are asserted, because the claim names
// all three and a test that checked only one would leave the other two free to
// move.
//
// setup fails here on the missing BABKI_DATABASE_URL — deliberately, since
// that keeps the test off a database it does not need. The install happens
// before that check, and the point of the test is that it happens at all.
func TestSetupInstallsTheConfiguredLoggerAsTheDefault(t *testing.T) {
	ctx := context.Background()

	// setup mutates a process-wide global. Put it back, or every test running
	// after this one in this package logs at error into a closed pipe.
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	t.Setenv("BABKI_DATABASE_URL", "")
	t.Setenv("BABKI_LOG_LEVEL", "error")
	t.Setenv("BABKI_LOG_FORMAT", "json")

	// logging.New writes to whatever os.Stderr is when it is called, so the
	// swap has to be in place before setup runs. The handler keeps the pipe
	// afterwards, which is what makes "the stream" assertable at all.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	realStderr := os.Stderr
	os.Stderr = w
	_, setupErr := setup(ctx, false)
	os.Stderr = realStderr

	if setupErr == nil {
		t.Fatal("setup succeeded with no BABKI_DATABASE_URL; this test relies on it failing after the logger is installed")
	}

	installed := slog.Default()
	if installed == prev {
		t.Fatal("slog.Default() is unchanged after setup: the configured logger was never installed, " +
			"so family.WriteError's 500 lines go to Go's built-in default instead")
	}
	// Level: configured error, so info must be off. Go's built-in default is
	// on at info, which is exactly what a missing install looks like.
	if installed.Enabled(ctx, slog.LevelInfo) {
		t.Error("the default logger is enabled at INFO, but BABKI_LOG_LEVEL=error was configured")
	}
	if !installed.Enabled(ctx, slog.LevelError) {
		t.Error("the default logger is disabled at ERROR, so the line behind a 500 would not be written at all")
	}

	const msg = "the line WriteError writes"
	installed.Error(msg, "account", "one")
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close pipe reader: %v", err)
	}

	// Stream: it arrived on the pipe that stood in for stderr. Format: it
	// parses as one JSON object with slog's own field names, which a text
	// handler's output does not.
	if len(out) == 0 {
		t.Fatal("nothing reached stderr: the default logger does not write to the stream setup configured")
	}
	var rec map[string]any
	if err := json.Unmarshal(out, &rec); err != nil {
		t.Fatalf("the record is not JSON, but BABKI_LOG_FORMAT=json was configured: %v\ngot: %s", err, out)
	}
	if rec["msg"] != msg {
		t.Errorf("msg = %v, want %q", rec["msg"], msg)
	}
	if rec["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", rec["level"])
	}
	if rec["account"] != "one" {
		t.Errorf("account = %v, want \"one\": the attributes callers pass have to survive too", rec["account"])
	}
}
