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

// The quantity here is 10^15 — a hundred times what a write accepts (see
// operation.maxQuantity, 10^13) — and the price an unremarkable 100. A position
// reaches this size without any single write being unusual: one accepted buy at
// a time, or a split multiplying the whole holding by a ratio, or a row written
// before the bound existed. Their product is not an int64 of cents, which is the
// point: the write-side bound moves the wrapping figure out of easy reach, it
// does not make it impossible.
func TestMarketValueRefusesAShareValuationThatWouldWrap(t *testing.T) {
	q := marketdata.Quote{Price: dec("100"), Currency: "USD"}
	minor, currency, gap, err := marketValue(instrument.TypeShare, nil, nil, dec("1e15"), q, true)
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("marketValue(share) = (%d, %q, %v), err = %v; want ErrOverflow: 1e15 shares at 100 is not an int64 of cents", minor, currency, gap, err)
	}
	// And no gap beside it. A refusal is a figure this server cannot state; the
	// gaps are figures it does not have. Naming one here would let the caller
	// caption a broken journal as absent market data — the very confusion the
	// error is kept separate from the gap to prevent.
	if _, named := apiMarketValueGap(gap); named {
		t.Error("marketValue named a gap alongside the refusal; the caller would publish it as an honest absence")
	}
}

// TestMarketValuePublishesTheLargestQuantityAWriteAccepts is the test above's
// necessary other half, and the one that keeps the two sides of the program from
// contradicting each other. The write side accepts a quantity of at most 10^13
// (operation.maxQuantity); this screen has to be able to VALUE what the write
// side lets in, or the bound over there is a refusal that changed nothing.
//
// It failed to be true once. The bound was first set at 10^15, and at 10^15 a
// quote of 92.24 — less than an ordinary share costs — already overflowed here:
// the two halves of the same branch stated their verdicts on 10^15 side by side,
// one accepting and one refusing, and neither mentioned the other. So this test
// names the number the write side promises and the largest whole-currency quote
// that survives it, and its twin over there
// (operation.TestQuantityExactlyAtTheBoundIsAccepted) asserts the same pair
// through money.Minor. Whichever bound moves, one of the two reddens.
func TestMarketValuePublishesTheLargestQuantityAWriteAccepts(t *testing.T) {
	q := marketdata.Quote{Price: dec("9223"), Currency: "USD"}
	minor, _, gap, err := marketValue(instrument.TypeShare, nil, nil, dec("1e13"), q, true)
	if err != nil || gap != valuationStruck {
		t.Fatalf("marketValue(1e13 at 9223) = (%d, %v), err = %v; want it published: a quantity the write accepts must be valuable at an ordinary quote",
			minor, gap, err)
	}
	if minor != 9_223_000_000_000_000_000 {
		t.Errorf("marketValue = %d, want 9223000000000000000 cents", minor)
	}

	// One unit of money more per share does not fit. The margin is that narrow
	// by construction — 10^13 is the largest quantity for which an ordinary
	// quote fits at all — so a bound loosened even slightly over there stops
	// being the bound this test is describing.
	q.Price = dec("9224")
	if _, _, gap, err := marketValue(instrument.TypeShare, nil, nil, dec("1e13"), q, true); !errors.Is(err, money.ErrOverflow) || gap != valuationStruck {
		t.Errorf("marketValue(1e13 at 9224): gap = %v, err = %v; want ErrOverflow and no gap", gap, err)
	}
}

