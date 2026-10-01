package portfolio

import (
	"testing"
	"time"

	"github.com/oapi-codegen/nullable"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/platform/apitypes"
)

// The account total's two MARKS are tested here rather than through HTTP for
// one reason: the shape that raises the first of them cannot be created through
// the manual door at all. A holding that does not know what it cost arrives by
// a transfer the broker sent with no price, and the operations endpoint refuses
// a bare transfer_in outright ("use the transfer endpoint") while the transfer
// endpoint derives the basis from the source account's own lots. Only the
// importer writes such a row, through operation.ApplyImportDelta.
//
// So the marks are exercised where the decision is taken. What the numbers mean
// end to end is pinned in http_account_total_test.go, over real journals.

func minor(v int64) nullable.Nullable[int64] { return nullable.NewNullableWithValue(v) }

func noFigure() nullable.Nullable[int64] { return nullable.NewNullNullable[int64]() }

// pos builds the fields of one published row that the account total reads. The
// gap is the row's market_value_gap: non-null on exactly the row nothing prices.
func pos(currency, quantity string, cost int64, total, settled nullable.Nullable[int64], unvalued bool) apitypes.Position {
	p := apitypes.Position{
		Currency:     currency,
		Quantity:     quantity,
		CostMinor:    cost,
		TotalMinor:   total,
		SettledMinor: settled,
	}
	if unvalued {
		p.MarketValueGap = nullable.NewNullableWithValue(apitypes.NoQuote)
	} else {
		p.MarketValueGap = nullable.NewNullNullable[apitypes.MarketValueGap]()
	}
	return p
}

// TestAccountTotalCountsAHoldingThatDoesNotKnowWhatItCost: shares counted as
// bought for nothing (Position.has_unknown_cost — which basis that is, is
// decided by hasUnknownCost and tested there). Their whole market value counts
// as profit, so the total is HIGHER than the truth by whatever was really paid —
// and the count is the only thing that says so.
func TestAccountTotalCountsAHoldingThatDoesNotKnowWhatItCost(t *testing.T) {
	at := newAccountTotals("RUB")
	row := pos("RUB", "10", 0, minor(120_000), minor(0), false)
	row.HasUnknownCost = true
	if err := at.addPosition(row, nil, inBaseSameCurrency, gapNone); err != nil {
		t.Fatalf("addPosition: %v", err)
	}
	got := at.result()

	if got.UnknownCostPositions != 1 {
		t.Errorf("unknown_cost_positions = %d, want 1", got.UnknownCostPositions)
	}
	if got.ZeroValuedPositions != 0 {
		t.Errorf("zero_valued_positions = %d, want 0 — this paper IS priced. The two marks pull the total in opposite directions and must never be confused", got.ZeroValuedPositions)
	}
	if figure := got.ByCurrency[0].AmountMinor; figure.IsNull() || figure.MustGet() != 120_000 {
		t.Errorf("total = %v, want 120000: the mark warns about the figure, it does not change it", figure)
	}
}

// TestAccountTotalDoesNotCallASoldOutPositionCostless is the boundary the count
// above must not cross. A position sold out of also carries a basis of nought —
// there is nothing left to hold — and it is not a paper whose price nobody
// recorded. Counting it would put a warning on every account that ever closed a
// deal, which is the fastest way to make a real warning invisible.
func TestAccountTotalDoesNotCallASoldOutPositionCostless(t *testing.T) {
	at := newAccountTotals("RUB")
	if err := at.addPosition(pos("RUB", "0", 0, minor(49_500), minor(49_500), false), nil, inBaseSameCurrency, gapNone); err != nil {
		t.Fatalf("addPosition: %v", err)
	}
	if got := at.result(); got.UnknownCostPositions != 0 {
		t.Errorf("unknown_cost_positions = %d, want 0: nothing is held, so nothing has an unknown price", got.UnknownCostPositions)
	}
}

// TestAccountTotalSeparatesTheTwoReasonsABaseFigureIsMissing is the pair the
// account's total must not confuse. A row with no settled result in the base
// currency is stopped either by a disposal whose parcels have no acquisition
// day — which no job will ever supply, so the paper is left out and counted —
// or by a missing RATE, which the backfill supplies on its own. Leaving a paper
// out for the second reason would publish a figure that silently changes the
// day the rate lands.
func TestAccountTotalSeparatesTheTwoReasonsABaseFigureIsMissing(t *testing.T) {
	// A row with a valuation (no gap) and no settled result: rowTotal cannot
	// answer, and what happens next is decided by the realized gap alone.
	row := pos("RUB", "10", 50_000, noFigure(), noFigure(), false)

	t.Run("a day nobody recorded leaves the paper out", func(t *testing.T) {
		at := newAccountTotals("RUB")
		if err := at.addPosition(row, nil, inBaseSameCurrency, gapUndated); err != nil {
			t.Fatalf("addPosition: %v", err)
		}
		got := at.result()
		if got.UndatedPositions != 1 {
			t.Errorf("undated_positions = %d, want 1", got.UndatedPositions)
		}
		if got.InBase.IsNull() {
			t.Errorf("in_base is null (%v) — a date does not arrive later, so withholding the figure withholds it for ever", got.InBaseGap)
		}
	})

	t.Run("a missing rate withholds the figure", func(t *testing.T) {
		at := newAccountTotals("RUB")
		if err := at.addPosition(row, nil, inBaseSameCurrency, gapNoRate); err != nil {
			t.Fatalf("addPosition: %v", err)
		}
		got := at.result()
		if !got.InBase.IsNull() {
			t.Errorf("in_base = %d, want null: this figure appears when the backfill catches up, and publishing a total without the paper now would change it silently then", got.InBase.MustGet())
		}
		if got.UndatedPositions != 0 {
			t.Errorf("undated_positions = %d, want 0 — nothing here is about a date", got.UndatedPositions)
		}
	})
}

