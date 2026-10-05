package marketdata_test

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/testdb"
)

// newConverterFixture returns a Converter over a fresh database, its Store and
// a context. fx_rates has no foreign keys, so nothing else is set up.
func newConverterFixture(t *testing.T) (*marketdata.Converter, *marketdata.Store, context.Context) {
	t.Helper()
	conv, store, _, ctx := newConverterFixtureWithPool(t)
	return conv, store, ctx
}

// newConverterFixtureWithPool also returns the pool, for counting round trips
// through Stat().AcquireCount().
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

	// No rate seeded: identity must not look anything up.
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

	// Only USD/RUB and EUR/RUB, as the CBR publishes them.
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

// The RUB bridge gives the same result as a direct row at the implied rate.
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

	// GBP and JPY have no direct, inverse or RUB legs.
	_, err = conv.Convert(ctx, 10000, "GBP", "JPY", on)
	if !errors.Is(err, marketdata.ErrNoRate) {
		t.Fatalf("Convert unrelated pair: err = %v, want ErrNoRate", err)
	}
}

// Exact halves round away from zero, so a debt never shrinks.
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

// ratesOn is the date of the oldest rate used — here the nearest-earlier
// fallback two days back — not on and not today.
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

// An all-identity summary resolves no rate, so ratesOn stays zero.
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

	// A real failure is err, not a currency in missing.
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

// Rate's identity short-circuit: rate 1, zero date, no lookup.
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

// Applying Rate's answer by hand gives exactly Convert's result, for a direct
// pair and a bridge — which is what lets callers memoize it.
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

// A real DB or context failure comes back as itself, never as ErrNoRate:
// callers degrade on ErrNoRate and fail on anything else.
func TestRatePropagatesRealErrors(t *testing.T) {
	conv, store, ctx := newConverterFixture(t)
	on := date("2026-07-01")

	// The pair is seeded, so the failure can only come from the canceled context.
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

// seedRatesOnFixture seeds two USD/RUB rows (for the nearest-earlier
// fallback), an EUR/RUB row on the newer USD date, and an older CHF/RUB row (a
// bridge whose legs disagree on date).
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

// The expected rates are spelled out independently of Rate, since a broken
// shared rule would move both. Five queries naming thirteen distinct rows cost
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

	// The unconnected pair fails alone; its neighbours resolve in the same batch.
	res, err := got.For(unresolvable.From, unresolvable.To, unresolvable.On)
	if err != nil {
		t.Fatalf("RatesOn[GBP->JPY]: For returned %v, want the entry carrying ErrNoRate", err)
	}
	if !errors.Is(res.Err, marketdata.ErrNoRate) {
		t.Fatalf("RatesOn[GBP->JPY].Err = %v, want ErrNoRate", res.Err)
	}
}

// RatesOn agrees with Rate for every shape of input, down to the decimal's
// representation, the date and the error text. Agreement only: rules live in
// the shared resolution, so their values are pinned by the tests beside this.
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
		// String, not Equal: the same decimal, not just an equal one.
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

// A bridge needs both legs: one RUB leg alone is ErrNoRate, never a rate. The
// broken `||` returned rate 0 with a nil error. Both orderings are checked.
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
		// Exactly what the broken version returned.
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

// A bridge is dated by its older leg, whichever leg that is, on Rate and
// RatesOn alike.
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

// One query and twenty-four cost the same single round trip.
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

// ConvertMany's cost does not grow with the number of currencies (#72): two
// widths, one round trip each.
func TestConvertManyCostsOneRoundTripWhateverTheCurrencies(t *testing.T) {
	conv, store, pool, ctx := newConverterFixtureWithPool(t)
	on := date("2026-07-03")

	// A direct row each, so every currency converts.
	currencies := []string{"USD", "EUR", "CHF", "GBP", "SEK", "TRY"}
	rates := make([]marketdata.FxRate, 0, len(currencies))
	for i, currency := range currencies {
		rates = append(rates, marketdata.FxRate{
			Base: currency, Quote: "RUB", On: on,
			Rate: decimal.NewFromInt(int64(10 + i)), Source: "cbr",
		})
	}
	if err := store.UpsertFxRates(ctx, rates); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for _, width := range []int{2, len(currencies)} {
		amounts := make(map[string]int64, width)
		for i, currency := range currencies[:width] {
			amounts[currency] = int64(1000 + i)
		}

		before := pool.Stat().AcquireCount()
		_, missing, _, err := conv.ConvertMany(ctx, amounts, "RUB", on)
		if err != nil {
			t.Fatalf("ConvertMany(%d currencies): %v", width, err)
		}
		// Nothing missing: a call converting nothing would be cheapest.
		if len(missing) != 0 {
			t.Fatalf("ConvertMany(%d currencies) left %v unconverted, so the trips below bought less than the whole total", width, missing)
		}
		if trips := pool.Stat().AcquireCount() - before; trips != 1 {
			t.Fatalf("ConvertMany(%d currencies) acquired %d connections, want exactly 1", width, trips)
		}
	}
}

