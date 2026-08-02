package marketdata_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/testdb"
)

// newConverterFixture spins up a fresh, migrated DB and returns a Converter
// wired to it, plus the underlying Store (to seed rates) and a context.
// fx_rates has no foreign keys, so — unlike newFixture in store_test.go —
// no user/space/instrument setup is needed here.
func newConverterFixture(t *testing.T) (*marketdata.Converter, *marketdata.Store, context.Context) {
	t.Helper()
	conv, store, _, ctx := newConverterFixtureWithPool(t)
	return conv, store, ctx
}

// newConverterFixtureWithPool is newConverterFixture plus the pool underneath,
// for tests that count round trips through Stat().AcquireCount() (the
// technique store_test.go's TestFxRatesOnBatch uses).
func newConverterFixtureWithPool(t *testing.T) (*marketdata.Converter, *marketdata.Store, *pgxpool.Pool, context.Context) {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()
	store := marketdata.NewStore(pool)
	return marketdata.NewConverter(store), store, pool, ctx
}

func TestConvertSameCurrencyIsIdentity(t *testing.T) {
	conv, _, ctx := newConverterFixture(t)
	on := date("2026-07-01")

	// No rate seeded at all: from == to must short-circuit before any
	// lookup, positive and negative alike.
	for _, amount := range []int64{12345, -12345, 0} {
		got, err := conv.Convert(ctx, amount, "USD", "USD", on)
		if err != nil {
			t.Fatalf("Convert(%d, USD, USD): %v", amount, err)
		}
		if got != amount {
			t.Fatalf("Convert(%d, USD, USD) = %d, want %d unchanged", amount, got, amount)
		}
	}
}

func TestConvertDirectAndInverseRate(t *testing.T) {
	conv, store, ctx := newConverterFixture(t)
	on := date("2026-07-01")

	err := store.UpsertFxRates(ctx, []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: dec("90"), Source: "cbr"},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Direct: USD -> RUB uses the stored rate as-is.
	got, err := conv.Convert(ctx, 10000, "USD", "RUB", on) // 100.00 USD
	if err != nil {
		t.Fatalf("Convert direct: %v", err)
	}
	if got != 900000 { // 9000.00 RUB
		t.Fatalf("Convert direct USD->RUB = %d, want 900000", got)
	}

	// Inverse: RUB -> USD has no stored row, so it must fall back to 1/rate.
	got, err = conv.Convert(ctx, 900000, "RUB", "USD", on) // 9000.00 RUB
	if err != nil {
		t.Fatalf("Convert inverse: %v", err)
	}
	if got != 10000 { // 100.00 USD
		t.Fatalf("Convert inverse RUB->USD = %d, want 10000", got)
	}
}

func TestConvertBridgesThroughRUB(t *testing.T) {
	conv, store, ctx := newConverterFixture(t)
	on := date("2026-07-01")

	// Only USD/RUB and EUR/RUB exist (as cbr actually publishes them) — no
	// direct or inverse USD/EUR row anywhere.
	err := store.UpsertFxRates(ctx, []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: dec("90"), Source: "cbr"},
		{Base: "EUR", Quote: "RUB", On: on, Rate: dec("100"), Source: "cbr"},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// USD -> RUB -> EUR: 90 RUB/USD, then 1/100 EUR/RUB => 0.9 EUR/USD.
	got, err := conv.Convert(ctx, 10000, "USD", "EUR", on) // 100.00 USD
	if err != nil {
		t.Fatalf("Convert bridge: %v", err)
	}
	if got != 9000 { // 90.00 EUR
		t.Fatalf("Convert bridge USD->EUR = %d, want 9000", got)
	}
}

