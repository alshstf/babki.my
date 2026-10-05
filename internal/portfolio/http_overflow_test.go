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

// Overflows on the positions screen (#27). Write-time bounds (#84) do not cover
// quotes, rates, multi-operation positions or old journals, so the read side
// keeps its guards. Every refusal is an error, never one of the screen's nulls,
// which mean data that may yet arrive.

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// fixedRateConverter answers every lookup with one rate.
type fixedRateConverter struct{ rate decimal.Decimal }

func (c fixedRateConverter) Rate(context.Context, string, string, time.Time) (decimal.Decimal, time.Time, error) {
	return c.rate, time.Time{}, nil
}

// RatesOn panics: these tests call handler functions directly, and an empty
// Rates would read as a missing rate, passing for the wrong reason.
func (c fixedRateConverter) RatesOn(context.Context, []marketdata.RateQuery) (marketdata.Rates, error) {
	panic("fixedRateConverter: RatesOn not used")
}

// A quantity of 10^15 (a hundred times the write bound) at 100 overflows; a
// position can grow there through many writes, a split, or old rows.
func TestMarketValueRefusesAShareValuationThatWouldWrap(t *testing.T) {
	q := marketdata.Quote{Price: dec("100"), Currency: "USD"}
	minor, currency, gap, err := marketValue(instrument.TypeShare, nil, nil, dec("1e15"), q, true)
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("marketValue(share) = (%d, %q, %v), err = %v; want ErrOverflow: 1e15 shares at 100 is not an int64 of cents", minor, currency, gap, err)
	}
	// No gap beside the refusal: an overflow must not be captioned as missing
	// data.
	if _, named := apiMarketValueGap(gap); named {
		t.Error("marketValue named a gap alongside the refusal; the caller would publish it as an honest absence")
	}
}

// The largest quantity a write accepts (10^13) is valuable at an ordinary
// quote. Its twin, operation.TestQuantityExactlyAtTheBoundIsAccepted, holds the
// same pair, so moving either bound reddens one of them.
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

	// One unit more per share does not fit: 10^13 is the largest quantity at which
	// an ordinary quote fits.
	q.Price = dec("9224")
	if _, _, gap, err := marketValue(instrument.TypeShare, nil, nil, dec("1e13"), q, true); !errors.Is(err, money.ErrOverflow) || gap != valuationStruck {
		t.Errorf("marketValue(1e13 at 9224): gap = %v, err = %v; want ErrOverflow and no gap", gap, err)
	}
}

// The bond branch multiplies by face value and overflows on other inputs.
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

// A valuation exactly at MaxInt64 is published; one kopeck more is refused.
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

// Two terms that fit can sum past int64; the total is refused.
func TestSumInBaseRefusesATotalThatWouldWrap(t *testing.T) {
	h := &Handler{conv: fixedRateConverter{rate: decimal.NewFromInt(1)}}
	on := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	terms := []datedMinor{{minor: math.MaxInt64, from: "USD", on: on}, {minor: math.MaxInt64, from: "USD", on: on}}

	minor, ok, err := h.sumInBase(context.Background(), terms, "RUB", map[rateKey]*rateLookup{})
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("sumInBase = (%d, %v), err = %v; want ErrOverflow", minor, ok, err)
	}
	if ok || minor != 0 {
		t.Errorf("sumInBase = (%d, %v) alongside the refusal, want (0, false)", minor, ok)
	}
}

// An overflow is an error, not ok=false, which means a missing rate.
func TestSumInBaseOverflowIsNotAMissingRate(t *testing.T) {
	h := &Handler{conv: fixedRateConverter{rate: decimal.NewFromInt(1)}}
	on := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	terms := []datedMinor{{minor: math.MaxInt64, from: "USD", on: on}, {minor: math.MaxInt64, from: "USD", on: on}}

	if _, _, err := h.sumInBase(context.Background(), terms, "RUB", map[rateKey]*rateLookup{}); err == nil {
		t.Fatal("sumInBase answered an overflow with a nil error, which this handler reads as a missing rate and renders as a gap")
	}
}

// The tests below cover sums and differences of int64 figures (#83): Go's +
// wraps silently.