// ConvertMany equals converting each entry alone, across direct, inverse,
// bridged and unconnected pairs.
func TestConvertManyMatchesConvertOneAtATime(t *testing.T) {
	conv, store, ctx := newConverterFixture(t)
	on := date("2026-07-03")

	// USD and EUR bridge to GBP through RUB, RUB->GBP inverts GBP/RUB, KZT has
	// nothing.
	if err := store.UpsertFxRates(ctx, []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: dec("91.2"), Source: "cbr"},
		{Base: "EUR", Quote: "RUB", On: date("2026-07-01"), Rate: dec("100"), Source: "cbr"},
		{Base: "GBP", Quote: "RUB", On: on, Rate: dec("120"), Source: "cbr"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	amounts := map[string]int64{
		"USD": 123456, // bridged: USD -> RUB -> GBP
		"EUR": -50000, // bridged too, and negative, where rounding is directional
		"RUB": 777777, // inverse: no RUB/GBP row exists, only GBP/RUB
		"GBP": 42,     // identity: no rate resolved at all
		"KZT": 999,    // nothing connects it: belongs in missing, not in the sum
	}

	var wantTotal int64
	var wantMissing []string
	for currency, amountMinor := range amounts {
		got, err := conv.Convert(ctx, amountMinor, currency, "GBP", on)
		if errors.Is(err, marketdata.ErrNoRate) {
			wantMissing = append(wantMissing, currency)
			continue
		}
		if err != nil {
			t.Fatalf("Convert(%s): %v", currency, err)
		}
		wantTotal += got
	}
	sort.Strings(wantMissing)

	converted, missing, _, err := conv.ConvertMany(ctx, amounts, "GBP", on)
	if err != nil {
		t.Fatalf("ConvertMany: %v", err)
	}
	if converted != wantTotal {
		t.Fatalf("ConvertMany = %d, want %d — the sum of the very same amounts converted one at a time", converted, wantTotal)
	}
	if !reflect.DeepEqual(missing, wantMissing) {
		t.Fatalf("ConvertMany missing = %v, want %v", missing, wantMissing)
	}
	// A zero total would match a batch that converted nothing.
	if wantTotal == 0 {
		t.Fatal("the fixture totals zero, which any broken batch would also produce")
	}
}

// No queries, or only identity ones, cost no round trip.
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

// A DB or context failure fails the whole call and is never disguised as a
// per-query ErrNoRate.
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
	// The voided Rates answers For with an error, not a zero rate.
	if _, lookupErr := got.For("USD", "RUB", on); !errors.Is(lookupErr, marketdata.ErrNotRequested) {
		t.Fatalf("For on the voided batch: err = %v, want ErrNotRequested", lookupErr)
	}
}

// Rates are keyed by calendar day: every spelling of a day (location,
// time of day, monotonic reading) finds the same entry rather than Go's zero
// RateResult.
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

	// Two spellings of one day in one call collapse onto one entry, which is right
	// for both because the database is asked for a `date`.
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

	// Only time.Now() carries a monotonic reading, which alone makes two
	// otherwise equal values different map keys.
	now := time.Now()
	got, err = conv.RatesOn(ctx, []marketdata.RateQuery{{From: "USD", To: "RUB", On: now}})
	if err != nil {
		t.Fatalf("RatesOn(time.Now()): %v", err)
	}
	if _, lookupErr := got.For("USD", "RUB", now.Round(0)); lookupErr != nil {
		t.Fatalf("For with the monotonic reading stripped: %v, want the same entry", lookupErr)
	}
}

// A triple the batch was never given is ErrNotRequested, not a zero result.
// Near misses — the wrong day, the pair reversed — are the realistic cases.
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
		// ErrNoRate would read as an honest gap.
		if errors.Is(lookupErr, marketdata.ErrNoRate) {
			t.Fatalf("For(%s): err = %v, want ErrNotRequested and NOT ErrNoRate — those mean different things to the user", tc.name, lookupErr)
		}
	}
}

// A caller discarding For's error still sees the miss in res.Err.
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

// A Rates built by NewRates answers For exactly as RatesOn's own, so fakes
// behave like the real thing.
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

	// Round-trip RatesOn's answers through NewRates, as a fake would.
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

	// Two spellings of one day collapse onto one entry, as in RatesOn.
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

	// An unsupplied triple is ErrNotRequested.
	if _, forErr := collapsed.For("EUR", "RUB", date("2026-07-03")); !errors.Is(forErr, marketdata.ErrNotRequested) {
		t.Fatalf("collapsed.For(unasked triple): err = %v, want ErrNotRequested", forErr)
	}
}

// When both directions are stored the direct row wins (1/0.02 is 50, the
// direct row says 90), on Rate and RatesOn alike.
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
