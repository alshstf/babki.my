package account

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata/ratetest"
	"babki.my/babki/internal/platform/money"
)

// A balance times a rate can overflow int64 (#27); it is an error, never the
// (nil, nil) that means "no rate".

// overflowOn is any fixed date; the double ignores it.
var overflowOn = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

func withBalance(minor int64) WithBalance {
	return WithBalance{
		Account: Account{Currency: "USD"},
		Balance: &BalancePoint{AmountMinor: minor, AsOf: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
	}
}

func TestBalanceInBaseRefusesABalanceThatWouldWrap(t *testing.T) {
	h := &Handler{converter: ratetest.Fixed{At: decimal.NewFromInt(2)}}

	got, err := h.balanceInBase(context.Background(), withBalance(math.MaxInt64), "RUB", overflowOn, marketdata.NewRateMemo(h.converter))
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("balanceInBase = %+v, err = %v; want ErrOverflow: twice maxint64 is not an int64", got, err)
	}
	if got != nil {
		t.Errorf("balanceInBase returned %+v alongside the refusal, want nil", got)
	}
}

// An overflow is an error, not the uncovered-currency null.
func TestBalanceInBaseOverflowIsNotAnUncoveredCurrency(t *testing.T) {
	h := &Handler{converter: ratetest.Fixed{At: decimal.NewFromInt(2)}}

	if _, err := h.balanceInBase(context.Background(), withBalance(math.MaxInt64), "RUB", overflowOn, marketdata.NewRateMemo(h.converter)); err == nil {
		t.Fatal("balanceInBase answered an overflow with a nil error, which this screen renders as a currency with no rate")
	}
}

// The same balance at a rate of 1 converts exactly.
func TestBalanceInBasePublishesTheLargestBalanceThatFits(t *testing.T) {
	h := &Handler{converter: ratetest.Fixed{At: decimal.NewFromInt(1)}}

	got, err := h.balanceInBase(context.Background(), withBalance(math.MaxInt64), "RUB", overflowOn, marketdata.NewRateMemo(h.converter))
	if err != nil {
		t.Fatalf("balanceInBase at exactly maxint64: %v", err)
	}
	if got == nil || got.AmountMinor != math.MaxInt64 {
		t.Errorf("balanceInBase = %+v, want %d", got, int64(math.MaxInt64))
	}
}
