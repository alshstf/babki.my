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

// recordingHandler keeps records so tests assert levels and attributes.
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

// An unmapped error behind a 500 is logged with its text: the client sees
// "internal error" and the request log has no cause.
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
	// The body is unchanged.
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

// A mapped error is not logged: it is an ordinary answer.
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
