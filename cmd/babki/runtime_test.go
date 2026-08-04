package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"babki.my/babki/internal/platform/config"
	"babki.my/babki/internal/platform/secretbox"
)

// validHexKey is a literal 64-character hex string (32 bytes) — an
// AES-256-valid BABKI_ENCRYPTION_KEY — used wherever a test needs *a* key
// that secretbox.ParseKey accepts and does not care which bytes it decodes
// to. Mirrors internal/platform/secretbox/secretbox_test.go's constant of
// the same name; the two packages cannot share one without an import this
// value is too small to justify.
const validHexKey = "0123456789abcdef" +
	"0123456789abcdef" +
	"0123456789abcdef" +
	"0123456789abcdef"

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
	_, setupErr := setup(ctx, false, false)
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

// TestSetupRefusesToStartWithoutEncryptionKeyWhenRequired covers the worker
// role (and, by the same requireEncryptionKey=true path, all/api): started
// without BABKI_ENCRYPTION_KEY, it must refuse rather than come up unable to
// ever decrypt a broker token it will later be asked to read.
//
// BABKI_DATABASE_URL is set to a syntactically valid but unreachable value —
// setup must reject the missing key BEFORE it ever dials the database, so
// this test needs no Docker/testdb dependency. If a future change reordered
// the two checks, this test would start timing out against a connection
// instead of failing fast, which is itself a signal something moved.
func TestSetupRefusesToStartWithoutEncryptionKeyWhenRequired(t *testing.T) {
	ctx := context.Background()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	t.Setenv("BABKI_DATABASE_URL", "postgres://u:p@localhost:5432/babki")
	t.Setenv("BABKI_ENCRYPTION_KEY", "")

	_, err := setup(ctx, true, true)
	if err == nil {
		t.Fatal("setup(ctx, true, true) succeeded with no BABKI_ENCRYPTION_KEY; " +
			"the worker/api/all roles must refuse to start without one")
	}
	if !strings.Contains(err.Error(), "BABKI_ENCRYPTION_KEY") {
		t.Errorf("error does not name BABKI_ENCRYPTION_KEY: %v", err)
	}
	if !strings.Contains(err.Error(), "openssl rand -hex 32") {
		t.Errorf("error does not give the exact command to generate a key: %v", err)
	}
}

// TestSetupKeylessRolesReachDatabaseConnectWithoutAKey covers the migrate
// role's own shape directly: setup(ctx, false, false).
//
// The pre-existing end-to-end coverage for "migrate and seed work without a
// key" is TestSeedDemo (cmd/babki/seed_test.go) — but that test calls
// seedDemo directly against a container-provided pool, never setup, so it
// cannot see whether setup's requireEncryptionKey=false path actually skips
// secretbox.ParseKey. The only thing standing between "migrate and seed work
// without a key" and "every role requires one" is two boolean literals at
// setup's two keyless call sites (newMigrateCmd in root.go, newSeedCmd in
// seed.go); flipping either back to true would leave the whole suite green
// without this test, because nothing else calls setup with both arguments
// false. This pins the fact at the unit that does the gating.
//
// BABKI_DATABASE_URL points at 127.0.0.1:1 — a port nothing can be listening
// on, since binding a listener there needs root — rather than the
// port-5432-but-unreachable style DSN used above, on purpose: this test's
// point is the OPPOSITE of that one's. setup(ctx, true, true) must fail on
// the key WITHOUT reaching db.Connect; setup(ctx, false, false) must reach
// db.Connect and fail there instead. No Docker is needed either way — the
// connection is refused immediately rather than timing out.
func TestSetupKeylessRolesReachDatabaseConnectWithoutAKey(t *testing.T) {
	ctx := context.Background()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	t.Setenv("BABKI_DATABASE_URL", "postgres://u:p@127.0.0.1:1/babki")
	t.Setenv("BABKI_ENCRYPTION_KEY", "")

	_, err := setup(ctx, false, false)
	if err == nil {
		t.Fatal("setup(ctx, false, false) succeeded against an unreachable database; want a connection error")
	}
	if strings.Contains(err.Error(), "BABKI_ENCRYPTION_KEY") {
		t.Fatalf("setup(ctx, false, false) failed on the encryption key (%v); migrate and seed must never require one", err)
	}
}

// TestBuildBoxCarriesTheValidatedKeyIntoOneBox pins buildBox's whole reason
// to exist: the *secretbox.Box it returns when the key is required is built
// from the SAME key material cfg.EncryptionKey decodes to — not a stray
// zero value, and not some other key that merely happens to round-trip
// against itself. A Seal-then-Open on buildBox's own Box alone would not
// tell the two apart (any working Box round-trips against itself
// regardless of which key it holds), so this instead builds a second,
// independent Box straight from secretbox.ParseKey/New — bypassing buildBox
// entirely — and requires each Box to be able to open what the OTHER
// sealed. That only holds if both were built from the same key.
func TestBuildBoxCarriesTheValidatedKeyIntoOneBox(t *testing.T) {
	cfg := &config.Config{EncryptionKey: validHexKey}
	box, err := buildBox(cfg, true)
	if err != nil {
		t.Fatalf("buildBox(requireEncryptionKey=true, valid key): %v", err)
	}
	if box == nil {
		t.Fatal("buildBox(requireEncryptionKey=true, valid key) returned a nil Box")
	}

	key, err := secretbox.ParseKey(validHexKey)
	if err != nil {
		t.Fatalf("secretbox.ParseKey(validHexKey): %v", err)
	}
	reference, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}

	plaintext := []byte("t.buildBox-key-identity-proof")
	got, err := reference.Open(box.Seal(plaintext))
	if err != nil {
		t.Fatalf("a Box built directly from validHexKey could not open what buildBox's Box sealed: %v — "+
			"buildBox is not using the key it was given", err)
	}
	if string(got) != string(plaintext) {
		t.Errorf("roundtrip via the independently-built reference Box = %q, want %q", got, plaintext)
	}
}

// TestBuildBoxNilWhenKeyNotRequired covers the migrate/seed/version shape at
// the unit that decides it: requireEncryptionKey=false must return a nil Box
// and no error, without even looking at whether cfg.EncryptionKey parses.
// The empty key here is deliberate — the zero value config.Config.Load
// produces when BABKI_ENCRYPTION_KEY is unset — so a regression that started
// validating the key regardless of requireEncryptionKey would fail on the
// returned error, not silently hand back a Box nobody asked for.
func TestBuildBoxNilWhenKeyNotRequired(t *testing.T) {
	cfg := &config.Config{EncryptionKey: ""}
	box, err := buildBox(cfg, false)
	if err != nil {
		t.Fatalf("buildBox(requireEncryptionKey=false): %v", err)
	}
	if box != nil {
		t.Fatal("buildBox(requireEncryptionKey=false) returned a non-nil Box; migrate/seed/version must never build one")
	}
}

// TestVersionRoleNeedsNoEncryptionKey exercises the version command through
// the real cobra tree, the way it is actually invoked: it never calls setup
// at all, so BABKI_ENCRYPTION_KEY being unset must not matter to it.
func TestVersionRoleNeedsNoEncryptionKey(t *testing.T) {
	t.Setenv("BABKI_ENCRYPTION_KEY", "")

	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("version command failed with no BABKI_ENCRYPTION_KEY: %v", err)
	}
	if buf.Len() == 0 {
		t.Error("version command printed nothing")
	}
}
