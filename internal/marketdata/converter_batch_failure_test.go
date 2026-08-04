package marketdata_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
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
// warning is written, so all four are covered by the two tests below.
//
// WHAT IS UNDER TEST IS THE LEVEL, not that some line was written. A substring
// match against a log buffer cannot tell a Warn from a Debug — a Debug is
// invisible at this application's default level, which is the very silence #70
// is about — and this repository has already shipped one test that passed for
// exactly that reason. So records are captured structurally and the level is
// read off the record.

// deadBatchMessage is the line an operator greps for. Naming it here rather
// than matching loosely means a silent rename is a red test with a list of what
// WAS logged, not a test that quietly stops watching anything.
const deadBatchMessage = "batched fx rate lookup failed"

// logCapture is an slog.Handler that keeps every record whole.
//
// Enabled answers true for EVERY level on purpose. The point is to observe the
// level a line was written at, and a handler that filtered by level would make a
// line demoted to Debug simply disappear — the assertion below would then fail
// with "no such record" instead of "wrong level", which is the same colour for
// the wrong reason and would hide a genuine rename behind a genuine demotion.
type logCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r.Clone())
	return nil
}

func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

// all returns a snapshot of everything captured so far.
func (c *logCapture) all() []slog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]slog.Record, len(c.records))
	copy(out, c.records)
	return out
}

// captureLogs installs a recording handler as the process default for the rest
// of the test, and restores the previous one afterwards. slog.Default is where
// the code under test writes, for the same reason family.WriteError does: it
// holds no logger of its own and cmd/babki installs the configured one there
// (see runtime.go). Nothing in this package's tests runs in parallel, so one
// default at a time is enough.
func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{}
	previous := slog.Default()
	slog.SetDefault(slog.New(c))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return c
}

// assertOneWarning fails unless exactly one record carries msg, that record was
// written at WARN, and it names the cause.
//
// The level is compared for equality rather than "at least WARN". Both
// neighbours are wrong and for opposite reasons: DEBUG (or INFO) is invisible at
// the level this application runs at, which is the silence being fixed, and
// ERROR is what family.WriteError writes when a request actually failed — a
// batch that died while its fallback answered correctly must not read like a
// user who got no answer.
func assertOneWarning(t *testing.T, capture *logCapture, cause string) {
	t.Helper()
	records := capture.all()
	var matched []slog.Record
	for _, r := range records {
		if r.Message == deadBatchMessage {
			matched = append(matched, r)
		}
	}
	if len(matched) != 1 {
		t.Fatalf("%d records say %q, want exactly 1; everything captured: %s",
			len(matched), deadBatchMessage, describeRecords(records))
	}
	if matched[0].Level != slog.LevelWarn {
		t.Fatalf("%q was logged at %s, want %s — a dead batch is not a failed request (the answer is right) and not routine (the optimization is dead), and at %s it would not be printed at all under the default level",
			deadBatchMessage, matched[0].Level, slog.LevelWarn, matched[0].Level)
	}
	if got := attrsOf(matched[0]); !strings.Contains(got, cause) {
		t.Fatalf("%q carried %s, which does not name the cause %q — a warning nobody can act on is barely better than silence",
			deadBatchMessage, got, cause)
	}
}

// describeRecords renders captured records for a failure message: level and
// message, which is everything needed to tell a rename from a demotion.
func describeRecords(records []slog.Record) string {
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

// attrsOf flattens one record's attributes into a single string for the
// contains check above.
func attrsOf(r slog.Record) string {
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

// TestRatesOnLogsADeadBatchAsAWarning covers the three HTTP handlers at once:
// each of them warms its memo through RatesOn and drops the error on the floor
// by design, because the per-pair fallback below it answers correctly and an
// error page would be a worse outcome than a slow one. That deliberate silence
// is what left the failure undetectable, and this is the line that ends it.
func TestRatesOnLogsADeadBatchAsAWarning(t *testing.T) {
	boom := errors.New("canceling statement due to statement timeout")
	conv := deadBatchConverter(boom)
	capture := captureLogs(t)

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
	capture := captureLogs(t)

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
