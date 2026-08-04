package marketdata_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"

	"babki.my/babki/internal/marketdata"
)

// This file covers issue #70: a batched fx lookup that dies while the database
// is otherwise perfectly well — a statement timeout on the one large query, a
// problem encoding its array arguments — used to be met by nothing at all. The
// per-pair path resolves the same figures, so the screen comes out right and
// slow, and nobody learns the optimization stopped working.
//
// There are FOUR places that survive such a failure: the accounts, journal and
// positions handlers, all three through Converter.RatesOn, and Converter.prewarm
// inside ConvertMany, which swallows it outright and hands back the un-prefetched
// row source. All four go through one batched fetch, and that is where the
// warning is written, so the tests below cover THAT FETCH — the one place
// the four have in common. They execute no handler code: the handlers are
// reached by inference, from the fetch being warned about here plus each
// handler's own test pinning that it makes exactly one batched call. Pinning it
// end to end from a handler package is possible — a decorator over the pool
// failing only the batched statement — and was deliberately not done, because it
// would widen this package's exported surface for a chain that is already tight.
//
// It also covers #98, which is about the same fetch NOT warning: a request the
// reader abandoned kills the batch statement through its context, and that used
// to be written up as a breakage beside the genuine ones. See the last two tests
// and fetchRates' own doc for where the line between the two is drawn.
//
// WHAT IS UNDER TEST IS THE LEVEL, not that some line was written. A substring
// match against a log buffer cannot tell a Warn from a Debug — a Debug is
// invisible at this application's default level, which is the very silence #70
// is about — and this repository has already shipped one test that passed for
// exactly that reason. So records are captured structurally and the level is
// read off the record, through the helpers in logcapture_test.go that every
// level assertion in this package shares.

// deadBatchMessage is the line an operator greps for. Naming it here rather
// than matching loosely means a silent rename is a red test with a list of what
// WAS logged, not a test that quietly stops watching anything.
const deadBatchMessage = "batched fx rate lookup failed"

// assertOneWarning fails unless exactly one record carries deadBatchMessage,
// that record was written at WARN, and it names the cause.
//
// Both neighbours are wrong and for opposite reasons: DEBUG (or INFO) is
// invisible at the level this application runs at, which is the silence being
// fixed, and ERROR is what family.WriteError writes when a request actually
// failed — a batch that died while its fallback answered correctly must not
// read like a user who got no answer.
//
// The capture machinery it stands on lives in logcapture_test.go, shared with
// the tests that read the other two levels this package writes.
func assertOneWarning(t *testing.T, capture *marketdata.LogCapture, cause string) {
	t.Helper()
	marketdata.AssertOneRecordAt(t, capture, deadBatchMessage, slog.LevelWarn, cause)
}

// deadBatch is a source of rows whose BATCH query fails while single-row lookups
// keep working — exactly the shape of failure #70 is about, and one no real
// Postgres fixture can be asked to produce on demand. Store.FxRatesOn is the
// only reader here that goes through Query; Store.FxRateOn goes through
// QueryRow, so the per-pair fallback answers normally from row.
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

// deadBatchConverter builds a Converter whose batch statement always fails with
// boom and whose per-pair lookups always answer USD->RUB at 90.
func deadBatchConverter(boom error) *marketdata.Converter {
	return marketdata.NewConverter(marketdata.NewStoreForRows(deadBatch{
		err: boom,
		row: fxRateRow(marketdata.FxRate{
			Base: "USD", Quote: "RUB", On: date("2026-06-30"),
			Rate: dec("90"), Source: "cbr",
		}),
	}))
}

// TestRatesOnLogsADeadBatchAsAWarning covers the path all three HTTP handlers
// take: each of them warms its memo through RatesOn and drops the error on the floor
// by design, because the per-pair fallback below it answers correctly and an
// error page would be a worse outcome than a slow one. That deliberate silence
// is what left the failure undetectable, and this is the line that ends it.
func TestRatesOnLogsADeadBatchAsAWarning(t *testing.T) {
	boom := errors.New("canceling statement due to statement timeout")
	conv := deadBatchConverter(boom)
	capture := marketdata.CaptureLogs(t)

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

// TestConvertManyLogsADeadBatchAsAWarning covers the fourth site, and the one
// that is not a handler at all: Converter.prewarm swallows the failure inside
// ConvertMany and hands the loop the un-prefetched row source. Nothing above it
// ever sees an error, so before this line there was no place left where the
// degradation could be noticed.
//
// The total is asserted alongside the warning, because the two together are the
// whole claim: the batch died, the answer is still right — 10 000 minor units of
// USD at 90 — and the only thing lost is the round trip the batch was there to
// save.
func TestConvertManyLogsADeadBatchAsAWarning(t *testing.T) {
	boom := errors.New("canceling statement due to statement timeout")
	conv := deadBatchConverter(boom)
	capture := marketdata.CaptureLogs(t)

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

// canceledBatchMessage is the line a canceled batch writes instead of the
// warning. It is a DIFFERENT message rather than the same one at a lower level,
// because it reports a different event: not "the optimization died" but "nobody
// is waiting for this any more". An operator grepping for a dead batch must not
// find these, and one asking why a request stopped half-way must be able to.
const canceledBatchMessage = "batched fx rate lookup canceled"

// TestRatesOnDoesNotSoundTheAlarmForACanceledRequest is #98. A reader who
// navigates away cancels the request context, the batch statement dies with it,
// and until now that landed in the same branch as a statement timeout and was
// written up as a breakage. It is not one: nothing degraded, because there was
// no longer a request to be slow. False alarms devalue the very line #70 added
// to be noticed, so this one goes to DEBUG — present for whoever turns the level
// up, absent from the stream the warning lives in.
//
// Both halves are asserted. That the cancellation is recorded at DEBUG, and that
// the dead-batch warning is NOT written — the second being the whole point, and
// the one a test asserting only the first would let regress.
func TestRatesOnDoesNotSoundTheAlarmForACanceledRequest(t *testing.T) {
	// A context the reader has already abandoned, and a batch statement failing
	// the way pgx fails one: with the context's own error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conv := deadBatchConverter(context.Canceled)
	capture := marketdata.CaptureLogs(t)

	if _, err := conv.RatesOn(ctx, []marketdata.RateQuery{
		{From: "USD", To: "RUB", On: date("2026-07-01")},
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("RatesOn err = %v, want context.Canceled — a cancellation still fails the call, it just is not news", err)
	}
	marketdata.AssertOneRecordAt(t, capture, canceledBatchMessage, slog.LevelDebug, "context canceled")
	marketdata.AssertNoRecord(t, capture, deadBatchMessage)
}

// TestRatesOnStillWarnsWhenADeadlineExpires draws the line the cancellation rule
// stops at, and it is deliberate: only context.Canceled is routine.
// context.DeadlineExceeded is the server's OWN limit running out, which is the
// slowness the warning exists to reveal — demoting it would silence the batch
// failure most worth hearing about. The reader who leaves cancels; a deadline
// that expires is the program failing to be quick enough.
func TestRatesOnStillWarnsWhenADeadlineExpires(t *testing.T) {
	conv := deadBatchConverter(context.DeadlineExceeded)
	capture := marketdata.CaptureLogs(t)

	if _, err := conv.RatesOn(context.Background(), []marketdata.RateQuery{
		{From: "USD", To: "RUB", On: date("2026-07-01")},
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RatesOn err = %v, want context.DeadlineExceeded", err)
	}
	assertOneWarning(t, capture, "deadline exceeded")
	marketdata.AssertNoRecord(t, capture, canceledBatchMessage)
}
