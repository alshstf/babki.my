package operation

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/money"
)

// The journal's end of #27. An operation's amount and its fee are converted
// into the base currency independently, so each is its own way past int64, and
// decimal.IntPart() answers both by wrapping rather than failing.
//
// A refusal here is an error, never one of the gaps that render in_base as
// null: that null tells the reader "no rate for this day", a gap the fx
// backfill closes on its own. An amount too large to state is not waiting for
// a backfill.

// fixedRateConverter answers every lookup with the same rate.
type fixedRateConverter struct{ rate decimal.Decimal }

func (c fixedRateConverter) Rate(context.Context, string, string, time.Time) (decimal.Decimal, time.Time, error) {
	return c.rate, time.Time{}, nil
}

func (c fixedRateConverter) RatesOn(context.Context, []marketdata.RateQuery) (marketdata.Rates, error) {
	return marketdata.Rates{}, nil
}

func overflowFixture() (*Handler, Operation) {
	h := &Handler{conv: fixedRateConverter{rate: decimal.NewFromInt(2)}}
	return h, Operation{
		Type:        TypeDeposit,
		AmountMinor: 10_000,
		Currency:    "USD",
		OccurredOn:  time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestOperationInBaseRefusesAnAmountThatWouldWrap(t *testing.T) {
	h, op := overflowFixture()
	op.AmountMinor = math.MaxInt64

	got, _, err := h.operationInBase(context.Background(), op, "RUB", map[rateKey]*rateLookup{})
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("operationInBase = %+v, err = %v; want ErrOverflow: twice maxint64 is not an int64", got, err)
	}
	if got != nil {
		t.Errorf("operationInBase returned %+v alongside the refusal, want nil", got)
	}
}

// TestOperationInBaseRefusesAFeeThatWouldWrap keeps the fee's own guard
// honest: the amount here converts perfectly well, and only the fee leaves the
// range. The two are converted separately (a fee is a figure in its own right,
// not a term of the amount), so one guard cannot stand for the other.
func TestOperationInBaseRefusesAFeeThatWouldWrap(t *testing.T) {
	h, op := overflowFixture()
	op.FeeMinor = math.MaxInt64

	got, _, err := h.operationInBase(context.Background(), op, "RUB", map[rateKey]*rateLookup{})
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("operationInBase = %+v, err = %v; want ErrOverflow for the fee", got, err)
	}
	if got != nil {
		t.Errorf("operationInBase returned %+v alongside the refusal, want nil", got)
	}
}

// TestOperationInBaseOverflowIsNotAMissingRate: a nil object with a nil error
// is this function's word for "this row has no base-currency figure", which the
// journal renders as a quiet gap. An overflow must not be able to take that
// shape.
func TestOperationInBaseOverflowIsNotAMissingRate(t *testing.T) {
	h, op := overflowFixture()
	op.AmountMinor = math.MaxInt64

	if _, _, err := h.operationInBase(context.Background(), op, "RUB", map[rateKey]*rateLookup{}); err == nil {
		t.Fatal("operationInBase answered an overflow with a nil error, which this page renders as a row that simply has no rate")
	}
}

// TestOperationInBasePublishesTheLargestFigureThatFits is the other side of
// both guards: at a rate of exactly 1, maxint64 converts to maxint64 and is a
// real figure. A guard that refused it would withhold a publishable number.
func TestOperationInBasePublishesTheLargestFigureThatFits(t *testing.T) {
	h := &Handler{conv: fixedRateConverter{rate: decimal.NewFromInt(1)}}
	op := Operation{
		Type:        TypeDeposit,
		AmountMinor: math.MaxInt64,
		FeeMinor:    math.MaxInt64,
		Currency:    "USD",
		OccurredOn:  time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	}

	got, _, err := h.operationInBase(context.Background(), op, "RUB", map[rateKey]*rateLookup{})
	if err != nil {
		t.Fatalf("operationInBase at exactly maxint64: %v", err)
	}
	if got == nil || got.AmountMinor != math.MaxInt64 || got.FeeMinor != math.MaxInt64 {
		t.Errorf("operationInBase = %+v, want both figures at %d", got, int64(math.MaxInt64))
	}
}
