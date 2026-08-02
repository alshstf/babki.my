package marketdata_test

import (
	"context"
	"errors"
	"testing"

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
	pool := testdb.New(t)
	ctx := context.Background()
	store := marketdata.NewStore(pool)
	return marketdata.NewConverter(store), store, ctx
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