// TestMarketValueRefusesABondValuationThatWouldWrap exercises the other
// branch, which multiplies by a face value instead of a price and therefore
// leaves int64 on entirely different inputs.
func TestMarketValueRefusesABondValuationThatWouldWrap(t *testing.T) {
	face := int64(1_000_000_000_000_000)
	faceCurrency := "RUB"
	q := marketdata.Quote{Price: dec("100"), Currency: "RUB"} // 100% of face
	minor, currency, gap, err := marketValue(instrument.TypeBond, &face, &faceCurrency, dec("1e5"), q, true)
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("marketValue(bond) = (%d, %q, %v), err = %v; want ErrOverflow", minor, currency, gap, err)
	}
	if _, named := apiMarketValueGap(gap); named {
		t.Error("marketValue named a gap alongside the refusal")
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
	minor, currency, gap, err := marketValue(instrument.TypeShare, nil, nil, dec("1"), q, true)
	if err != nil || gap != valuationStruck {
		t.Fatalf("marketValue at exactly maxint64 = (%d, %q, %v), err = %v; want it published", minor, currency, gap, err)
	}
	if minor != math.MaxInt64 {
		t.Errorf("marketValue = %d, want %d", minor, int64(math.MaxInt64))
	}

	q.Price = dec("92233720368547758.08") // one kopeck further
	if _, _, gap, err := marketValue(instrument.TypeShare, nil, nil, dec("1"), q, true); !errors.Is(err, money.ErrOverflow) || gap != valuationStruck {
		t.Errorf("marketValue one kopeck past maxint64: gap = %v, err = %v; want ErrOverflow and no gap", gap, err)
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

// The four tests below are about SUMS rather than products (#83). Plan 10 put a
// guard on every decimal that becomes money, and left the arithmetic done on the
// results: an account's realized totals are accumulated with +=, and an
// unrealized figure is a subtraction. Each term is an int64 already — it had to
// be, to get this far — and int64 addition in Go wraps exactly as silently as
// decimal.IntPart() does. The guard belongs on the PUBLISHED figure, and the
// published figure is the sum. It is the argument this package already applied
// to sumInBase and to marketdata.ConvertMany, reaching the last places it had
// not been applied to.

// TestRealizedTotalsRefusesANativeTotalThatWouldWrap: two positions in one
// currency, each realized result an ordinary int64, and only their sum outside
// the range. The by-currency total is published as it stands — nothing converts
// it and nothing rounds it — so a wrapped sum would be a plausible-looking
// figure of the wrong sign under a header that says «итого».
func TestRealizedTotalsRefusesANativeTotalThatWouldWrap(t *testing.T) {
	rt := newRealizedTotals("RUB")
	none := nullable.NewNullNullable[int64]()
	if err := rt.add("USD", math.MaxInt64, none, gapNoRate); err != nil {
		t.Fatalf("first position: %v — maxint64 is a figure, and one of them fits", err)
	}
	err := rt.add("USD", 1, none, gapNoRate)
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("second position: err = %v, want ErrOverflow — maxint64 + 1 is not a total", err)
	}
	if rt.byCurrency["USD"] != math.MaxInt64 {
		t.Errorf("USD total = %d after the refusal, want maxint64 left as it was: a refused term must not be half-added", rt.byCurrency["USD"])
	}
}

// TestRealizedTotalsRefusesABaseTotalThatWouldWrap is the same for the other
// accumulator. The two positions are in different currencies so that only the
// base-currency total is in question here, and each term is a figure realizedInBase
// already converted, rounded and found publishable on its own.
func TestRealizedTotalsRefusesABaseTotalThatWouldWrap(t *testing.T) {
	rt := newRealizedTotals("RUB")
	big := nullable.NewNullableWithValue(int64(math.MaxInt64))
	if err := rt.add("USD", 0, big, gapNone); err != nil {
		t.Fatalf("first position: %v", err)
	}
	err := rt.add("EUR", 0, big, gapNone)
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("second position: err = %v, want ErrOverflow", err)
	}
	if rt.inBaseMinor != math.MaxInt64 {
		t.Errorf("base total = %d after the refusal, want maxint64 left as it was", rt.inBaseMinor)
	}
}

// TestRealizedTotalsOverflowIsNotAGap keeps this refusal out of the pile it must
// never join, exactly as TestSumInBaseOverflowIsNotAMissingRate does one screen
// element up. `undated` and `no_rate` are the account total's two ways of saying
// "a term could not be valued": one closes when the fx backfill catches up, the
// other never closes but is at least true about the data. An overflow filed
// under either would put a caption about missing data over a figure whose terms
// are all present and one of which is broken.
func TestRealizedTotalsOverflowIsNotAGap(t *testing.T) {
	rt := newRealizedTotals("RUB")
	big := nullable.NewNullableWithValue(int64(math.MaxInt64))
	if err := rt.add("USD", 0, big, gapNone); err != nil {
		t.Fatalf("first position: %v", err)
	}
	if err := rt.add("EUR", 0, big, gapNone); err == nil {
		t.Fatal("the overflowing total was accepted; there is nothing to publish and nothing was said")
	}
	if rt.undated || rt.noRate {
		t.Errorf("overflow recorded as a gap (undated=%v, no_rate=%v); the account header would then explain a broken total as data that has yet to arrive",
			rt.undated, rt.noRate)
	}
}

// TestToAPIRefusesAnUnrealizedFigureThatWouldWrap covers the subtraction. Both
// operands are int64 and in the same currency — the guard above this line has
// just made sure of that — and a difference still leaves the range when they
// have opposite signs.
//
// A NEGATIVE market value is what makes it reachable, and this test reaches it
// the cheapest way available: an Instrument built directly in memory with a
// negative face value, rather than through the HTTP write path. checkFacePair
// (internal/instrument/http.go) now refuses a face value that is not positive
// (#93), so this exact construction can no longer occur through a POST or a
// PATCH. The guard below stays regardless — a broken face value was never the
// only door to a negative basis: Position.CostMinor and RealizedPnLMinor are
// both accumulated with a bare += in the engine (addLot at engine.go:702,
// realize at engine.go:212), each term individually valid and none of them
// carrying a broken field, so a long enough run of ordinary buys (about 4612
// at the write side's own cap) wraps the running total negative with nothing
// to catch it before it reaches this subtraction. This test exercises that
// same guarded subtraction through the shorter path — a face value set
// directly — rather than assembling four thousand operations to reach it.
// Swallowing this refusal publishes an unrealized profit of +8446744073709551616
// kopecks, which is what -10^19 wraps to: a broken row reading as an enormous
// gain, beside a market value that says the opposite.
func TestToAPIRefusesAnUnrealizedFigureThatWouldWrap(t *testing.T) {
	id := uuid.New()
	face := int64(-9_000_000_000_000_000_000)
	faceCurrency := "RUB"
	p := &Position{InstrumentID: id, Currency: "RUB", Quantity: dec("1"), CostMinor: 1_000_000_000_000_000_000}
	inst := instrument.Instrument{
		ID: id, Type: instrument.TypeBond, Currency: "RUB",
		FaceValueMinor: &face, FaceCurrency: &faceCurrency,
	}
	// 100% of face, so the valuation is the face value itself: -9e18, an int64.
	quotes := map[uuid.UUID]marketdata.Quote{id: {InstrumentID: id, Price: dec("100"), Currency: "RUB"}}

	out, err := (&Handler{}).toAPI(context.Background(), p, inst, quotes, time.Now(), map[rateKey]*rateLookup{})
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("toAPI = %+v, err = %v; want ErrOverflow: -9e18 minus 1e18 is not an int64, and the wrapped answer is a small positive profit", out, err)
	}
	if out.Quantity != "" {
		t.Errorf("toAPI returned a position (%+v) alongside the refusal, want the zero value", out)
	}
}

