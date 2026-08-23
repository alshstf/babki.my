package portfolio

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// TestIncomeByInstrumentMatchesEngineIncomeTypes guards against
// incomeByInstrument's hardcoded type switch drifting from Compute's own,
// separate switch over the same Type enum (engine.go's TypeDividend/
// TypeCoupon/TypeTax cases, which are the only ones that touch
// Position.IncomeByCurrency). The two switches live in different files and
// nothing but this test ties them together — package portfolio's own test
// suite (http_test.go, http_position_in_base_test.go) never notices a
// mismatch because every fixture happens to use the same three types both
// switches already agree on.
//
// For every operation Type in the enum, it builds one operation (with
// whatever setup operations are needed for the engine to accept it, e.g. a
// prior buy before a sell) and runs it through Compute, then checks that the
// resulting position booked income if and only if
// incomeByInstrument groups that same operation under its instrument. If a
// reviewer ever widens engine.go's income-affecting switch (adds a case, or
// folds another type into an existing one) without widening
// incomeByInstrument's switch to match, Compute starts booking income for
// that type while incomeByInstrument silently keeps excluding it — the exact
// drift the doc comment on incomeByInstrument warns about, and the exact
// thing this test is built to turn red for. See that doc comment.
//
// Deposit, withdrawal, interest and conversion are exercised as cash-level
// operations (no InstrumentID) rather than attributed to the test
// instrument: engine.go's instrument-scoped switch has no case for any of
// them today, so attaching an instrument would make Compute itself fail with
// ErrBadOperation, which is a pre-existing engine limitation unrelated to
// this test's purpose. Left cash-level, they never reach an instrument's
// income at all (Compute skips straight past them, and
// incomeByInstrument skips any operation with a nil InstrumentID), so both
// sides trivially agree — the four types are included here only so every
// Type constant is accounted for.
func TestIncomeByInstrumentMatchesEngineIncomeTypes(t *testing.T) {
	on := func(daysAfterEpoch int) time.Time {
		return time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, daysAfterEpoch)
	}
	qty := func(s string) *decimal.Decimal {
		v := decimal.RequireFromString(s)
		return &v
	}

	tests := []struct {
		typ Type
		// build returns the operations to run through Compute for this
		// case, all sharing instID as their InstrumentID except where the
		// type is cash-level only (see the function doc comment).
		build func(instID uuid.UUID) []Operation
	}{
		{TypeBuy, func(id uuid.UUID) []Operation {
			return []Operation{
				{Type: TypeBuy, InstrumentID: &id, OccurredOn: on(1), Currency: "USD", Quantity: qty("10"), AmountMinor: -100_000},
			}
		}},
		{TypeSell, func(id uuid.UUID) []Operation {
			return []Operation{
				{Type: TypeBuy, InstrumentID: &id, OccurredOn: on(1), Currency: "USD", Quantity: qty("10"), AmountMinor: -100_000},
				{Type: TypeSell, InstrumentID: &id, OccurredOn: on(2), Currency: "USD", Quantity: qty("10"), AmountMinor: 150_000},
			}
		}},
		{TypeRedemption, func(id uuid.UUID) []Operation {
			return []Operation{
				{Type: TypeBuy, InstrumentID: &id, OccurredOn: on(1), Currency: "USD", Quantity: qty("10"), AmountMinor: -100_000},
				{Type: TypeRedemption, InstrumentID: &id, OccurredOn: on(2), Currency: "USD", Quantity: qty("10"), AmountMinor: 100_000},
			}
		}},
		{TypeDeposit, func(id uuid.UUID) []Operation {
			return []Operation{
				{Type: TypeDeposit, OccurredOn: on(1), Currency: "USD", AmountMinor: 100_000},
			}
		}},
		{TypeWithdrawal, func(id uuid.UUID) []Operation {
			return []Operation{
				{Type: TypeWithdrawal, OccurredOn: on(1), Currency: "USD", AmountMinor: -50_000},
			}
		}},
		{TypeDividend, func(id uuid.UUID) []Operation {
			return []Operation{
				{Type: TypeDividend, InstrumentID: &id, OccurredOn: on(1), Currency: "USD", AmountMinor: 5_000},
			}
		}},
		{TypeCoupon, func(id uuid.UUID) []Operation {
			return []Operation{
				{Type: TypeCoupon, InstrumentID: &id, OccurredOn: on(1), Currency: "USD", AmountMinor: 3_000},
			}
		}},
		{TypeAmortization, func(id uuid.UUID) []Operation {
			return []Operation{
				{Type: TypeBuy, InstrumentID: &id, OccurredOn: on(1), Currency: "USD", Quantity: qty("10"), AmountMinor: -100_000},
				{Type: TypeAmortization, InstrumentID: &id, OccurredOn: on(2), Currency: "USD", AmountMinor: 20_000},
			}
		}},
		{TypeFee, func(id uuid.UUID) []Operation {
			return []Operation{
				{Type: TypeFee, InstrumentID: &id, OccurredOn: on(1), Currency: "USD", AmountMinor: -1_000},
			}
		}},
		{TypeTax, func(id uuid.UUID) []Operation {
			return []Operation{
				{Type: TypeTax, InstrumentID: &id, OccurredOn: on(1), Currency: "USD", AmountMinor: -2_000},
			}
		}},
		{TypeTransferIn, func(id uuid.UUID) []Operation {
			return []Operation{
				{Type: TypeTransferIn, InstrumentID: &id, OccurredOn: on(1), Currency: "USD", Quantity: qty("5"), AmountMinor: 25_000},
			}
		}},
		{TypeTransferOut, func(id uuid.UUID) []Operation {
			return []Operation{
				{Type: TypeBuy, InstrumentID: &id, OccurredOn: on(1), Currency: "USD", Quantity: qty("10"), AmountMinor: -100_000},
				{Type: TypeTransferOut, InstrumentID: &id, OccurredOn: on(2), Currency: "USD", Quantity: qty("5"), AmountMinor: 0},
			}
		}},
		// A conversion's two legs, built the way the service builds them: the
		// departing one gives up the recorded lots and the arriving one rebuilds
		// them under the new paper. Neither is income, and the point of having
		// them here is that incomeByInstrument must agree.
		{TypeExchangeOut, func(id uuid.UUID) []Operation {
			acquired := on(1)
			return []Operation{
				{Type: TypeBuy, InstrumentID: &id, OccurredOn: on(1), Currency: "USD", Quantity: qty("10"), AmountMinor: -100_000},
				{
					Type: TypeExchangeOut, InstrumentID: &id, OccurredOn: on(2), Currency: "USD", Quantity: qty("10"), AmountMinor: 100_000,
					TransferLots: []ReleasedLot{{Quantity: *qty("10"), CostMinor: 100_000, AcquiredOn: &acquired}},
				},
			}
		}},
		{TypeExchangeIn, func(id uuid.UUID) []Operation {
			acquired := on(1)
			return []Operation{
				{
					Type: TypeExchangeIn, InstrumentID: &id, OccurredOn: on(2), Currency: "USD", Quantity: qty("20"), AmountMinor: 100_000,
					TransferLots: []ReleasedLot{{Quantity: *qty("20"), CostMinor: 100_000, AcquiredOn: &acquired}},
				},
			}
		}},
		{TypeSplit, func(id uuid.UUID) []Operation {
			ratio := decimal.RequireFromString("2")
			return []Operation{
				{Type: TypeBuy, InstrumentID: &id, OccurredOn: on(1), Currency: "USD", Quantity: qty("10"), AmountMinor: -100_000},
				{Type: TypeSplit, InstrumentID: &id, OccurredOn: on(2), Currency: "USD", SplitRatio: &ratio},
			}
		}},
		{TypeInterest, func(id uuid.UUID) []Operation {
			return []Operation{
				{Type: TypeInterest, OccurredOn: on(1), Currency: "USD", AmountMinor: 1_000},
			}
		}},
		{TypeConversion, func(id uuid.UUID) []Operation {
			return []Operation{
				{Type: TypeConversion, OccurredOn: on(1), Currency: "USD", AmountMinor: 1_000},
			}
		}},
	}

	if len(tests) != len(validTypes) {
		t.Fatalf("test table covers %d types, but the engine's Type enum has %d — add the missing one(s)", len(tests), len(validTypes))
	}

	for _, tc := range tests {
		t.Run(string(tc.typ), func(t *testing.T) {
			id := uuid.New()
			ops := tc.build(id)

			positions, err := Compute(ops)
			if err != nil {
				t.Fatalf("Compute: %v", err)
			}
			gotIncome := false
			if p, ok := positions[id]; ok {
				// Whether income was BOOKED, not whether it came to a nonzero
				// total: income is kept per currency, and an entry exists as
				// soon as one payment lands in that currency — including one
				// that cancels against another. That is the sharper question
				// and it is the one this test wants, since a type folded into
				// income for zero would still be a type incomeByInstrument has
				// to know about.
				gotIncome = len(p.IncomeByCurrency) > 0
			}

			wantIncome := len(incomeByInstrument(ops)[id]) > 0

			if gotIncome != wantIncome {
				t.Errorf("%s: Compute booked income=%v, but incomeByInstrument groups it as income=%v — the two switches disagree for this type",
					tc.typ, gotIncome, wantIncome)
			}
		})
	}
}