// Two realized results in one currency summing past int64 are refused.
func TestRealizedTotalsRefusesANativeTotalThatWouldWrap(t *testing.T) {
	rt := newRealizedTotals("RUB")
	none := nullable.NewNullNullable[int64]()
	if err := rt.add("USD", nullable.NewNullableWithValue[int64](math.MaxInt64), none, gapNoRate, false); err != nil {
		t.Fatalf("first position: %v — maxint64 is a figure, and one of them fits", err)
	}
	err := rt.add("USD", nullable.NewNullableWithValue[int64](1), none, gapNoRate, false)
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("second position: err = %v, want ErrOverflow — maxint64 + 1 is not a total", err)
	}
	if rt.byCurrency["USD"] != math.MaxInt64 {
		t.Errorf("USD total = %d after the refusal, want maxint64 left as it was: a refused term must not be half-added", rt.byCurrency["USD"])
	}
}

// The same for the base-currency total.
func TestRealizedTotalsRefusesABaseTotalThatWouldWrap(t *testing.T) {
	rt := newRealizedTotals("RUB")
	big := nullable.NewNullableWithValue(int64(math.MaxInt64))
	if err := rt.add("USD", nullable.NewNullableWithValue[int64](0), big, gapNone, false); err != nil {
		t.Fatalf("first position: %v", err)
	}
	err := rt.add("EUR", nullable.NewNullableWithValue[int64](0), big, gapNone, false)
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("second position: err = %v, want ErrOverflow", err)
	}
	if rt.inBaseMinor != math.MaxInt64 {
		t.Errorf("base total = %d after the refusal, want maxint64 left as it was", rt.inBaseMinor)
	}
}

// A realized total overflow is an error, not `undated` or `no_rate`.
func TestRealizedTotalsOverflowIsNotAGap(t *testing.T) {
	rt := newRealizedTotals("RUB")
	big := nullable.NewNullableWithValue(int64(math.MaxInt64))
	if err := rt.add("USD", nullable.NewNullableWithValue[int64](0), big, gapNone, false); err != nil {
		t.Fatalf("first position: %v", err)
	}
	if err := rt.add("EUR", nullable.NewNullableWithValue[int64](0), big, gapNone, false); err == nil {
		t.Fatal("the overflowing total was accepted; there is nothing to publish and nothing was said")
	}
	if rt.undatedPositions != 0 || rt.noRate {
		t.Errorf("overflow recorded as a gap (undated=%d, no_rate=%v); the account header would then explain a broken total as data that has yet to arrive",
			rt.undatedPositions, rt.noRate)
	}
}

// The unrealized subtraction can overflow when the operands' signs differ. A
// negative valuation is reached here through an in-memory face value (the write
// side now refuses one, #93); in practice the engine's plain additions to cost
// could also wrap negative over thousands of ordinary buys. Swallowed, −10^19
// would read as an enormous gain.
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

// The same subtraction in the base currency.
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

// The tests below check the call sites: a guard is only as good as the `if`
// that reads it, and a swallowed overflow surfaces as a null or a zero — the
// shapes this screen uses for "not yet".

// marketValue's refusal fails toAPI; swallowed, the position would read as
// "no quote".
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

// A struck valuation that overflows on conversion to the position's currency
// fails toAPI; swallowed, it would publish 0 beside a MaxInt64 source.
func TestToAPIRefusesAValuationThatCannotBeConvertedToThePositionCurrency(t *testing.T) {
	id := uuid.New()
	p := &Position{InstrumentID: id, Currency: "RUB", Quantity: dec("1")}
	inst := instrument.Instrument{ID: id, Type: instrument.TypeShare, Currency: "USD"}
	// price × quantity × 100 is exactly MaxInt64.
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

// A valuation that overflows only in the base currency fails positionInBase;
// swallowed, it would read as a holding wiped out.
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
	// An overflow names no gap.
	if gap != inBaseStruck {
		t.Errorf("positionInBase named gap %v beside the refusal, want none: an overflow fails the request, it is not a gap to caption", gap)
	}
}

// A bond is valued only with both its face value and its currency. The state
// can no longer be stored (#93, migration 0012), so marketValue is called
// directly.
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

	// With both halves it values as usual.
	minor, currency, gap, err := marketValue(instrument.TypeBond, &face, &faceCurrency, dec("100"), q, true)
	if err != nil || gap != valuationStruck {
		t.Fatalf("marketValue of a whole pair = (%d, %q, %v), err = %v; want it valued", minor, currency, gap, err)
	}
	// 100 bonds at 95.20% of a 1 000,00 ₽ face.
	if minor != 9_520_000 || currency != "RUB" {
		t.Errorf("marketValue = (%d, %q), want (9520000, \"RUB\")", minor, currency)
	}
}
