package marketdata

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// Log capture that keeps whole records, so tests assert the level a line was
// written at. Exported from this test file so the external test package can
// use it too.

// LogCapture is an slog.Handler that keeps every record, at every level, so a
// demoted line fails as "wrong level", not "missing".
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

// CaptureLogs installs the capture as slog's default for the test and
// restores the previous one afterwards.
func CaptureLogs(t *testing.T) *LogCapture {
	t.Helper()
	c := &LogCapture{}
	previous := slog.Default()
	slog.SetDefault(slog.New(c))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return c
}

// AssertOneRecordAt requires exactly one record with msg, at exactly want,
// naming cause.
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

// AssertNoRecord requires that nothing was logged under msg.
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

// DescribeRecords renders records' levels and messages for a failure.
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

// AttrsOf flattens a record's attributes into one string.
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
