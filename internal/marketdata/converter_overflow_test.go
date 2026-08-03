package marketdata_test

import (
	"errors"
	"math"
	"testing"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/money"
)

// TestConvertRefusesAnAmountThatWouldWrap covers the money path's own end of
// #27. Nothing about the inputs looks wrong — an amount that fits in the
// column it came out of, a rate of 2 — and the product does not fit. Before
// the guard, decimal.IntPart() wrapped it to MINUS TWO KOPECKS and Convert
// reported success.
func TestConvertRefusesAnAmountThatWouldWrap(t *testing.T) {
	conv, store, ctx := newConverterFixture(t)
	on := date("2026-07-01")

	err := store.UpsertFxRates(ctx, []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: dec("2"), Source: "cbr"},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := conv.Convert(ctx, math.MaxInt64, "USD", "RUB", on)
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("Convert(maxint64, USD->RUB @2) = %d, err = %v; want ErrOverflow, since twice that amount is not an int64", got, err)
	}
	if got != 0 {
		t.Errorf("Convert returned %d alongside the refusal, want 0", got)
	}
}

// TestConvertOverflowIsNotAMissingCurrency: ConvertMany sorts its per-entry
// failures into two piles, and they mean opposite things to a reader. A
// currency with no rate joins `missing` and the total is published beside a
// note naming it — an honest partial answer. An overflow must not land there:
// the total would be published without the very holding that broke it,
// looking exactly like a smaller portfolio. It fails the whole call instead.
func TestConvertOverflowIsNotAMissingCurrency(t *testing.T) {
	conv, store, ctx := newConverterFixture(t)
	on := date("2026-07-01")

	err := store.UpsertFxRates(ctx, []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: dec("2"), Source: "cbr"},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	converted, missing, _, err := conv.ConvertMany(ctx, map[string]int64{"USD": math.MaxInt64}, "RUB", on)
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("ConvertMany err = %v, want ErrOverflow", err)
	}
	if converted != 0 || missing != nil {
		t.Errorf("ConvertMany = (%d, %v) alongside the refusal, want (0, nil): an overflow is not a currency without a rate", converted, missing)
	}
}
