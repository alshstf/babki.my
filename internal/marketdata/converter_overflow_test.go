package marketdata_test

import (
	"errors"
	"math"
	"testing"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/money"
)

// An amount that fits times a rate of 2 can overflow; it is refused, not
// wrapped to minus two kopecks (#27).
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

// An overflow fails ConvertMany rather than joining `missing`, which would
// publish a total quietly short of the holding.
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

// Two balances that each convert fine can sum past int64; the total is
// refused too.
func TestConvertManyRefusesATotalThatWouldWrap(t *testing.T) {
	conv, store, ctx := newConverterFixture(t)
	on := date("2026-07-01")

	err := store.UpsertFxRates(ctx, []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: dec("1"), Source: "cbr"},
		{Base: "EUR", Quote: "RUB", On: on, Rate: dec("1"), Source: "cbr"},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	amounts := map[string]int64{"USD": math.MaxInt64, "EUR": math.MaxInt64}
	converted, missing, ratesOn, err := conv.ConvertMany(ctx, amounts, "RUB", on)
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("ConvertMany = (%d, %v, %v), err = %v; want ErrOverflow: each balance converts at a rate of 1 and only their sum is too large", converted, missing, ratesOn, err)
	}
	if converted != 0 || missing != nil || !ratesOn.IsZero() {
		t.Errorf("ConvertMany = (%d, %v, %v) alongside the refusal, want everything zero-valued", converted, missing, ratesOn)
	}
}