// TestConvertBridgeMatchesDirectRate checks that bridging through RUB is not
// just "some" answer but the mathematically correct one: converting a pair
// that only has a RUB bridge must produce the same minor-unit result as
// converting a different pair whose direct rate equals the bridge's implied
// rate (90 RUB/AAA * 1/100 EUR/RUB... = 0.9, matched by a direct 0.9 row).
func TestConvertBridgeMatchesDirectRate(t *testing.T) {
	conv, store, ctx := newConverterFixture(t)
	on := date("2026-07-01")

	err := store.UpsertFxRates(ctx, []marketdata.FxRate{
		// AAA -> BBB has no direct/inverse row; only a RUB bridge exists.
		{Base: "AAA", Quote: "RUB", On: on, Rate: dec("90"), Source: "test"},
		{Base: "BBB", Quote: "RUB", On: on, Rate: dec("100"), Source: "test"},
		// CCC -> DDD has a direct row at the bridge's implied rate (0.9).
		{Base: "CCC", Quote: "DDD", On: on, Rate: dec("0.9"), Source: "test"},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	bridged, err := conv.Convert(ctx, 10000, "AAA", "BBB", on)
	if err != nil {
		t.Fatalf("Convert bridged: %v", err)
	}
	direct, err := conv.Convert(ctx, 10000, "CCC", "DDD", on)
	if err != nil {
		t.Fatalf("Convert direct: %v", err)
	}
	if bridged != direct {
		t.Fatalf("bridge result %d != direct result %d for the same effective 0.9 rate", bridged, direct)
	}
}

func TestConvertNoRateReturnsSentinel(t *testing.T) {
	conv, store, ctx := newConverterFixture(t)
	on := date("2026-07-01")

	err := store.UpsertFxRates(ctx, []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: dec("90"), Source: "cbr"},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// GBP and JPY are both unrelated to the only stored pair (USD/RUB): no
	// direct, no inverse, and no RUB bridge (neither has a RUB leg).
	_, err = conv.Convert(ctx, 10000, "GBP", "JPY", on)
	if !errors.Is(err, marketdata.ErrNoRate) {
		t.Fatalf("Convert unrelated pair: err = %v, want ErrNoRate", err)
	}
}

// TestConvertRoundingIsHalfAwayFromZero documents and locks in the rounding
// decision for exact .5 minor units: symmetric half-away-from-zero, applied
// once at the end. This matters most for negative amounts (debts): a
// -150.5 minor-unit result rounds to -151, not -150 — rounding never shrinks
// the magnitude of a debt.
func TestConvertRoundingIsHalfAwayFromZero(t *testing.T) {
	conv, store, ctx := newConverterFixture(t)
	on := date("2026-07-01")

	err := store.UpsertFxRates(ctx, []marketdata.FxRate{
		{Base: "XXX", Quote: "YYY", On: on, Rate: dec("150.5"), Source: "test"},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// 1 * 150.5 = 150.5 -> rounds away from zero to 151, not down to 150.
	got, err := conv.Convert(ctx, 1, "XXX", "YYY", on)
	if err != nil {
		t.Fatalf("Convert positive .5: %v", err)
	}
	if got != 151 {
		t.Fatalf("Convert 1 * 150.5 = %d, want 151 (half rounds away from zero)", got)
	}

	// -1 * 150.5 = -150.5 -> rounds away from zero to -151, not -150.
	got, err = conv.Convert(ctx, -1, "XXX", "YYY", on)
	if err != nil {
		t.Fatalf("Convert negative .5: %v", err)
	}
	if got != -151 {
		t.Fatalf("Convert -1 * 150.5 = %d, want -151 (symmetric half-away-from-zero rounding for debts)", got)
	}
}

func TestConvertManySumsAndReportsMissing(t *testing.T) {
	conv, store, ctx := newConverterFixture(t)
	on := date("2026-07-01")

	err := store.UpsertFxRates(ctx, []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: dec("90"), Source: "cbr"},
		{Base: "EUR", Quote: "RUB", On: on, Rate: dec("100"), Source: "cbr"},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	amounts := map[string]int64{
		"USD": 10000, // 100.00 USD -> 9000.00 RUB = 900000
		"EUR": 5000,  // 50.00 EUR -> 5000.00 RUB = 500000
		"RUB": 500,   // identity, from == to
		"GBP": 2000,  // no rate path at all -> missing, not an error
	}
	converted, missing, ratesOn, err := conv.ConvertMany(ctx, amounts, "RUB", on)
	if err != nil {
		t.Fatalf("ConvertMany: %v", err)
	}
	wantConverted := int64(900000 + 500000 + 500)
	if converted != wantConverted {
		t.Fatalf("ConvertMany converted = %d, want %d", converted, wantConverted)
	}
	if len(missing) != 1 || missing[0] != "GBP" {
		t.Fatalf("ConvertMany missing = %v, want [GBP]", missing)
	}
	if !ratesOn.Equal(on) {
		t.Fatalf("ConvertMany ratesOn = %v, want %v (both USD and EUR rates are dated exactly on)", ratesOn, on)
	}
}

func TestConvertManyEmptyInput(t *testing.T) {
	conv, _, ctx := newConverterFixture(t)
	on := date("2026-07-01")

	converted, missing, ratesOn, err := conv.ConvertMany(ctx, map[string]int64{}, "RUB", on)
	if err != nil {
		t.Fatalf("ConvertMany empty: %v", err)
	}
	if converted != 0 || len(missing) != 0 {
		t.Fatalf("ConvertMany empty = (%d, %v), want (0, empty)", converted, missing)
	}
	if !ratesOn.IsZero() {
		t.Fatalf("ConvertMany empty ratesOn = %v, want zero value (nothing converted)", ratesOn)
	}
}

// TestConvertManyRatesOnIsOldestRateUsed is fix (1)'s core regression test:
// a summary must disclose how stale the fx rate behind it is, not silently
// imply "today's rate". Two currencies convert here — one against a rate
// dated exactly "on", the other against a rate two days older (FxRateOn's
// nearest-earlier-date fallback) — so ratesOn must surface that older date,
// not on, and not today.
func TestConvertManyRatesOnIsOldestRateUsed(t *testing.T) {
	conv, store, ctx := newConverterFixture(t)
	on := date("2026-07-10")
	older := date("2026-07-08") // two days before "on"

	err := store.UpsertFxRates(ctx, []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: dec("90"), Source: "cbr"},
		{Base: "EUR", Quote: "RUB", On: older, Rate: dec("100"), Source: "cbr"}, // stale
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	amounts := map[string]int64{"USD": 10000, "EUR": 5000}
	_, missing, ratesOn, err := conv.ConvertMany(ctx, amounts, "RUB", on)
	if err != nil {
		t.Fatalf("ConvertMany: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("ConvertMany missing = %v, want empty", missing)
	}
	if !ratesOn.Equal(older) {
		t.Fatalf("ConvertMany ratesOn = %v, want %v (the older of the two rates actually used, not %v)", ratesOn, older, on)
	}
}

// TestConvertManyRatesOnZeroWhenOnlyIdentity covers a base-currency-only
// summary: every amount is already in the target currency, so no fx rate is
// ever resolved and ratesOn must stay the zero value (the caller renders
// that as "null", never as a fabricated date).
func TestConvertManyRatesOnZeroWhenOnlyIdentity(t *testing.T) {
	conv, _, ctx := newConverterFixture(t)
	on := date("2026-07-01")

	amounts := map[string]int64{"RUB": 500}
	converted, missing, ratesOn, err := conv.ConvertMany(ctx, amounts, "RUB", on)
	if err != nil {
		t.Fatalf("ConvertMany: %v", err)
	}
	if converted != 500 || len(missing) != 0 {
		t.Fatalf("ConvertMany identity-only = (%d, %v), want (500, empty)", converted, missing)
	}
	if !ratesOn.IsZero() {
		t.Fatalf("ConvertMany identity-only ratesOn = %v, want zero value", ratesOn)
	}
}

func TestConvertManyPropagatesRealErrors(t *testing.T) {
	conv, store, ctx := newConverterFixture(t)
	on := date("2026-07-01")

	err := store.UpsertFxRates(ctx, []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: dec("90"), Source: "cbr"},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()

	// A real DB/context failure must surface as err, not be swallowed into
	// missing like an ordinary "no rate for this currency" case.
	converted, missing, ratesOn, err := conv.ConvertMany(cctx, map[string]int64{"USD": 100}, "RUB", on)
	if err == nil {
		t.Fatalf("ConvertMany with canceled context: err = nil (converted=%d, missing=%v, ratesOn=%v), want a real error", converted, missing, ratesOn)
	}
	if !ratesOn.IsZero() {
		t.Fatalf("ConvertMany with canceled context: ratesOn = %v, want zero value on error", ratesOn)
	}
	if errors.Is(err, marketdata.ErrNoRate) {
		t.Fatalf("ConvertMany with canceled context: got ErrNoRate, want the underlying DB/context error")
	}
}

// TestRateIdentityIsOneWithZeroDate documents Rate's identity short-circuit,
// mirroring TestConvertSameCurrencyIsIdentity: from == to must resolve to
// rate 1 and a zero rateDate without any DB lookup (no rate seeded at all).
func TestRateIdentityIsOneWithZeroDate(t *testing.T) {
	conv, _, ctx := newConverterFixture(t)
	on := date("2026-07-01")

	rate, rateDate, err := conv.Rate(ctx, "USD", "USD", on)
	if err != nil {
		t.Fatalf("Rate(USD, USD): %v", err)
	}
	if !rate.Equal(dec("1")) {
		t.Fatalf("Rate(USD, USD) = %s, want 1", rate)
	}
	if !rateDate.IsZero() {
		t.Fatalf("Rate(USD, USD) rateDate = %v, want zero value", rateDate)
	}
}

// TestRateMatchesConvertResult is Rate's core contract: applying the rate it
// returns to an amount by hand (decimal.Mul(...).Round(0), the same step
// convert uses internally) must produce bit-for-bit the same minor-unit
// result as calling Convert directly, for both a direct/inverse pair and a
// RUB bridge. This is what lets a caller memoize Rate per currency and apply
// it to N different amounts instead of calling Convert N times.
func TestRateMatchesConvertResult(t *testing.T) {
	conv, store, ctx := newConverterFixture(t)
	on := date("2026-07-01")

	err := store.UpsertFxRates(ctx, []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: dec("90"), Source: "cbr"},
		{Base: "EUR", Quote: "RUB", On: on, Rate: dec("100"), Source: "cbr"},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	for _, tc := range []struct {
		from, to string
		amount   int64
	}{
		{"USD", "RUB", 12345},  // direct
		{"RUB", "USD", 900000}, // inverse
		{"USD", "EUR", 10000},  // RUB bridge
	} {
		wantConverted, err := conv.Convert(ctx, tc.amount, tc.from, tc.to, on)
		if err != nil {
			t.Fatalf("Convert(%s->%s): %v", tc.from, tc.to, err)
		}

		rate, rateDate, err := conv.Rate(ctx, tc.from, tc.to, on)
		if err != nil {
			t.Fatalf("Rate(%s->%s): %v", tc.from, tc.to, err)
		}
		got := decimal.NewFromInt(tc.amount).Mul(rate).Round(0).IntPart()
		if got != wantConverted {
			t.Fatalf("Rate(%s->%s) applied by hand = %d, want %d (Convert's own result)", tc.from, tc.to, got, wantConverted)
		}
		if rateDate.IsZero() {
			t.Fatalf("Rate(%s->%s) rateDate = zero, want a real date", tc.from, tc.to)
		}
	}
}

// TestRateNoRateReturnsSentinel mirrors TestConvertNoRateReturnsSentinel:
// Rate must surface the same ErrNoRate sentinel Convert does for an
// unrelated pair, not a bare error or a zero-value rate mistaken for success.
func TestRateNoRateReturnsSentinel(t *testing.T) {
	conv, store, ctx := newConverterFixture(t)
	on := date("2026-07-01")

	err := store.UpsertFxRates(ctx, []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: dec("90"), Source: "cbr"},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, _, err = conv.Rate(ctx, "GBP", "JPY", on)
	if !errors.Is(err, marketdata.ErrNoRate) {
		t.Fatalf("Rate unrelated pair: err = %v, want ErrNoRate", err)
	}
}

func TestConvertPropagatesRealErrors(t *testing.T) {
	conv, _, ctx := newConverterFixture(t)
	on := date("2026-07-01")

	cctx, cancel := context.WithCancel(ctx)
	cancel()

	_, err := conv.Convert(cctx, 100, "USD", "RUB", on)
	if err == nil {
		t.Fatal("Convert with canceled context: err = nil, want a real error")
	}
	if errors.Is(err, marketdata.ErrNoRate) {
		t.Fatal("Convert with canceled context: got ErrNoRate, want the underlying DB/context error")
	}
}

// TestRatePropagatesRealErrors is Rate's counterpart to
// TestConvertPropagatesRealErrors / TestConvertManyPropagatesRealErrors: a
// genuine DB or context failure must come back as itself, never disguised as
// ErrNoRate.
//
// Every caller of Rate branches on errors.Is(err, ErrNoRate) to decide
// between "this pair simply has no rate — degrade honestly and carry on"
// and "something is broken — fail the request" (see
// account.Handler.balanceInBase and portfolio.Handler.positionInBase). If
// Rate ever collapsed the second case into the first, both handlers would
// dutifully render an outage as an ordinary missing rate and the user would
// never learn anything was wrong.
func TestRatePropagatesRealErrors(t *testing.T) {
	conv, store, ctx := newConverterFixture(t)
	on := date("2026-07-01")

	// Seed the pair so the failure below can only come from the canceled
	// context, not from the rate genuinely being absent.
	if err := store.UpsertFxRates(ctx, []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: dec("90"), Source: "cbr"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()

	rate, rateDate, err := conv.Rate(cctx, "USD", "RUB", on)
	if err == nil {
		t.Fatalf("Rate with canceled context: err = nil (rate=%s, rateDate=%v), want a real error", rate, rateDate)
	}
	if errors.Is(err, marketdata.ErrNoRate) {
		t.Fatalf("Rate with canceled context: got ErrNoRate, want the underlying DB/context error")
	}
}

// seedRatesOnFixture seeds the rate table every RatesOn test below shares:
// two USD/RUB rows (so nearest-earlier-date fallback has something to fall
// back to), one EUR/RUB row dated the same day as the newer USD/RUB row (a
// bridge whose legs agree on their date), and one CHF/RUB row dated days
// earlier (a bridge whose legs disagree — see TestBridgeRateDateIsOlderLeg).
func seedRatesOnFixture(t *testing.T, store *marketdata.Store, ctx context.Context) {
	t.Helper()
	err := store.UpsertFxRates(ctx, []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: date("2026-07-01"), Rate: dec("90"), Source: "cbr"},
		{Base: "USD", Quote: "RUB", On: date("2026-07-03"), Rate: dec("91.2"), Source: "cbr"},
		{Base: "EUR", Quote: "RUB", On: date("2026-07-03"), Rate: dec("100"), Source: "cbr"},
		{Base: "CHF", Quote: "RUB", On: date("2026-06-28"), Rate: dec("95"), Source: "cbr"},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// TestRatesOnResolvesEveryPathInOneCall pins the values, not just the
// agreement with Rate: a mutation that breaks a rule in the shared resolution
// (dropping the 1/rate inversion, say) moves Rate and RatesOn together and so
// stays invisible to the differential test below. Here the expected rates are
// spelled out independently of either method.
//
// It also pins the whole point of the batch: five queries whose resolution
// consults up to eleven distinct (base, quote, date) rows must cost exactly
// one round trip.
func TestRatesOnResolvesEveryPathInOneCall(t *testing.T) {
	conv, store, pool, ctx := newConverterFixtureWithPool(t)
	seedRatesOnFixture(t, store, ctx)
	on := date("2026-07-03")

	direct := marketdata.RateQuery{From: "USD", To: "RUB", On: on}
	inverse := marketdata.RateQuery{From: "RUB", To: "USD", On: on}
	bridge := marketdata.RateQuery{From: "USD", To: "EUR", On: on}
	identity := marketdata.RateQuery{From: "USD", To: "USD", On: on}
	unresolvable := marketdata.RateQuery{From: "GBP", To: "JPY", On: on}
	queries := []marketdata.RateQuery{direct, inverse, bridge, identity, unresolvable}

	before := pool.Stat().AcquireCount()
	got, err := conv.RatesOn(ctx, queries)
	if err != nil {
		t.Fatalf("RatesOn: %v", err)
	}
	if after := pool.Stat().AcquireCount(); after-before != 1 {
		t.Fatalf("RatesOn(%d queries) acquired %d connections, want exactly 1", len(queries), after-before)
	}
	if len(got) != len(queries) {
		t.Fatalf("RatesOn returned %d results, want %d: %+v", len(got), len(queries), got)
	}

	for _, tc := range []struct {
		name     string
		query    marketdata.RateQuery
		wantRate decimal.Decimal
		wantDate time.Time
	}{
		// The stored row itself.
		{"direct", direct, dec("91.2"), date("2026-07-03")},
		// No RUB/USD row exists, so it must be 1/91.2 — not 91.2.
		{"inverse", inverse, decimal.NewFromInt(1).Div(dec("91.2")), date("2026-07-03")},
		// 91.2 RUB/USD * 1/100 EUR/RUB = 0.912 EUR/USD.
		{"bridge", bridge, dec("0.912"), date("2026-07-03")},
		// Identity resolves nothing at all: rate 1, zero date.
		{"identity", identity, decimal.NewFromInt(1), time.Time{}},
	} {
		res, ok := got[tc.query]
		if !ok {
			t.Fatalf("RatesOn[%s] missing from the result map", tc.name)
		}
		if res.Err != nil {
			t.Fatalf("RatesOn[%s].Err = %v, want nil", tc.name, res.Err)
		}
		if !res.Rate.Equal(tc.wantRate) {
			t.Fatalf("RatesOn[%s].Rate = %s, want %s", tc.name, res.Rate, tc.wantRate)
		}
		if !res.RateDate.Equal(tc.wantDate) {
			t.Fatalf("RatesOn[%s].RateDate = %v, want %v", tc.name, res.RateDate, tc.wantDate)
		}
	}

	// The one pair nothing connects fails on its own line only: its four
	// neighbours above are all resolved in the very same map. One exotic
	// holding must not blank out a page.
	res, ok := got[unresolvable]
	if !ok {
		t.Fatalf("RatesOn[GBP->JPY] missing from the result map")
	}
	if !errors.Is(res.Err, marketdata.ErrNoRate) {
		t.Fatalf("RatesOn[GBP->JPY].Err = %v, want ErrNoRate", res.Err)
	}
}

// TestRatesOnMatchesRate is the no-drift test: for every shape of input —
// direct, inverse, bridge, identity, nearest-earlier-date fallback, a date
// before all data, a pair with only one RUB leg, a pair with none — the
// batched answer must be the single answer, down to the decimal's own
// representation, the resolved row's date and the error text. The two paths
// share one implementation of the rules precisely so this test can never
// find anything; it exists to notice the day someone forks them.
func TestRatesOnMatchesRate(t *testing.T) {
	conv, store, _, ctx := newConverterFixtureWithPool(t)
	seedRatesOnFixture(t, store, ctx)

	queries := []marketdata.RateQuery{
		{From: "USD", To: "RUB", On: date("2026-07-03")}, // direct, exact date
		{From: "RUB", To: "USD", On: date("2026-07-03")}, // inverse
		{From: "USD", To: "EUR", On: date("2026-07-03")}, // bridge, legs agree on the date
		{From: "USD", To: "CHF", On: date("2026-07-03")}, // bridge, legs disagree -> older leg
		{From: "CHF", To: "USD", On: date("2026-07-03")}, // same bridge the other way round
		{From: "USD", To: "RUB", On: date("2026-07-02")}, // no row that day -> nearest earlier
		{From: "USD", To: "RUB", On: date("2026-06-01")}, // before all data -> ErrNoRate
		{From: "USD", To: "USD", On: date("2026-07-03")}, // identity
		{From: "USD", To: "JPY", On: date("2026-07-03")}, // one RUB leg only -> ErrNoRate
		{From: "GBP", To: "JPY", On: date("2026-07-03")}, // no leg at all -> ErrNoRate
		{From: "USD", To: "RUB", On: date("2026-07-03")}, // exact duplicate -> collapses
	}

	got, err := conv.RatesOn(ctx, queries)
	if err != nil {
		t.Fatalf("RatesOn: %v", err)
	}

	for _, q := range queries {
		wantRate, wantDate, wantErr := conv.Rate(ctx, q.From, q.To, q.On)
		res, ok := got[q]
		if !ok {
			t.Fatalf("RatesOn[%+v] missing from the result map", q)
		}
		switch {
		case wantErr == nil && res.Err != nil:
			t.Fatalf("RatesOn[%+v].Err = %v, want nil (Rate succeeded)", q, res.Err)
		case wantErr != nil && res.Err == nil:
			t.Fatalf("RatesOn[%+v].Err = nil, want %v (Rate failed)", q, wantErr)
		case wantErr != nil:
			if errors.Is(wantErr, marketdata.ErrNoRate) != errors.Is(res.Err, marketdata.ErrNoRate) {
				t.Fatalf("RatesOn[%+v].Err = %v, want the same class as Rate's %v", q, res.Err, wantErr)
			}
			if res.Err.Error() != wantErr.Error() {
				t.Fatalf("RatesOn[%+v].Err = %q, want %q (Rate's own message)", q, res.Err, wantErr)
			}
		}
		// String, not Equal: "the numbers must not change" means the same
		// decimal, not merely a numerically equal one.
		if res.Rate.String() != wantRate.String() {
			t.Fatalf("RatesOn[%+v].Rate = %s, want %s (Rate's own)", q, res.Rate, wantRate)
		}
		if !res.RateDate.Equal(wantDate) {
			t.Fatalf("RatesOn[%+v].RateDate = %v, want %v (Rate's own)", q, res.RateDate, wantDate)
		}
	}

	// The duplicate query collapses onto its twin: 11 queries, 10 distinct.
	if len(got) != 10 {
		t.Fatalf("RatesOn returned %d results for 11 queries (one an exact duplicate), want 10: %+v", len(got), got)
	}
}

// TestBridgeRateDateIsOlderLeg pins the rule that a bridge is only as fresh
// as its stalest leg, in both directions so that "always take the first leg's
// date" and "always take the second's" are as red as "take the newer one".
// USD/RUB is dated 2026-07-03 and CHF/RUB 2026-06-28, so the older leg is the
// second one for USD->CHF and the first one for CHF->USD.
//
// This is checked on Rate and RatesOn alike: they share the rule, so a
// mutation moves both, and no test that compares them to each other would
// notice.
func TestBridgeRateDateIsOlderLeg(t *testing.T) {
	conv, store, _, ctx := newConverterFixtureWithPool(t)
	seedRatesOnFixture(t, store, ctx)
	on := date("2026-07-03")
	older := date("2026-06-28")

	forward := marketdata.RateQuery{From: "USD", To: "CHF", On: on}
	backward := marketdata.RateQuery{From: "CHF", To: "USD", On: on}

	got, err := conv.RatesOn(ctx, []marketdata.RateQuery{forward, backward})
	if err != nil {
		t.Fatalf("RatesOn: %v", err)
	}

	for _, q := range []marketdata.RateQuery{forward, backward} {
		res, ok := got[q]
		if !ok || res.Err != nil {
			t.Fatalf("RatesOn[%s->%s] = %+v, ok=%v, want a resolved bridge", q.From, q.To, res, ok)
		}
		if !res.RateDate.Equal(older) {
			t.Fatalf("RatesOn[%s->%s].RateDate = %v, want %v (the older of the two legs, not %v)",
				q.From, q.To, res.RateDate, older, on)
		}

		_, rateDate, err := conv.Rate(ctx, q.From, q.To, q.On)
		if err != nil {
			t.Fatalf("Rate(%s->%s): %v", q.From, q.To, err)
		}
		if !rateDate.Equal(older) {
			t.Fatalf("Rate(%s->%s) rateDate = %v, want %v (the older of the two legs, not %v)",
				q.From, q.To, rateDate, older, on)
		}
	}
}

// TestRatesOnCostsOneRoundTripWhateverTheCount is the count check made
// independent of the query set: one query and twenty-four cost the same one
// round trip. An implementation that loops FxRateOn (or FxRatesOn) per query
// passes every value check above and fails only here — which is the entire
// reason RatesOn exists.
func TestRatesOnCostsOneRoundTripWhateverTheCount(t *testing.T) {
	conv, store, pool, ctx := newConverterFixtureWithPool(t)
	seedRatesOnFixture(t, store, ctx)

	var many []marketdata.RateQuery
	for _, pair := range [][2]string{{"USD", "RUB"}, {"RUB", "USD"}, {"USD", "EUR"}, {"EUR", "CHF"}} {
		for _, day := range []string{"2026-06-29", "2026-07-01", "2026-07-02", "2026-07-03", "2026-07-04", "2026-07-05"} {
			many = append(many, marketdata.RateQuery{From: pair[0], To: pair[1], On: date(day)})
		}
	}

	for _, queries := range [][]marketdata.RateQuery{
		{{From: "USD", To: "RUB", On: date("2026-07-03")}},
		many,
	} {
		before := pool.Stat().AcquireCount()
		if _, err := conv.RatesOn(ctx, queries); err != nil {
			t.Fatalf("RatesOn(%d queries): %v", len(queries), err)
		}
		if after := pool.Stat().AcquireCount(); after-before != 1 {
			t.Fatalf("RatesOn(%d queries) acquired %d connections, want exactly 1", len(queries), after-before)
		}
	}
}

// TestRatesOnWithoutLookupsNeverTouchesTheStore covers the two inputs that
// resolve nothing: no queries at all, and queries that are all identity.
// Neither may cost a round trip — identity is a short-circuit in Rate, and it
// has to stay one here.
func TestRatesOnWithoutLookupsNeverTouchesTheStore(t *testing.T) {
	conv, _, pool, ctx := newConverterFixtureWithPool(t)
	on := date("2026-07-03")

	before := pool.Stat().AcquireCount()
	empty, err := conv.RatesOn(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("RatesOn(nil) = %+v, %v, want an empty map and no error", empty, err)
	}

	identities := []marketdata.RateQuery{
		{From: "USD", To: "USD", On: on},
		{From: "RUB", To: "RUB", On: on},
	}
	got, err := conv.RatesOn(ctx, identities)
	if err != nil {
		t.Fatalf("RatesOn(identities): %v", err)
	}
	if after := pool.Stat().AcquireCount(); after != before {
		t.Fatalf("RatesOn with nothing to resolve acquired %d connections (before=%d after=%d), want none",
			after-before, before, after)
	}
	for _, q := range identities {
		res, ok := got[q]
		if !ok || res.Err != nil || !res.Rate.Equal(decimal.NewFromInt(1)) || !res.RateDate.IsZero() {
			t.Fatalf("RatesOn[%s->%s] = %+v, ok=%v, want rate 1 with a zero date and no error", q.From, q.To, res, ok)
		}
	}
}

// TestRatesOnPropagatesRealErrors is RatesOn's counterpart to
// TestRatePropagatesRealErrors: a DB or context failure fails the whole call
// (the map is unusable, exactly as ConvertMany's total is), and must never be
// disguised as the per-query ErrNoRate that callers render as "not converted".
func TestRatesOnPropagatesRealErrors(t *testing.T) {
	conv, store, _, ctx := newConverterFixtureWithPool(t)
	seedRatesOnFixture(t, store, ctx)

	cctx, cancel := context.WithCancel(ctx)
	cancel()

	got, err := conv.RatesOn(cctx, []marketdata.RateQuery{{From: "USD", To: "RUB", On: date("2026-07-03")}})
	if err == nil {
		t.Fatalf("RatesOn with canceled context: err = nil (got %+v), want a real error", got)
	}
	if errors.Is(err, marketdata.ErrNoRate) {
		t.Fatalf("RatesOn with canceled context: got ErrNoRate, want the underlying DB/context error")
	}
	if got != nil {
		t.Fatalf("RatesOn with canceled context: map = %+v, want nil", got)
	}
}
