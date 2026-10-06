package portfolio

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// incomeByInstrument's type switch matches the engine's: for every type, an
// operation books income in Compute exactly when incomeByInstrument groups it.
// Deposit, withdrawal, interest and conversion run cash-level, where both
// sides trivially agree.
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
		// build returns the operations for the case.
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
		// A conversion's two legs, built as the service builds them.
		{TypeExchangeOut, func(id uuid.UUID) []Operation {
			acquired := on(1)
			buy := Operation{ID: uuid.New(), Type: TypeBuy, InstrumentID: &id, OccurredOn: on(1), Currency: "USD", Quantity: qty("10"), AmountMinor: -100_000}
			return []Operation{
				buy,
				{
					Type: TypeExchangeOut, InstrumentID: &id, OccurredOn: on(2), Currency: "USD", Quantity: qty("10"), AmountMinor: 100_000,
					TransferLots: []ReleasedLot{{From: buy.lotID(0), Quantity: *qty("10"), CostMinor: 100_000, AcquiredOn: &acquired}},
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
		// A spin-off's two legs.
		{TypeSpinoffOut, func(id uuid.UUID) []Operation {
			acquired := on(1)
			buy := Operation{ID: uuid.New(), Type: TypeBuy, InstrumentID: &id, OccurredOn: on(1), Currency: "USD", Quantity: qty("10"), AmountMinor: -100_000}
			return []Operation{
				buy,
				{
					Type: TypeSpinoffOut, InstrumentID: &id, OccurredOn: on(2), Currency: "USD", AmountMinor: 40_000,
					TransferLots: []ReleasedLot{{From: buy.lotID(0), Quantity: *qty("10"), CostMinor: 40_000, AcquiredOn: &acquired}},
				},
			}
		}},
		{TypeSpinoffIn, func(id uuid.UUID) []Operation {
			acquired := on(1)
			return []Operation{
				{
					Type: TypeSpinoffIn, InstrumentID: &id, OccurredOn: on(2), Currency: "USD", Quantity: qty("10"), AmountMinor: 40_000,
					TransferLots: []ReleasedLot{{Quantity: *qty("10"), CostMinor: 40_000, AcquiredOn: &acquired}},
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
				// Whether income was booked, not its total: a zero-sum entry still counts.
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
