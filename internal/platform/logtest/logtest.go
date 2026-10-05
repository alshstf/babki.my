// Package logtest captures slog records in tests. Every level is kept, so a
// line demoted to Debug fails as "wrong level", not as "missing": a test can
// assert the level a line was written at, which is what decides whether
// anyone sees it in production.
package logtest

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Capture is an slog.Handler that keeps every record. The zero value is
// ready to use.
type Capture struct {
	mu   sync.Mutex
	list []slog.Record
}

func (c *Capture) Enabled(context.Context, slog.Level) bool { return true }

func (c *Capture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.list = append(c.list, r.Clone())
	return nil
}

func (c *Capture) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &withAttrs{capture: c, attrs: attrs}
}

// WithGroup panics: grouped attributes would be misread as flat ones.
func (c *Capture) WithGroup(string) slog.Handler {
	panic("logtest: grouped attributes are not modelled; teach Capture if a caller starts using them")
}

// withAttrs is a logger.With child: its records carry its attributes too.
type withAttrs struct {
	capture *Capture
	attrs   []slog.Attr
}

func (h *withAttrs) Enabled(context.Context, slog.Level) bool { return true }

func (h *withAttrs) Handle(ctx context.Context, r slog.Record) error {
	r = r.Clone()
	r.AddAttrs(h.attrs...)
	return h.capture.Handle(ctx, r)
}

func (h *withAttrs) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &withAttrs{capture: h.capture, attrs: append(slices.Clip(h.attrs), attrs...)}
}

func (h *withAttrs) WithGroup(name string) slog.Handler { return h.capture.WithGroup(name) }

// Logger is a logger writing into c.
func (c *Capture) Logger() *slog.Logger { return slog.New(c) }

// Default installs a new Capture as slog's default for the test and restores
// the previous default afterwards.
func Default(t *testing.T) *Capture {
	t.Helper()
	c := &Capture{}
	previous := slog.Default()
	slog.SetDefault(c.Logger())
	t.Cleanup(func() { slog.SetDefault(previous) })
	return c
}

// Records is a snapshot of everything captured so far.
func (c *Capture) Records() []slog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.list)
}

// Lines is every captured record with exactly msg.
func (c *Capture) Lines(msg string) []slog.Record {
	var out []slog.Record
	for _, r := range c.Records() {
		if r.Message == msg {
			out = append(out, r)
		}
	}
	return out
}

// Only requires exactly one record with msg and returns it.
func Only(t *testing.T, c *Capture, msg string) slog.Record {
	t.Helper()
	lines := c.Lines(msg)
	if len(lines) != 1 {
		t.Fatalf("%d records say %q, want exactly 1; everything captured: %s",
			len(lines), msg, Describe(c.Records()))
	}
	return lines[0]
}

// AssertOne requires exactly one record with msg, at exactly want, whose
// attributes name cause, and returns it.
func AssertOne(t *testing.T, c *Capture, msg string, want slog.Level, cause string) slog.Record {
	t.Helper()
	r := Only(t, c, msg)
	if r.Level != want {
		t.Fatalf("%q was logged at %s, want %s; everything captured: %s",
			msg, r.Level, want, Describe(c.Records()))
	}
	if got := Attrs(r); !strings.Contains(got, cause) {
		t.Fatalf("%q carried %s, which does not name the cause %q — a line nobody can act on is barely better than silence",
			msg, got, cause)
	}
	return r
}

// AssertNone requires that nothing was logged with msg.
func AssertNone(t *testing.T, c *Capture, msg string) {
	t.Helper()
	if lines := c.Lines(msg); len(lines) != 0 {
		t.Fatalf("%q was logged at %s and should not have been; everything captured: %s",
			msg, lines[0].Level, Describe(c.Records()))
	}
}

// Attr is the value of the attribute key, or "" when r has none.
func Attr(r slog.Record, key string) string {
	v, _ := lookup(r, key)
	return v
}

func lookup(r slog.Record, key string) (string, bool) {
	var got string
	found := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			got, found = a.Value.String(), true
			return false
		}
		return true
	})
	return got, found
}

// AttrIs requires the attribute key to be exactly want: "0" is a substring of
// "connections=10".
func AttrIs(t *testing.T, r slog.Record, key, want string) {
	t.Helper()
	got, found := lookup(r, key)
	if !found {
		t.Fatalf("%q carries no %q attribute at all; it has %s", r.Message, key, Attrs(r))
	}
	if got != want {
		t.Fatalf("%q says %s=%q, want %q", r.Message, key, got, want)
	}
}

// Attrs flattens a record's attributes into one string.
func Attrs(r slog.Record) string {
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

// Describe renders records for a failure message.
func Describe(records []slog.Record) string {
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
		if attrs := Attrs(r); attrs != "" {
			b.WriteString(" [")
			b.WriteString(strings.TrimSpace(attrs))
			b.WriteString("]")
		}
	}
	return b.String()
}
