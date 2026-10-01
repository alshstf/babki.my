package operation_test

import (
	"testing"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/operation"
)

// TestAConversionKeepsTheDayOfAParcelItRoundsAway: three parcels become so few
// units of the new paper that the first no longer holds one. Its money used to
// move onto the next parcel's day; it now stays a parcel of its own.
func TestAConversionKeepsTheDayOfAParcelItRoundsAway(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	produced := newPaper(t, f, "NEW", "Новая бумага")
	for _, buy := range []struct {
		on, qty string
		amount  int64
	}{{"2021-01-10", "1", -10_000}, {"2022-01-10", "1", -20_000}, {"2023-01-10", "98", -980_000}} {
		if _, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
			AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
			OccurredOn: date(buy.on), Quantity: dec(buy.qty), AmountMinor: buy.amount, Currency: "RUB",
		}); err != nil {
			t.Fatalf("seed buy: %v", err)
		}
	}

	// 100 units become 0.000000005: half a ten-billionth each for the first
	// two, which is less than the journal can name.
	_, in, err := svc.CreateExchange(f.ctx, f.spaceID, operation.ExchangeParams{
		AccountID: f.accountID, FromInstrumentID: f.sberID, ToInstrumentID: produced,
		Quantity: decimal.RequireFromString("100"), ToQuantity: decimal.RequireFromString("0.000000005"),
		OccurredOn: date("2024-01-10"), Source: operation.SourceRegistry,
	})
	if err != nil {
		t.Fatalf("CreateExchange: %v", err)
	}
	if len(in.TransferLots) != 3 {
		t.Fatalf("arriving pieces = %+v, want one per parcel", in.TransferLots)
	}
	first := in.TransferLots[0]
	if !first.Quantity.IsZero() || first.CostMinor != 10_000 ||
		first.AcquiredOn == nil || !first.AcquiredOn.Equal(date("2021-01-10")) {
		t.Errorf("first piece = %+v, want no units and 10000 acquired 2021-01-10", first)
	}
	var units decimal.Decimal
	var cost int64
	for _, pc := range in.TransferLots {
		units = units.Add(pc.Quantity)
		cost += pc.CostMinor
	}
	if units.String() != "0.000000005" || cost != 1_010_000 {
		t.Errorf("the pieces come to %s units and %d, want 0.000000005 and 1010000", units, cost)
	}
}
