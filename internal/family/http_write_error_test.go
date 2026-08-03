package family_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/money"
)

// recordingHandler keeps every record written to it, so an assertion can look
// at the LEVEL and at the attributes rather than searching a formatted buffer
// for a substring — a substring says nothing about whether the line was an
// error or a debug note that happens to mention the same words.
type recordingHandler struct{ records []slog.Record }

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r)
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler {
	panic("recordingHandler: attributes are not modelled; teach it if a caller starts using them")
}

func (h *recordingHandler) WithGroup(string) slog.Handler {
	panic("recordingHandler: groups are not modelled; teach it if a caller starts using them")
}

// TestWriteErrorLogsTheCauseBehindA500 pins the one place an unmapped error
// can still be read. The client is told "internal error" and nothing else, and
// the request log records only method, path, status and duration — so without
// this line the context every caller carefully builds ("balance of account
// <uuid> in RUB") reaches nobody at all, and the owner's blank screen is
// undiagnosable even with server access.
func TestWriteErrorLogsTheCauseBehindA500(t *testing.T) {
	handler := &recordingHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })

	cause := fmt.Errorf("balance of account 11111111-1111-1111-1111-111111111111 in RUB: %w", money.ErrOverflow)
	rec := httptest.NewRecorder()
	family.WriteError(rec, cause)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	// The body stays as it was: what the client is shown is a separate
	// decision from what the operator can find in the log.
	if body := rec.Body.String(); !strings.Contains(body, `"error":"internal error"`) {
		t.Errorf("body = %s, want the unchanged constant message", body)
	}
	if strings.Contains(rec.Body.String(), "11111111") {
		t.Errorf("body = %s: the error's own text must not reach the client", rec.Body.String())
	}

	if len(handler.records) != 1 {
		t.Fatalf("records = %d, want exactly 1", len(handler.records))
	}
	r := handler.records[0]
	if r.Level != slog.LevelError {
		t.Errorf("level = %v, want %v: a 500 nobody planned for is not an informational event", r.Level, slog.LevelError)
	}
	var logged string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "err" {
			logged = a.Value.String()
		}
		return true
	})
	if logged != cause.Error() {
		t.Errorf("logged err = %q, want the error's own text %q", logged, cause.Error())
	}
}

// TestWriteErrorDoesNotLogAMappedError keeps the log honest in the other
// direction. A validation failure or a missing row is an ordinary answer to an
// ordinary request; logging those at error level would bury the real one among
// them.
func TestWriteErrorDoesNotLogAMappedError(t *testing.T) {
	handler := &recordingHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })

	rec := httptest.NewRecorder()
	family.WriteError(rec, fmt.Errorf("%w: name is required", family.ErrValidation))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(handler.records) != 0 {
		t.Errorf("records = %d, want none: a 400 is an answer, not a failure", len(handler.records))
	}
}