// TestAccountTotalRowContribution is the whole rule of what one row adds, case
// by case, as a table — because the four answers differ in kind and three of
// them are easy to reach by accident.
func TestAccountTotalRowContribution(t *testing.T) {
	cases := []struct {
		name       string
		p          apitypes.Position
		wantMinor  int64
		wantOK     bool
		wantAtZero bool
	}{
		{
			name:      "a row with a total contributes it",
			p:         pos("RUB", "10", 100_000, minor(40_000), minor(30_000), false),
			wantMinor: 40_000, wantOK: true,
		},
		{
			// The owner's decision, and the case this whole mark exists for.
			name:      "a row nothing prices contributes its settled result less its basis",
			p:         pos("RUB", "10", 50_000, noFigure(), minor(0), true),
			wantMinor: -50_000, wantOK: true, wantAtZero: true,
		},
		{
			// A valuation exists and simply cannot be compared with this row's
			// own currency — a bond whose face value is in another. Writing it
			// off would be a lie about a paper that has a perfectly good price.
			name: "a row with a valuation it cannot compare has no contribution",
			p:    pos("RUB", "10", 50_000, noFigure(), minor(0), false),
		},
		{
			// Nothing settled in this currency: a disposal or a payment came in
			// a third one, so the term does not exist rather than being nought.
			name: "a row with no settled result has no contribution",
			p:    pos("RUB", "10", 50_000, noFigure(), noFigure(), true),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			at := newAccountTotals("RUB")
			if err := at.addPosition(c.p, nil, inBaseSameCurrency, gapNone); err != nil {
				t.Fatalf("addPosition: %v", err)
			}
			got := at.result()
			figure := got.ByCurrency[0].AmountMinor
			if !c.wantOK {
				if !figure.IsNull() {
					t.Fatalf("amount_minor = %d, want null: the term genuinely does not exist, and a bucket short a term reads as a result rather than as a gap", figure.MustGet())
				}
				if got.InBaseGap == nil || got.InBaseGap.IsNull() {
					t.Errorf("in_base_gap = null, want a named gap: the one figure cannot be struck either")
				}
				return
			}
			if figure.IsNull() {
				t.Fatalf("amount_minor is null, want %d", c.wantMinor)
			}
			if figure.MustGet() != c.wantMinor {
				t.Errorf("amount_minor = %d, want %d", figure.MustGet(), c.wantMinor)
			}
			if (got.ZeroValuedPositions == 1) != c.wantAtZero {
				t.Errorf("zero_valued_positions = %d, want %v", got.ZeroValuedPositions, c.wantAtZero)
			}
		})
	}
}

// hasUnknownCost reads the lots, not the position's total basis: a paper is
// "bought for nothing" when some of its basis arrived with no price, held or
// already sold — and not merely because nothing is left to hold.
func TestWhichBasisCountsAsBoughtForNothing(t *testing.T) {
	day := func(s string) *time.Time {
		d, err := time.Parse(time.DateOnly, s)
		if err != nil {
			t.Fatal(err)
		}
		return &d
	}
	q := decimal.RequireFromString
	for name, tc := range map[string]struct {
		p           Position
		any, inSale bool
	}{
		"held with no price": {
			p:   Position{Lots: []Lot{{Quantity: q("10"), CostMinor: 0}}},
			any: true,
		},
		"held, one lot priced and one not": {
			p:   Position{Lots: []Lot{{Quantity: q("5"), CostMinor: 50_000, AcquiredOn: day("2024-03-01")}, {Quantity: q("5"), CostMinor: 0}}},
			any: true,
		},
		"sold with no price": {
			p:   Position{Realizations: []Realization{{Released: []ReleasedLot{{Quantity: q("10"), CostMinor: 0}}}}},
			any: true, inSale: true,
		},
		"sold out of ordinary purchases": {
			p: Position{Realizations: []Realization{{Released: []ReleasedLot{{Quantity: q("10"), CostMinor: 49_500, AcquiredOn: day("2024-03-01")}}}}},
		},
		"a shareless parcel of money is not shares bought for nothing": {
			p: Position{Lots: []Lot{{Quantity: decimal.Zero, CostMinor: 0}}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := hasUnknownCost(&tc.p); got != tc.any {
				t.Errorf("hasUnknownCost = %v, want %v", got, tc.any)
			}
			if got := soldUnknownCost(&tc.p); got != tc.inSale {
				t.Errorf("soldUnknownCost = %v, want %v", got, tc.inSale)
			}
		})
	}
}

// The count follows the paper's own flag, not its total basis: a paper with one
// priced lot and one unpriced is counted, and so is one sold out of shares that
// had no price — the old test "held, and a basis of nought" missed both.
func TestAccountTotalCountsEveryPaperWithUnpricedBasis(t *testing.T) {
	at := newAccountTotals("RUB")
	partly := pos("RUB", "10", 50_000, minor(120_000), minor(0), false)
	partly.HasUnknownCost = true
	soldOut := pos("RUB", "0", 0, minor(49_500), minor(49_500), false)
	soldOut.HasUnknownCost = true
	for _, row := range []apitypes.Position{partly, soldOut} {
		if err := at.addPosition(row, nil, inBaseSameCurrency, gapNone); err != nil {
			t.Fatalf("addPosition: %v", err)
		}
	}
	if got := at.result(); got.UnknownCostPositions != 2 {
		t.Errorf("unknown_cost_positions = %d, want 2", got.UnknownCostPositions)
	}
}
