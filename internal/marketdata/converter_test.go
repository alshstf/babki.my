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
// It also pins the whole point of the batch: five queries whose enumeration
// names thirteen distinct (base, quote, date) rows — USD/RUB, RUB/USD,
// RUB/RUB, USD/EUR, EUR/USD, RUB/EUR, EUR/RUB, GBP/JPY, JPY/GBP, GBP/RUB,
// RUB/GBP, RUB/JPY, JPY/RUB — must cost exactly one round trip.
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
	if got.Len() != len(queries) {
		t.Fatalf("RatesOn returned %d results, want %d", got.Len(), len(queries))
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
		res, err := got.For(tc.query.From, tc.query.To, tc.query.On)
		if err != nil {
			t.Fatalf("RatesOn[%s]: For returned %v, want the resolved entry", tc.name, err)
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
	// neighbours above are all resolved in the very same batch. One exotic
	// holding must not blank out a page.
	res, err := got.For(unresolvable.From, unresolvable.To, unresolvable.On)
	if err != nil {
		t.Fatalf("RatesOn[GBP->JPY]: For returned %v, want the entry carrying ErrNoRate", err)
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
//
// Read what it does NOT cover, because the shape invites over-trusting it: a
// DIFFERENTIAL TEST BETWEEN TWO PATHS CANNOT POLICE ANY RULE THAT LIVES IN THE
// CODE THEY SHARE. Break the direct/inverse order, the 1/rate inversion, the
// bridge's insistence on both legs — the rule sits in resolveRate, both paths
// move together, and every comparison below still passes while every number is
// wrong. A reviewer confirmed exactly that: turning resolveRate's `ok1 && ok2`
// into `ok1 || ok2` left this whole package green. Every rule therefore needs a
// VALUE assertion somewhere that names its expected answer out loud, with no
// reference to the other path — TestRatesOnResolvesEveryPathInOneCall for the
// direct/inverse/bridge rates, TestOneRubLegAloneIsNoRate for the both-legs
// rule, TestBridgeRateDateIsOlderLeg for the bridge's date,
// TestDirectRowWinsOverTheInverseOfAReverseRow for the precedence. This test
// covers agreement, and only agreement.
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
		res, lookupErr := got.For(q.From, q.To, q.On)
		if lookupErr != nil {
			t.Fatalf("RatesOn[%+v]: For returned %v, want the resolved entry", q, lookupErr)
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
	if got.Len() != 10 {
		t.Fatalf("RatesOn returned %d results for 11 queries (one an exact duplicate), want 10", got.Len())
	}
}

// TestOneRubLegAloneIsNoRate is the both-legs rule stated as a value, not as
// agreement between the two paths: a bridge needs BOTH its legs, and a pair
// that has exactly one of them is ErrNoRate, never a rate.
//
// This is finding (2). Mutating resolveRate's `ok1 && ok2` into `ok1 || ok2`
// used to leave the entire package green, because the only test that exercised
// USD->JPY was the differential one above and the rule lives in the code both
// paths share (see that test's doc). What the mutation actually produced was
// rate 0 on a zero date with a nil error — a currency that merely lacks its RUB
// leg converting every amount to nothing, under a caption saying the number is
// good. Hence a hard-coded expectation here, on Rate and RatesOn alike.
//
// USD has a RUB leg in the shared fixture and JPY has none, so USD->JPY is the
// (ok1, !ok2) case and JPY->USD the (!ok1, ok2) one: neither ordering of the
// missing leg may resolve.
func TestOneRubLegAloneIsNoRate(t *testing.T) {
	conv, store, _, ctx := newConverterFixtureWithPool(t)
	seedRatesOnFixture(t, store, ctx)
	on := date("2026-07-03")

	fromLegOnly := marketdata.RateQuery{From: "USD", To: "JPY", On: on} // from->RUB exists, RUB->to does not
	toLegOnly := marketdata.RateQuery{From: "JPY", To: "USD", On: on}   // the other way round

	got, err := conv.RatesOn(ctx, []marketdata.RateQuery{fromLegOnly, toLegOnly})
	if err != nil {
		t.Fatalf("RatesOn: %v", err)
	}

	for _, q := range []marketdata.RateQuery{fromLegOnly, toLegOnly} {
		res, lookupErr := got.For(q.From, q.To, q.On)
		if lookupErr != nil {
			t.Fatalf("RatesOn[%s->%s]: For returned %v, want the entry", q.From, q.To, lookupErr)
		}
		if !errors.Is(res.Err, marketdata.ErrNoRate) {
			t.Fatalf("RatesOn[%s->%s].Err = %v, want ErrNoRate: one RUB leg is not a bridge (rate=%s, date=%v)",
				q.From, q.To, res.Err, res.Rate, res.RateDate)
		}
		// Spelled out because this is precisely what the broken version
		// returned, and a caller reading Rate without checking Err would have
		// shown it as a real conversion.
		if !res.Rate.IsZero() || !res.RateDate.IsZero() {
			t.Fatalf("RatesOn[%s->%s] failed but carries rate=%s date=%v, want both zero-valued",
				q.From, q.To, res.Rate, res.RateDate)
		}

		rate, rateDate, err := conv.Rate(ctx, q.From, q.To, q.On)
		if !errors.Is(err, marketdata.ErrNoRate) {
			t.Fatalf("Rate(%s->%s) = %s on %v, err = %v, want ErrNoRate: one RUB leg is not a bridge",
				q.From, q.To, rate, rateDate, err)
		}
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
		res, lookupErr := got.For(q.From, q.To, q.On)
		if lookupErr != nil || res.Err != nil {
			t.Fatalf("RatesOn[%s->%s] = %+v, For err=%v, want a resolved bridge", q.From, q.To, res, lookupErr)
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
	if err != nil || empty.Len() != 0 {
		t.Fatalf("RatesOn(nil) resolved %d entries, err = %v, want none and no error", empty.Len(), err)
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
		res, lookupErr := got.For(q.From, q.To, q.On)
		if lookupErr != nil || res.Err != nil || !res.Rate.Equal(decimal.NewFromInt(1)) || !res.RateDate.IsZero() {
			t.Fatalf("RatesOn[%s->%s] = %+v, For err=%v, want rate 1 with a zero date and no error", q.From, q.To, res, lookupErr)
		}
	}
}

// TestRatesOnPropagatesRealErrors is RatesOn's counterpart to
// TestRatePropagatesRealErrors: a DB or context failure fails the whole call
// (the returned Rates is the zero value and unusable, exactly as
// ConvertMany's total is), and must never be disguised as the per-query
// ErrNoRate that callers render as "not converted".
func TestRatesOnPropagatesRealErrors(t *testing.T) {
	conv, store, _, ctx := newConverterFixtureWithPool(t)
	seedRatesOnFixture(t, store, ctx)

	cctx, cancel := context.WithCancel(ctx)
	cancel()

	on := date("2026-07-03")
	got, err := conv.RatesOn(cctx, []marketdata.RateQuery{{From: "USD", To: "RUB", On: on}})
	if err == nil {
		t.Fatalf("RatesOn with canceled context: err = nil (got %d entries), want a real error", got.Len())
	}
	if errors.Is(err, marketdata.ErrNoRate) {
		t.Fatalf("RatesOn with canceled context: got ErrNoRate, want the underlying DB/context error")
	}
	if got.Len() != 0 {
		t.Fatalf("RatesOn with canceled context: %d entries resolved, want none", got.Len())
	}
	// And the voided batch stays unreadable: a caller that ignored err above
	// gets an error out of For, not a zero rate it would render as 0,00.
	if _, lookupErr := got.For("USD", "RUB", on); !errors.Is(lookupErr, marketdata.ErrNotRequested) {
		t.Fatalf("For on the voided batch: err = %v, want ErrNotRequested", lookupErr)
	}
}

// TestRatesForKeysByCalendarDayNotTimeValue is finding (1)'s regression test.
//
// RatesOn used to hand back a map[RateQuery]RateResult, and RateQuery holds a
// time.Time. Two time.Time values that name the same day but differ in
// *time.Location or in monotonic reading are DIFFERENT map keys, so a caller
// that built its query one way and indexed with another — time.Now().UTC()
// evaluated twice, a date passed through .In() or .Truncate() on only one of
// the two paths — got Go's zero RateResult back: rate zero, Err nil. Without
// the comma-ok that is indistinguishable from a pair that resolved to zero,
// and the handlers this batch exists for would have rendered every row's
// base-currency amount as 0,00 with no gap marker and no error at all.
//
// So the day is the key, and every spelling of one day finds the same entry.
// Under the old shape each case below except the first returned a silent zero;
// now each must return the rate that was actually resolved.
func TestRatesForKeysByCalendarDayNotTimeValue(t *testing.T) {
	conv, store, _, ctx := newConverterFixtureWithPool(t)
	seedRatesOnFixture(t, store, ctx)

	// Asked for at midnight UTC, the way date() builds it.
	asked := date("2026-07-03")
	got, err := conv.RatesOn(ctx, []marketdata.RateQuery{{From: "USD", To: "RUB", On: asked}})
	if err != nil {
		t.Fatalf("RatesOn: %v", err)
	}

	msk := time.FixedZone("UTC+3", 3*60*60)
	for _, tc := range []struct {
		name string
		on   time.Time
	}{
		{"the value the query was built from", asked},
		{"the same instant in another location", asked.In(msk)},
		{"the same wall clock in another location", time.Date(2026, 7, 3, 0, 0, 0, 0, msk)},
		{"the same day with a time of day on it", time.Date(2026, 7, 3, 15, 4, 5, 0, time.UTC)},
		{"the same day just before midnight", time.Date(2026, 7, 3, 23, 59, 59, 999999999, time.UTC)},
	} {
		res, lookupErr := got.For("USD", "RUB", tc.on)
		if lookupErr != nil {
			t.Fatalf("For(USD, RUB, %s): %v — same calendar day as the query, must find its entry", tc.name, lookupErr)
		}
		if res.Err != nil {
			t.Fatalf("For(USD, RUB, %s).Err = %v, want nil", tc.name, res.Err)
		}
		if !res.Rate.Equal(dec("91.2")) {
			t.Fatalf("For(USD, RUB, %s).Rate = %s, want 91.2 (a zero here is the exact bug this test exists for)", tc.name, res.Rate)
		}
	}

	// The other side of keying by the day: two spellings of one day in the
	// SAME call are one query, not two. They collapse onto a single entry, and
	// that entry is right for both — which holds only because the database is
	// asked for a `date` and pgx encodes one from the value's own wall-clock
	// Y/M/D, so a location or a time of day never moves the row that comes
	// back. If that ever stopped being true, the collapse would start hiding a
	// real difference, so it is checked here rather than assumed.
	sameDay := []marketdata.RateQuery{
		{From: "USD", To: "RUB", On: asked},
		{From: "USD", To: "RUB", On: time.Date(2026, 7, 3, 0, 0, 0, 0, msk)},
	}
	collapsed, err := conv.RatesOn(ctx, sameDay)
	if err != nil {
		t.Fatalf("RatesOn(two spellings of one day): %v", err)
	}
	if collapsed.Len() != 1 {
		t.Fatalf("RatesOn(two spellings of one day) resolved %d entries, want 1", collapsed.Len())
	}
	for _, q := range sameDay {
		res, lookupErr := collapsed.For(q.From, q.To, q.On)
		if lookupErr != nil || res.Err != nil {
			t.Fatalf("For(%v) = %+v, err=%v, want the shared entry", q.On, res, lookupErr)
		}
		if !res.Rate.Equal(dec("91.2")) || !res.RateDate.Equal(date("2026-07-03")) {
			t.Fatalf("For(%v) = rate %s on %v, want 91.2 on 2026-07-03 — the collapsed entry must be right for both spellings",
				q.On, res.Rate, res.RateDate)
		}
	}

	// The monotonic reading is the same hazard from the other direction, and
	// only time.Now() carries one. Same instant, same wall clock, same
	// location; the two values differ solely in that reading, which is enough
	// to make them different map keys and was enough to lose the lookup.
	now := time.Now()
	got, err = conv.RatesOn(ctx, []marketdata.RateQuery{{From: "USD", To: "RUB", On: now}})
	if err != nil {
		t.Fatalf("RatesOn(time.Now()): %v", err)
	}
	if _, lookupErr := got.For("USD", "RUB", now.Round(0)); lookupErr != nil {
		t.Fatalf("For with the monotonic reading stripped: %v, want the same entry", lookupErr)
	}
}

// TestRatesForRefusesATripleNobodyAsked closes the other half of finding (1):
// a lookup the batch was never given must be a LOUD error, not a zero result.
//
// It is the caller-side counterpart of errNotPrefetched, and the distinction
// matters for the same reason: "nobody worked this out" and "this pair has no
// rate" are different statements, and only the second is safe to show the user
// as a gap. A near miss is the realistic case — the right pair on the wrong
// day, the pair reversed — which is why those are the cases here.
func TestRatesForRefusesATripleNobodyAsked(t *testing.T) {
	conv, store, _, ctx := newConverterFixtureWithPool(t)
	seedRatesOnFixture(t, store, ctx)
	on := date("2026-07-03")

	got, err := conv.RatesOn(ctx, []marketdata.RateQuery{{From: "USD", To: "RUB", On: on}})
	if err != nil {
		t.Fatalf("RatesOn: %v", err)
	}

	for _, tc := range []struct {
		name     string
		from, to string
		on       time.Time
	}{
		{"a pair nobody asked about", "EUR", "RUB", on},
		{"the asked pair reversed", "RUB", "USD", on},
		{"the asked pair on another day", "USD", "RUB", date("2026-07-01")},
		{"the asked pair the day before", "USD", "RUB", date("2026-07-02")},
	} {
		res, lookupErr := got.For(tc.from, tc.to, tc.on)
		if lookupErr == nil {
			t.Fatalf("For(%s) returned %+v with no error — an unasked triple must never read as an answer", tc.name, res)
		}
		if !errors.Is(lookupErr, marketdata.ErrNotRequested) {
			t.Fatalf("For(%s): err = %v, want ErrNotRequested", tc.name, lookupErr)
		}
		// ErrNoRate is what a caller renders as an honest gap; a bug must not
		// borrow that costume.
		if errors.Is(lookupErr, marketdata.ErrNoRate) {
			t.Fatalf("For(%s): err = %v, want ErrNotRequested and NOT ErrNoRate — those mean different things to the user", tc.name, lookupErr)
		}
	}
}

// TestRatesForMissCarriesErrEvenIfDiscarded pins the hardening of For's miss
// path: a caller that discards the second return value entirely
// (res, _ := rates.For(...)) — the exact pattern every caller uses for the
// non-miss case, since the ordinary outcome lives in res.Err, not in the
// method's own error — must still be able to tell a miss from a pair that
// genuinely resolved to zero. Before this fix, the miss branch returned
// RateResult{} alongside its error: rate zero, RateDate zero, Err nil, so
// discarding the error handed back exactly the fabricated-zero shape this
// whole type exists to make impossible.
func TestRatesForMissCarriesErrEvenIfDiscarded(t *testing.T) {
	conv, store, _, ctx := newConverterFixtureWithPool(t)
	seedRatesOnFixture(t, store, ctx)
	on := date("2026-07-03")

	got, err := conv.RatesOn(ctx, []marketdata.RateQuery{{From: "USD", To: "RUB", On: on}})
	if err != nil {
		t.Fatalf("RatesOn: %v", err)
	}

	res, _ := got.For("EUR", "RUB", on) // the method's own error, deliberately discarded
	if res.Err == nil {
		t.Fatalf("For(unasked triple) with the error return discarded: res.Err = nil, want ErrNotRequested — a caller checking only res.Err must still see the miss")
	}
	if !errors.Is(res.Err, marketdata.ErrNotRequested) {
		t.Fatalf("For(unasked triple).Err = %v, want ErrNotRequested", res.Err)
	}
	if !res.Rate.IsZero() || !res.RateDate.IsZero() {
		t.Fatalf("For(unasked triple) = rate %s date %v, want both zero-valued alongside the error", res.Rate, res.RateDate)
	}
}

// TestNewRatesMatchesRatesOn is finding (2)'s pin: a Rates built by hand
// through NewRates must answer For exactly as one RatesOn produced, because
// NewRates exists specifically so that code outside this package — chiefly
// the test fakes standing in for the converter interface the position and
// journal handlers hide behind — can construct one to inject. If NewRates
// keyed its entries any differently than RatesOn does, a fake built on it
// would behave differently than the real thing it replaces, in whatever way
// nobody happened to test.
func TestNewRatesMatchesRatesOn(t *testing.T) {
	conv, store, _, ctx := newConverterFixtureWithPool(t)
	seedRatesOnFixture(t, store, ctx)

	direct := marketdata.RateQuery{From: "USD", To: "RUB", On: date("2026-07-03")}
	bridge := marketdata.RateQuery{From: "USD", To: "EUR", On: date("2026-07-03")}
	miss := marketdata.RateQuery{From: "GBP", To: "JPY", On: date("2026-07-03")}
	queries := []marketdata.RateQuery{direct, bridge, miss}

	want, err := conv.RatesOn(ctx, queries)
	if err != nil {
		t.Fatalf("RatesOn: %v", err)
	}

	// Round-trip RatesOn's own answers through NewRates: this is what a fake
	// wrapping a real Converter (or hand-picking values, as a fake forcing a
	// specific failure does) would do.
	results := make(map[marketdata.RateQuery]marketdata.RateResult, len(queries))
	for _, q := range queries {
		res, forErr := want.For(q.From, q.To, q.On)
		if forErr != nil {
			t.Fatalf("want.For(%+v): %v", q, forErr)
		}
		results[q] = res
	}
	got := marketdata.NewRates(results)

	if got.Len() != want.Len() {
		t.Fatalf("NewRates(...).Len() = %d, want %d (RatesOn's own)", got.Len(), want.Len())
	}
	for _, q := range queries {
		wantRes, _ := want.For(q.From, q.To, q.On)
		gotRes, forErr := got.For(q.From, q.To, q.On)
		if forErr != nil {
			t.Fatalf("NewRates(...).For(%+v): %v, want the entry RatesOn resolved", q, forErr)
		}
		if gotRes.Rate.String() != wantRes.Rate.String() || !gotRes.RateDate.Equal(wantRes.RateDate) {
			t.Fatalf("NewRates(...).For(%+v) = %+v, want %+v (RatesOn's own)", q, gotRes, wantRes)
		}
		if errors.Is(wantRes.Err, marketdata.ErrNoRate) != errors.Is(gotRes.Err, marketdata.ErrNoRate) {
			t.Fatalf("NewRates(...).For(%+v).Err = %v, want the same class as RatesOn's %v", q, gotRes.Err, wantRes.Err)
		}
	}

	// The calendar-day collapse: two different time.Time spellings of one day
	// in the input map land on the same lookupKey and so on the same entry —
	// exactly as RatesOn's own resolveQueries collapses them (see
	// TestRatesForKeysByCalendarDayNotTimeValue). Both spellings carry the
	// identical RateResult here, so which one the map iteration happens to
	// keep does not matter to the assertion.
	msk := time.FixedZone("UTC+3", 3*60*60)
	sharedResult := marketdata.RateResult{Rate: dec("91.2"), RateDate: date("2026-07-03")}
	collapsed := marketdata.NewRates(map[marketdata.RateQuery]marketdata.RateResult{
		{From: "USD", To: "RUB", On: date("2026-07-03")}:                       sharedResult,
		{From: "USD", To: "RUB", On: time.Date(2026, 7, 3, 18, 30, 0, 0, msk)}: sharedResult,
	})
	if collapsed.Len() != 1 {
		t.Fatalf("NewRates with two spellings of one day: Len() = %d, want 1", collapsed.Len())
	}
	for _, on := range []time.Time{date("2026-07-03"), time.Date(2026, 7, 3, 18, 30, 0, 0, msk)} {
		res, forErr := collapsed.For("USD", "RUB", on)
		if forErr != nil {
			t.Fatalf("collapsed.For(USD, RUB, %v): %v", on, forErr)
		}
		if !res.Rate.Equal(dec("91.2")) {
			t.Fatalf("collapsed.For(USD, RUB, %v).Rate = %s, want 91.2", on, res.Rate)
		}
	}

	// And a triple nobody supplied refuses exactly as RatesOn's own Rates
	// does: ErrNotRequested, not a silent zero.
	if _, forErr := collapsed.For("EUR", "RUB", date("2026-07-03")); !errors.Is(forErr, marketdata.ErrNotRequested) {
		t.Fatalf("collapsed.For(unasked triple): err = %v, want ErrNotRequested", forErr)
	}
}

// TestDirectRowWinsOverTheInverseOfAReverseRow pins the first rule of
// resolution: when both directions are stored, the direct row is the answer
// and the reverse row is not consulted at all. The two disagree here on
// purpose — 1/0.02 is 50, not the 90 the direct row says — because a published
// pair of rates does not have to be each other's exact reciprocal, and taking
// the wrong one produces a number that looks entirely plausible.
//
// Checked on Rate and RatesOn alike: they share the rule, so a mutation moves
// both and no test that compares them to each other would notice.
func TestDirectRowWinsOverTheInverseOfAReverseRow(t *testing.T) {
	conv, store, _, ctx := newConverterFixtureWithPool(t)
	on := date("2026-07-03")

	err := store.UpsertFxRates(ctx, []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: dec("90"), Source: "test"},
		{Base: "RUB", Quote: "USD", On: on, Rate: dec("0.02"), Source: "test"},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	forward := marketdata.RateQuery{From: "USD", To: "RUB", On: on}
	backward := marketdata.RateQuery{From: "RUB", To: "USD", On: on}

	got, err := conv.RatesOn(ctx, []marketdata.RateQuery{forward, backward})
	if err != nil {
		t.Fatalf("RatesOn: %v", err)
	}

	for _, tc := range []struct {
		query    marketdata.RateQuery
		wantRate decimal.Decimal
	}{
		{forward, dec("90")},    // its own row, not 1/0.02 = 50
		{backward, dec("0.02")}, // its own row, not 1/90
	} {
		res, lookupErr := got.For(tc.query.From, tc.query.To, tc.query.On)
		if lookupErr != nil || res.Err != nil {
			t.Fatalf("RatesOn[%s->%s] = %+v, For err=%v, want the stored direct rate", tc.query.From, tc.query.To, res, lookupErr)
		}
		if !res.Rate.Equal(tc.wantRate) {
			t.Fatalf("RatesOn[%s->%s].Rate = %s, want %s (the direct row, not the inverse of the reverse one)",
				tc.query.From, tc.query.To, res.Rate, tc.wantRate)
		}
		rate, _, err := conv.Rate(ctx, tc.query.From, tc.query.To, tc.query.On)
		if err != nil {
			t.Fatalf("Rate(%s->%s): %v", tc.query.From, tc.query.To, err)
		}
		if !rate.Equal(tc.wantRate) {
			t.Fatalf("Rate(%s->%s) = %s, want %s (the direct row, not the inverse of the reverse one)",
				tc.query.From, tc.query.To, rate, tc.wantRate)
		}
	}
}