// TestPositionInBaseRefusesAnUnrealizedFigureThatWouldWrap is the same
// subtraction in the base currency, where both operands have been converted
// first. The basis converts perfectly well and the valuation fits after its own
// conversion; only the difference does not.
func TestPositionInBaseRefusesAnUnrealizedFigureThatWouldWrap(t *testing.T) {
	acquired := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	p := &Position{
		Currency:  "USD",
		Quantity:  dec("1"),
		CostMinor: 1_000_000_000_000_000_000,
		Lots:      []Lot{{Quantity: dec("1"), CostMinor: 1_000_000_000_000_000_000, AcquiredOn: &acquired}},
	}
	apiPos := apitypes.Position{
		MarketValueMinor:    nullable.NewNullableWithValue(int64(-9_000_000_000_000_000_000)),
		MarketValueCurrency: nullable.NewNullableWithValue("USD"),
	}
	h := &Handler{conv: fixedRateConverter{rate: decimal.NewFromInt(1)}}

	out, gap, err := h.positionInBase(context.Background(), p, apiPos, nil, "RUB",
		nullable.NewNullNullable[int64](), time.Now(), map[rateKey]*rateLookup{})
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("positionInBase = %+v, err = %v; want ErrOverflow: -9e18 minus 1e18 is not an int64 of kopecks", out, err)
	}
	if out != nil {
		t.Errorf("positionInBase returned %+v alongside the refusal, want nil", out)
	}
	if gap != inBaseStruck {
		t.Errorf("positionInBase named gap %v beside the refusal, want none: an overflow fails the request, it is not a gap to caption", gap)
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

// TestMarketValueNeedsBothHalvesOfTheFacePair pins the read-side guard that a
// bond is valued from BOTH its face value and the currency that value is in, or
// not at all. A bare number with no currency on it would be worse than no
// valuation: the positions screen would print a figure nobody could say the unit
// of.
//
// It used to be an end-to-end test (portfolio.TestPositions...), because
// PATCHing face_currency to null was how the state was reached. #93 closed that
// door at the write, and migration 0012 makes the state unstorable, so the
// arguments are handed to marketValue directly now — its contract is about what
// it is CALLED with, not about what the catalog happens to hold, and the guard
// stays for the same reason every read-side guard here does.
func TestMarketValueNeedsBothHalvesOfTheFacePair(t *testing.T) {
	face := int64(100_000)
	faceCurrency := "RUB"
	q := marketdata.Quote{Price: dec("95.20"), Currency: "RUB"} // 95.20% of face

	for _, tc := range []struct {
		name     string
		face     *int64
		currency *string
	}{
		{"a face value with no currency to state it in", &face, nil},
		{"a face currency with no value under it", nil, &faceCurrency},
		{"neither half recorded", nil, nil},
	} {
		minor, currency, gap, err := marketValue(instrument.TypeBond, tc.face, tc.currency, dec("100"), q, true)
		if err != nil {
			t.Errorf("marketValue with %s: err = %v, want a plain refusal to value", tc.name, err)
		}
		if gap != valuationNoFaceValue || minor != 0 || currency != "" {
			t.Errorf("marketValue with %s = (%d, %q, %v), want no valuation at all and the gap naming the face value", tc.name, minor, currency, gap)
		}
	}

	// And with both halves it values as usual, so the guard above is about the
	// missing half and not about bonds.
	minor, currency, gap, err := marketValue(instrument.TypeBond, &face, &faceCurrency, dec("100"), q, true)
	if err != nil || gap != valuationStruck {
		t.Fatalf("marketValue of a whole pair = (%d, %q, %v), err = %v; want it valued", minor, currency, gap, err)
	}
	// 100 bonds at 95.20% of a 1 000,00 ₽ face.
	if minor != 9_520_000 || currency != "RUB" {
		t.Errorf("marketValue = (%d, %q), want (9520000, \"RUB\")", minor, currency)
	}
}
