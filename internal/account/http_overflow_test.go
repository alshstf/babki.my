package account

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/platform/money"
)

// The accounts screen's end of #27. A balance is an int64 already; multiplied
// by a rate it need not stay one, and decimal.IntPart() answers that by
// wrapping rather than failing — a balance published as a small negative
// number on a screen whose whole job is to say how much there is.
//
// The refusal is an error, never the (nil, nil) that renders balance_in_base
// as null: that null means this currency has no rate the provider covers, and
// a balance too large to state is a different piece of news entirely.

// fixedRateConverter answers every lookup with the same rate.
type fixedRateConverter struct{ rate decimal.Decimal }

func (c fixedRateConverter) Rate(context.Context, string, string, time.Time) (decimal.Decimal, time.Time, error) {
	return c.rate, time.Time{}, nil
}

func (c fixedRateConverter) ConvertMany(context.Context, map[string]int64, string, time.Time) (int64, []string, time.Time, error) {
	panic("fixedRateConverter: ConvertMany not used")
}

func withBalance(minor int64) WithBalance {
	return WithBalance{
		Account: Account{Currency: "USD"},
		Balance: &BalancePoint{AmountMinor: minor, AsOf: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
	}
}

func TestBalanceInBaseRefusesABalanceThatWouldWrap(t *testing.T) {
	h := &Handler{converter: fixedRateConverter{rate: decimal.NewFromInt(2)}}

	got, err := h.balanceInBase(context.Background(), withBalance(math.MaxInt64), "RUB", map[string]*rateLookup{})
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("balanceInBase = %+v, err = %v; want ErrOverflow: twice maxint64 is not an int64", got, err)
	}
	if got != nil {
		t.Errorf("balanceInBase returned %+v alongside the refusal, want nil", got)
	}
}

// TestBalanceInBaseOverflowIsNotAnUncoveredCurrency: (nil, nil) is this
// function's word for a currency the rate provider does not cover, which the
// screen shows as a quiet null. An overflow must not be able to take that
// shape.
func TestBalanceInBaseOverflowIsNotAnUncoveredCurrency(t *testing.T) {
	h := &Handler{converter: fixedRateConverter{rate: decimal.NewFromInt(2)}}

	if _, err := h.balanceInBase(context.Background(), withBalance(math.MaxInt64), "RUB", map[string]*rateLookup{}); err == nil {
		t.Fatal("balanceInBase answered an overflow with a nil error, which this screen renders as a currency with no rate")
	}
}

// TestBalanceInBasePublishesTheLargestBalanceThatFits is the guard's other
// side: at a rate of 1 the same balance converts exactly, and is a figure the
// owner is entitled to see.
func TestBalanceInBasePublishesTheLargestBalanceThatFits(t *testing.T) {
	h := &Handler{converter: fixedRateConverter{rate: decimal.NewFromInt(1)}}

	got, err := h.balanceInBase(context.Background(), withBalance(math.MaxInt64), "RUB", map[string]*rateLookup{})
	if err != nil {
		t.Fatalf("balanceInBase at exactly maxint64: %v", err)
	}
	if got == nil || got.AmountMinor != math.MaxInt64 {
		t.Errorf("balanceInBase = %+v, want %d", got, int64(math.MaxInt64))
	}
}
