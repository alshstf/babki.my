package marketdata

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// Capturing log records whole, so a test can assert the LEVEL a line was
// written at rather than that some line was written.
//
// A substring match against a log buffer cannot tell a Warn from a Debug, and a
// Debug is invisible at the level this application runs at (see
// config.Config.LogLevel) — so a test that only greps for the text passes just
// as happily after the line has been demoted into silence. This repository has
// already shipped one test that passed for exactly that reason.
//
// The helpers are EXPORTED from an internal test file so the external
// marketdata_test files can use them too (the same arrangement export_test.go
// uses): the dead-batch warning is asserted from outside the package and the
// uncovered-query error from inside it, and both must be read the same way. Two
// copies of "how do we check a log level" would be two things to keep in step.

// LogCapture is an slog.Handler that keeps every record whole.
//
// Enabled answers true for EVERY level on purpose. The point is to observe the
// level a line was written at, and a handler that filtered by level would make a
// line demoted to Debug simply disappear — the assertion below would then fail
// with "no such record" instead of "wrong level", which is the same colour for
// the wrong reason and would hide a genuine rename behind a genuine demotion.
type LogCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *LogCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *LogCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r.Clone())
	return nil
}

func (c *LogCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *LogCapture) WithGroup(string) slog.Handler      { return c }

// All returns a snapshot of everything captured so far.
func (c *LogCapture) All() []slog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]slog.Record, len(c.records))
	copy(out, c.records)
	return out
}

// CaptureLogs installs a recording handler as the process default for the rest
// of the test, and restores the previous one afterwards. slog.Default is where
// the code under test writes, for the same reason family.WriteError does: it
// holds no logger of its own and cmd/babki installs the configured one there
// (see runtime.go). Nothing in this package's tests runs in parallel, so one
// default at a time is enough.
func CaptureLogs(t *testing.T) *LogCapture {
	t.Helper()
	c := &LogCapture{}
	previous := slog.Default()
	slog.SetDefault(slog.New(c))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return c
}

// AssertOneRecordAt fails unless exactly one captured record carries msg, that
// record was written at want, and its attributes name cause.
//
// The level is compared for EQUALITY rather than "at least want". Every line
// this package writes has both neighbours wrong, and wrong in opposite
// directions — a demotion buries it under the default level, a promotion makes
// it read as news of a kind it is not — so "at least" would pass for exactly
// half of the mistakes worth catching.
func AssertOneRecordAt(t *testing.T, capture *LogCapture, msg string, want slog.Level, cause string) {
	t.Helper()
	records := capture.All()
	var matched []slog.Record
	for _, r := range records {
		if r.Message == msg {
			matched = append(matched, r)
		}
	}
	if len(matched) != 1 {
		t.Fatalf("%d records say %q, want exactly 1; everything captured: %s",
			len(matched), msg, DescribeRecords(records))
	}
	if matched[0].Level != want {
		t.Fatalf("%q was logged at %s, want %s; everything captured: %s",
			msg, matched[0].Level, want, DescribeRecords(records))
	}
	if got := AttrsOf(matched[0]); !strings.Contains(got, cause) {
		t.Fatalf("%q carried %s, which does not name the cause %q — a line nobody can act on is barely better than silence",
			msg, got, cause)
	}
}

// AssertNoRecord fails if anything at all was logged under msg. It is how a
// line's ABSENCE is asserted, which is a claim in its own right here: a false
// alarm devalues the very line that was added to be noticed.
func AssertNoRecord(t *testing.T, capture *LogCapture, msg string) {
	t.Helper()
	records := capture.All()
	for _, r := range records {
		if r.Message == msg {
			t.Fatalf("%q was logged at %s and should not have been; everything captured: %s",
				msg, r.Level, DescribeRecords(records))
		}
	}
}

// DescribeRecords renders captured records for a failure message: level and
// message, which is everything needed to tell a rename from a demotion.
func DescribeRecords(records []slog.Record) string {
	if len(records) == 0 {
		return "(nothing at all was logged)"
	}
	var b strings.Builder
	for i, r := range records {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(r.Level.String())
		b.WriteString(" ")
		b.WriteString(r.Message)
	}
	return b.String()
}

// AttrsOf flattens one record's attributes into a single string for the
// contains check above.
func AttrsOf(r slog.Record) string {
	var b strings.Builder
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(a.Key)
		b.WriteString("=")
		b.WriteString(a.Value.String())
		b.WriteString(" ")
		return true
	})
	return b.String()
}
