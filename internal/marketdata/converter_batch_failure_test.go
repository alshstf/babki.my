package marketdata_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/logtest"
)

// A failed batch fetch is answered correctly by per-pair lookups, so only a log
// line reveals it (#70); a canceled request is not such a failure (#98). The
// tests assert the level of the record, not just its text.

const deadBatchMessage = "batched fx rate lookup failed"

// assertOneWarning requires exactly one dead-batch record, at WARN, naming the
// cause: DEBUG would be invisible, ERROR would read as a failed request.
func assertOneWarning(t *testing.T, capture *logtest.Capture, cause string) {
	t.Helper()
	logtest.AssertOne(t, capture, deadBatchMessage, slog.LevelWarn, cause)
}

// deadBatch fails the batch Query while single-row QueryRow lookups answer
// from row.
type deadBatch struct {
	err error
	row scanRow
}

func (q deadBatch) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, q.err
}

func (q deadBatch) QueryRow(context.Context, string, ...any) pgx.Row {
	return fixedRow{scan: q.row}
}

func (q deadBatch) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	panic("deadBatch: SendBatch not used")
}

// fixedRow is one pgx.Row backed by a scanRow.
type fixedRow struct{ scan scanRow }

func (r fixedRow) Scan(dest ...any) error { return r.scan(dest...) }

// deadBatchConverter: the batch always fails, per-pair USD->RUB answers 90.
func deadBatchConverter(boom error) *marketdata.Converter {
	return marketdata.NewConverter(marketdata.NewStoreForRows(deadBatch{
		err: boom,
		row: fxRateRow(marketdata.FxRate{
			Base: "USD", Quote: "RUB", On: date("2026-06-30"),
			Rate: dec("90"), Source: "cbr",
		}),
	}))
}

// RatesOn's failure, which the handlers ignore by design, is logged as a
// warning.
func TestRatesOnLogsADeadBatchAsAWarning(t *testing.T) {
	boom := errors.New("canceling statement due to statement timeout")
	conv := deadBatchConverter(boom)
	capture := logtest.Default(t)

	got, err := conv.RatesOn(context.Background(), []marketdata.RateQuery{
		{From: "USD", To: "RUB", On: date("2026-07-01")},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("RatesOn err = %v, want %v", err, boom)
	}
	if got.Len() != 0 {
		t.Fatalf("RatesOn returned %d entries alongside the failure, want the zero Rates", got.Len())
	}
	assertOneWarning(t, capture, "statement timeout")
}

// ConvertMany's prefetch swallows the failure; it is still warned about, and
// the total is still right.
func TestConvertManyLogsADeadBatchAsAWarning(t *testing.T) {
	boom := errors.New("canceling statement due to statement timeout")
	conv := deadBatchConverter(boom)
	capture := logtest.Default(t)

	converted, missing, ratesOn, err := conv.ConvertMany(context.Background(),
		map[string]int64{"USD": 10000}, "RUB", date("2026-07-01"))
	if err != nil {
		t.Fatalf("ConvertMany: %v — a dead batch must not fail the call, the per-pair path answers it", err)
	}
	if converted != 900000 {
		t.Fatalf("ConvertMany = %d, want 900000 (10000 USD minor units at 90)", converted)
	}
	if len(missing) != 0 {
		t.Fatalf("ConvertMany left %v unconverted, want nothing — the fallback resolved the rate", missing)
	}
	if !ratesOn.Equal(date("2026-06-30")) {
		t.Fatalf("ConvertMany rates_on = %v, want 2026-06-30 (the row the fallback found)", ratesOn)
	}
	assertOneWarning(t, capture, "statement timeout")
}

// canceledBatchMessage is a different message, not a lower level: a canceled
// request is a different event.
const canceledBatchMessage = "batched fx rate lookup canceled"

// A canceled request logs at DEBUG and writes no dead-batch warning (#98).
func TestRatesOnDoesNotSoundTheAlarmForACanceledRequest(t *testing.T) {
	// An abandoned context; pgx fails the statement with the context's error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conv := deadBatchConverter(context.Canceled)
	capture := logtest.Default(t)

	if _, err := conv.RatesOn(ctx, []marketdata.RateQuery{
		{From: "USD", To: "RUB", On: date("2026-07-01")},
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("RatesOn err = %v, want context.Canceled — a cancellation still fails the call, it just is not news", err)
	}
	logtest.AssertOne(t, capture, canceledBatchMessage, slog.LevelDebug, "context canceled")
	logtest.AssertNone(t, capture, deadBatchMessage)
}

// An expired deadline still warns: it is the server's own slowness, the thing
// the warning exists to reveal.
func TestRatesOnStillWarnsWhenADeadlineExpires(t *testing.T) {
	conv := deadBatchConverter(context.DeadlineExceeded)
	capture := logtest.Default(t)

	if _, err := conv.RatesOn(context.Background(), []marketdata.RateQuery{
		{From: "USD", To: "RUB", On: date("2026-07-01")},
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RatesOn err = %v, want context.DeadlineExceeded", err)
	}
	assertOneWarning(t, capture, "deadline exceeded")
	logtest.AssertNone(t, capture, canceledBatchMessage)
}
