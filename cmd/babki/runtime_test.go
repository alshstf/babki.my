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

// validHexKey is a valid 64-hex-character BABKI_ENCRYPTION_KEY for tests that
// need any key. secretbox's tests have their own copy.
const validHexKey = "0123456789abcdef" +
	"0123456789abcdef" +
	"0123456789abcdef" +
	"0123456789abcdef"

// setup installs the configured logger as slog's default, which
// family.WriteError (no logger parameter, forty-odd callers) relies on to log the
// error behind every 500 in the configured stream, level and format. All three are
// asserted. setup fails on the missing database URL afterwards, keeping the test
// off a database.
func TestSetupInstallsTheConfiguredLoggerAsTheDefault(t *testing.T) {
	ctx := context.Background()

	// setup mutates a process-wide global. Put it back, or every test running
	// after this one in this package logs at error into a closed pipe.
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	t.Setenv("BABKI_DATABASE_URL", "")
	t.Setenv("BABKI_LOG_LEVEL", "error")
	t.Setenv("BABKI_LOG_FORMAT", "json")

	// logging.New captures os.Stderr when called, so the pipe goes in first.
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

	// Stream: it arrived on the pipe. Format: one JSON object with slog's
	// field names.
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

// A role that requires the key refuses to start without it, before dialling
// the database: the URL is valid but unreachable, so a reordered check would hang
// instead of failing fast.
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

// setup(ctx, false, false), the migrate and seed shape, reaches db.Connect
// without a key. Nothing else calls setup with both false; TestSeedDemo calls
// seedDemo directly. 127.0.0.1:1 refuses at once, so the failure is at the
// database, not the key.
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

// buildBox's Box uses the validated key: it and a Box built independently from
// the same key must open each other's seals, which a self round-trip would not
// prove.
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

// When the key is not required, buildBox returns nil and no error without
// parsing the (empty) key.
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

// version runs through the real command tree and never calls setup.
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

// While a key is being replaced, the box opens what the previous one sealed;
// a malformed previous key stops the start rather than being ignored.
func TestBuildBoxOpensWithThePreviousKey(t *testing.T) {
	previousHex := strings.Repeat("ab", 32)
	previousKey, _ := secretbox.ParseKey(previousHex)
	old, _ := secretbox.New(previousKey)
	sealed := old.Seal([]byte("token"))

	box, err := buildBox(&config.Config{EncryptionKey: validHexKey, EncryptionKeyPrevious: previousHex}, true)
	if err != nil {
		t.Fatalf("buildBox with a previous key: %v", err)
	}
	if got, err := box.Open(sealed); err != nil || string(got) != "token" {
		t.Errorf("open what the previous key sealed = %q, %v", got, err)
	}
	if _, err := buildBox(&config.Config{EncryptionKey: validHexKey, EncryptionKeyPrevious: "not-hex"}, true); err == nil ||
		!strings.Contains(err.Error(), "BABKI_ENCRYPTION_KEY_PREVIOUS") {
		t.Errorf("a malformed previous key = %v, want a refusal naming the variable", err)
	}
}
