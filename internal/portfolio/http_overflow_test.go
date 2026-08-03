package portfolio

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/money"
)

// This file covers #27 where it reaches this screen. A quantity is validated
// as positive and nothing bounds it from above, a price is whatever the market
// data says, and their product can leave int64 without either input looking in
// the least unusual. decimal.IntPart() does not fail there — it wraps, to a
// small figure of arbitrary sign — so a holding could be published as worth
// minus a few kopecks.
//
// Every refusal here is an ERROR and not a null. The screen has nulls, and
// they mean something specific: no quote, no rate, no purchase date — data
// that is absent and may yet arrive. An overflow is broken input, permanent
// until someone fixes the journal, and dressing it as one of those gaps would
// put it back among the honest ones.

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// fixedRateConverter answers every rate lookup with the same rate, so a test
// can aim entirely at what the handler does with the product.
type fixedRateConverter struct{ rate decimal.Decimal }

func (c fixedRateConverter) Rate(context.Context, string, string, time.Time) (decimal.Decimal, time.Time, error) {
	return c.rate, time.Time{}, nil
}

func (c fixedRateConverter) RatesOn(context.Context, []marketdata.RateQuery) (marketdata.Rates, error) {
	return marketdata.Rates{}, nil
}

func TestMarketValueRefusesAShareValuationThatWouldWrap(t *testing.T) {
	q := marketdata.Quote{Price: dec("100"), Currency: "USD"}
	minor, currency, ok, err := marketValue(instrument.TypeShare, nil, nil, dec("1e17"), q)
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("marketValue(share) = (%d, %q, %v), err = %v; want ErrOverflow: 1e17 shares at 100 is not an int64 of cents", minor, currency, ok, err)
	}
	if ok {
		t.Error("marketValue reported ok alongside the refusal; the caller would publish a wrapped figure")
	}
}

// TestMarketValueRefusesABondValuationThatWouldWrap exercises the other
// branch, which multiplies by a face value instead of a price and therefore
// leaves int64 on entirely different inputs.
func TestMarketValueRefusesABondValuationThatWouldWrap(t *testing.T) {
	face := int64(1_000_000_000_000_000)
	faceCurrency := "RUB"
	q := marketdata.Quote{Price: dec("100"), Currency: "RUB"} // 100% of face
	minor, currency, ok, err := marketValue(instrument.TypeBond, &face, &faceCurrency, dec("1e5"), q)
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("marketValue(bond) = (%d, %q, %v), err = %v; want ErrOverflow", minor, currency, ok, err)
	}
	if ok {
		t.Error("marketValue reported ok alongside the refusal")
	}
}

// TestMarketValuePublishesTheLargestValuationThatFits is the other side of the
// guard. A valuation landing exactly on the largest int64 is a real figure and
// must still be published; only the next kopeck up is refused. A guard that
// stopped one unit early would withhold a number the owner is entitled to and
// say nothing about why.
func TestMarketValuePublishesTheLargestValuationThatFits(t *testing.T) {
	// price * quantity * 100 == math.MaxInt64, exactly.
	q := marketdata.Quote{Price: dec("92233720368547758.07"), Currency: "USD"}
	minor, currency, ok, err := marketValue(instrument.TypeShare, nil, nil, dec("1"), q)
	if err != nil || !ok {
		t.Fatalf("marketValue at exactly maxint64 = (%d, %q, %v), err = %v; want it published", minor, currency, ok, err)
	}
	if minor != math.MaxInt64 {
		t.Errorf("marketValue = %d, want %d", minor, int64(math.MaxInt64))
	}

	q.Price = dec("92233720368547758.08") // one kopeck further
	if _, _, ok, err := marketValue(instrument.TypeShare, nil, nil, dec("1"), q); !errors.Is(err, money.ErrOverflow) || ok {
		t.Errorf("marketValue one kopeck past maxint64: ok = %v, err = %v; want ErrOverflow", ok, err)
	}
}

func TestApplyToRefusesAConvertedAmountThatWouldWrap(t *testing.T) {
	rl := &rateLookup{rate: dec("2")}
	got, err := rl.applyTo(math.MaxInt64)
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("applyTo(maxint64) at rate 2 = %d, err = %v; want ErrOverflow", got, err)
	}
	if got != 0 {
		t.Errorf("applyTo returned %d alongside the refusal, want 0", got)
	}
}

// TestSumInBaseRefusesATotalThatWouldWrap aims at the total rather than at any
// one term: both terms below convert to a perfectly ordinary int64 on their
// own, and only their sum leaves the range. The guard therefore has to sit on
// the figure that gets published, which is the sum.
func TestSumInBaseRefusesATotalThatWouldWrap(t *testing.T) {
	h := &Handler{conv: fixedRateConverter{rate: decimal.NewFromInt(1)}}
	on := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	terms := []datedMinor{{minor: math.MaxInt64, on: on}, {minor: math.MaxInt64, on: on}}

	minor, ok, err := h.sumInBase(context.Background(), terms, "USD", "RUB", map[rateKey]*rateLookup{})
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("sumInBase = (%d, %v), err = %v; want ErrOverflow", minor, ok, err)
	}
	if ok || minor != 0 {
		t.Errorf("sumInBase = (%d, %v) alongside the refusal, want (0, false)", minor, ok)
	}
}

// TestSumInBaseOverflowIsNotAMissingRate keeps the refusal out of the pile it
// must never join. ok=false with a nil error is this function's word for "one
// of the dates has no fx rate", and positionInBase answers that by publishing
// in_base as null — a gap the screen explains as data not arrived yet. An
// overflow answered the same way would be a permanent breakage wearing a
// temporary gap's label, which is the defect the previous branch spent itself
// removing.
func TestSumInBaseOverflowIsNotAMissingRate(t *testing.T) {
	h := &Handler{conv: fixedRateConverter{rate: decimal.NewFromInt(1)}}
	on := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	terms := []datedMinor{{minor: math.MaxInt64, on: on}, {minor: math.MaxInt64, on: on}}

	if _, _, err := h.sumInBase(context.Background(), terms, "USD", "RUB", map[rateKey]*rateLookup{}); err == nil {
		t.Fatal("sumInBase answered an overflow with a nil error, which this handler reads as a missing rate and renders as a gap")
	}
}
