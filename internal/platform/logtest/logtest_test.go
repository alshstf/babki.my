package logtest_test

import (
	"log/slog"
	"testing"

	"babki.my/babki/internal/platform/logtest"
)

// Every level is kept, and a logger.With child writes into the same capture
// with its attributes on each record.
func TestACaptureKeepsEveryLevelAndTheAttributesOfAChild(t *testing.T) {
	logs := &logtest.Capture{}
	log := logs.Logger()
	log.Debug("quiet", "n", 1)
	log.With("connection", "c-1").Warn("loud", "n", 2)

	logtest.AssertOne(t, logs, "quiet", slog.LevelDebug, "n=1")
	loud := logtest.AssertOne(t, logs, "loud", slog.LevelWarn, "n=2")
	logtest.AttrIs(t, loud, "connection", "c-1")
	if got := logtest.Attr(loud, "missing"); got != "" {
		t.Errorf("Attr of an absent key = %q, want empty", got)
	}
	logtest.AssertNone(t, logs, "never")
}

// Default captures what code logs through slog's default logger.
func TestDefaultCapturesTheDefaultLogger(t *testing.T) {
	logs := logtest.Default(t)
	slog.Info("through the default")
	logtest.AssertOne(t, logs, "through the default", slog.LevelInfo, "")
}
