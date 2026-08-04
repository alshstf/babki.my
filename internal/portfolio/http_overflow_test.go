package portfolio

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/money"
)

// This file covers #27 where it reaches this screen. A price is whatever the
// market data says, an fx rate likewise, and a product can leave int64 without
// any one input looking in the least unusual. decimal.IntPart() does not fail
// there — it wraps, to a small figure of arbitrary sign — so a holding could be
// published as worth minus a few kopecks.
//
// A quantity and a price ARE now bounded where they are written
// (operation.maxQuantity, #84), and the fixtures below still reach these guards
// through inputs no write-time bound covers: a quote's price, an fx rate, a
// position built from several operations, and journals written before that bound
// existed. That is the whole reason the read side keeps its own guards.
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

// RatesOn panics rather than answering, as the equivalent fakes in account and
// marketdata do. Nothing in this file goes through the prefetch — every test
// here calls one function of the handler directly — and an empty
// marketdata.Rates would answer "not among the resolved queries" for every
// triple, which the code under test reads as a missing rate. A change that
// routed any of these paths through the prefetch would then leave the tests
// passing for a reason that has nothing to do with what they check.
func (c fixedRateConverter) RatesOn(context.Context, []marketdata.RateQuery) (marketdata.Rates, error) {
	panic("fixedRateConverter: RatesOn not used")
}

// The quantity here is 10^15 — exactly the largest a write accepts (see
// operation.maxQuantity) — and the price an unremarkable 100. Their product is
// still not an int64 of cents, which is the point: the write-side bound moves
// the wrapping figure out of easy reach, it does not make it impossible.
func TestMarketValueRefusesAShareValuationThatWouldWrap(t *testing.T) {
	q := marketdata.Quote{Price: dec("100"), Currency: "USD"}
	minor, currency, ok, err := marketValue(instrument.TypeShare, nil, nil, dec("1e15"), q)
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("marketValue(share) = (%d, %q, %v), err = %v; want ErrOverflow: 1e15 shares at 100 is not an int64 of cents", minor, currency, ok, err)
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

// The three tests below stand at the CALL SITES rather than at the guards.
// Each guard above is only as good as the `if err != nil` that reads it, and
// that `if` is code like any other: neutered, every one of them let the request
// finish successfully and published the position anyway. What reached the
// screen was not even an obviously broken number — it was a null market value,
// or a converted valuation of zero, which is precisely the shape this screen
// uses for "no quote yet" and "no rate yet". An overflow wearing that shape is
// the failure the whole branch exists to remove, so the branch owes a test that
// reddens when the check disappears.

// TestToAPIRefusesToPublishAPositionWhoseValuationCannotBeStruck: marketValue
// refuses, and toAPI must fail with it. Swallowing the refusal drops toAPI into
// the `!ok` branch a paragraph below, which returns the position with
// market_value_minor left null — an overflow published as "this instrument has
// no quote".
//
// The position's currency and the quote's agree, so no rate is resolved and the
// handler needs no converter at all.
func TestToAPIRefusesToPublishAPositionWhoseValuationCannotBeStruck(t *testing.T) {
	id := uuid.New()
	p := &Position{InstrumentID: id, Currency: "USD", Quantity: dec("1e15")}
	inst := instrument.Instrument{ID: id, Type: instrument.TypeShare, Currency: "USD"}
	quotes := map[uuid.UUID]marketdata.Quote{id: {InstrumentID: id, Price: dec("100"), Currency: "USD"}}

	out, err := (&Handler{}).toAPI(context.Background(), p, inst, quotes, time.Now(), map[rateKey]*rateLookup{})
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("toAPI = %+v, err = %v; want ErrOverflow: 1e15 shares at 100 is not an int64 of cents, and a position published here would show a null market value instead", out, err)
	}
	if out.Quantity != "" {
		t.Errorf("toAPI returned a position (%+v) alongside the refusal, want the zero value", out)
	}
}

// TestToAPIRefusesAValuationThatCannotBeConvertedToThePositionCurrency is the
// same call site one branch further in, where the valuation is struck but the
// conversion into the position's own currency is not. The quote is in USD and
// the position trades in RUB, so the rate is actually applied; the valuation
// lands exactly on maxint64 kopecks and only doubling it leaves the range.
//
// Swallowing this refusal publishes market_value_minor = 0 RUB beside a
// market_value_source_minor of maxint64 USD — a holding worth nothing at all,
// on a row that says in the next column what it is worth.
func TestToAPIRefusesAValuationThatCannotBeConvertedToThePositionCurrency(t *testing.T) {
	id := uuid.New()
	p := &Position{InstrumentID: id, Currency: "RUB", Quantity: dec("1")}
	inst := instrument.Instrument{ID: id, Type: instrument.TypeShare, Currency: "USD"}
	// price * quantity * 100 == math.MaxInt64, exactly — see
	// TestMarketValuePublishesTheLargestValuationThatFits.
	quotes := map[uuid.UUID]marketdata.Quote{id: {InstrumentID: id, Price: dec("92233720368547758.07"), Currency: "USD"}}
	h := &Handler{conv: fixedRateConverter{rate: decimal.NewFromInt(2)}}

	out, err := h.toAPI(context.Background(), p, inst, quotes, time.Now(), map[rateKey]*rateLookup{})
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("toAPI = %+v, err = %v; want ErrOverflow: twice maxint64 is not an int64, and a position published here would show a market value of zero", out, err)
	}
	if out.Quantity != "" {
		t.Errorf("toAPI returned a position (%+v) alongside the refusal, want the zero value", out)
	}
}

// TestPositionInBaseRefusesAValuationThatCannotBeStruckInTheBaseCurrency is the
// third call site: the valuation is fine in the position's own currency and
// does not fit once carried into the base one. The basis converts perfectly
// well at the same rate, which is the point — swallowing the refusal publishes
// in_base with market_value_minor = 0 and an unrealized loss of exactly the
// basis, a figure that reads as a holding wiped out rather than as a broken
// one.
func TestPositionInBaseRefusesAValuationThatCannotBeStruckInTheBaseCurrency(t *testing.T) {
	acquired := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	p := &Position{
		Currency:  "USD",
		Quantity:  dec("1"),
		CostMinor: 100,
		Lots:      []Lot{{Quantity: dec("1"), CostMinor: 100, AcquiredOn: &acquired}},
	}
	apiPos := apitypes.Position{
		MarketValueMinor:    nullable.NewNullableWithValue(int64(math.MaxInt64)),
		MarketValueCurrency: nullable.NewNullableWithValue("USD"),
	}
	h := &Handler{conv: fixedRateConverter{rate: decimal.NewFromInt(2)}}

	out, gap, err := h.positionInBase(context.Background(), p, apiPos, nil, "RUB",
		nullable.NewNullNullable[int64](), time.Now(), map[rateKey]*rateLookup{})
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("positionInBase = %+v, err = %v; want ErrOverflow: twice maxint64 is not an int64 of kopecks", out, err)
	}
	if out != nil {
		t.Errorf("positionInBase returned %+v alongside the refusal, want nil", out)
	}
	// An overflow is not a gap the screen may caption. A gap says "this figure
	// is missing and here is why a reader should expect it later"; this one is
	// broken input, the request dies with it, and naming a cause here would be
	// the one thing this branch spent itself removing — a caption for a state
	// nobody is in.
	if gap != inBaseStruck {
		t.Errorf("positionInBase named gap %v beside the refusal, want none: an overflow fails the request, it is not a gap to caption", gap)
	}
}
