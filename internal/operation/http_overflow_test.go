package operation

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/marketdata/ratetest"
	"babki.my/babki/internal/platform/money"
)

// #27 on the journal: amount and fee are converted separately, each can pass
// int64, and an overflow is an error, never a null in_base, which would read as a
// rate the backfill will bring.

func overflowFixture() (*Handler, Operation) {
	h := &Handler{conv: ratetest.Fixed{At: decimal.NewFromInt(2)}}
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

	got, _, err := h.operationInBase(context.Background(), op, "RUB", marketdata.NewRateMemo(h.conv))
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("operationInBase = %+v, err = %v; want ErrOverflow: twice maxint64 is not an int64", got, err)
	}
	if got != nil {
		t.Errorf("operationInBase returned %+v alongside the refusal, want nil", got)
	}
}

// Only the fee leaves the range; its guard must hold on its own.
func TestOperationInBaseRefusesAFeeThatWouldWrap(t *testing.T) {
	h, op := overflowFixture()
	op.FeeMinor = math.MaxInt64

	got, _, err := h.operationInBase(context.Background(), op, "RUB", marketdata.NewRateMemo(h.conv))
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("operationInBase = %+v, err = %v; want ErrOverflow for the fee", got, err)
	}
	if got != nil {
		t.Errorf("operationInBase returned %+v alongside the refusal, want nil", got)
	}
}

// A nil object with a nil error means "no figure", a quiet gap; an overflow
// must not take that shape.
func TestOperationInBaseOverflowIsNotAMissingRate(t *testing.T) {
	h, op := overflowFixture()
	op.AmountMinor = math.MaxInt64

	if _, _, err := h.operationInBase(context.Background(), op, "RUB", marketdata.NewRateMemo(h.conv)); err == nil {
		t.Fatal("operationInBase answered an overflow with a nil error, which this page renders as a row that simply has no rate")
	}
}

// At rate 1, maxint64 converts to itself and is published.
func TestOperationInBasePublishesTheLargestFigureThatFits(t *testing.T) {
	h := &Handler{conv: ratetest.Fixed{At: decimal.NewFromInt(1)}}
	op := Operation{
		Type:        TypeDeposit,
		AmountMinor: math.MaxInt64,
		FeeMinor:    math.MaxInt64,
		Currency:    "USD",
		OccurredOn:  time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	}

	got, _, err := h.operationInBase(context.Background(), op, "RUB", marketdata.NewRateMemo(h.conv))
	if err != nil {
		t.Fatalf("operationInBase at exactly maxint64: %v", err)
	}
	if got == nil || got.AmountMinor != math.MaxInt64 || got.FeeMinor != math.MaxInt64 {
		t.Errorf("operationInBase = %+v, want both figures at %d", got, int64(math.MaxInt64))
	}
}
