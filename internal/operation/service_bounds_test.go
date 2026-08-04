package operation_test

import (
	"errors"
	"strings"
	"testing"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
)

// A quantity and a price are the two factors of one product, and that product
// is money: it is what the trade was for. Everything that later reads the
// journal multiplies the quantity by a price — a quote's, not this operation's
// — and then by an fx rate, and the result has to be an int64 of minor units.
// Plan 10 made a product that does not fit refuse to be published instead of
// wrapping, but that refusal arrives on the READ: the position screen answers
// 500 for as long as the row exists, and the only repair is to find and delete
// it. The same figure caught on the WRITE is a rejected field.
//
// These tests are about the write side. The read side keeps its own guard —
// see TestRowsWrittenBeforeTheBoundAreStillWorkable for why it has to.

// maxQuantity and maxPrice as strings, spelled out rather than derived from the
// package's own constants (they are unexported, and a test that computed them
// the same way the code does would agree with a wrong bound).
const (
	quantityBound = "1000000000000000" // 10^15 units
	priceBound    = "10000000000000"   // 10^13 major currency units per unit
)

// TestQuantityBeyondTheBoundIsRefusedAtTheWrite is the case from #84 as it
// actually arrives: an import whose quantity column was scaled wrong. 10^17
// shares is accepted by every check there is today — positive, on the journal's
// decimal scale, comfortably inside NUMERIC(30,10) — and the position it makes
// cannot be valued at any ordinary price.
func TestQuantityBeyondTheBoundIsRefusedAtTheWrite(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("1e17"), Price: dec("0.000001"),
		AmountMinor: -10_000_000, Currency: "RUB",
	}
	_, err := svc.Create(f.ctx, f.spaceID, buy)
	if !errors.Is(err, family.ErrValidation) {
		t.Fatalf("buy of 1e17 units: err = %v, want ErrValidation — a quantity no price can value must not enter the journal", err)
	}
	// The refusal has to say which field and what the limit is: this is the one
	// message that tells an importer which column it mis-scaled, and it must not
	// read like "something went wrong".
	if !strings.Contains(err.Error(), "quantity") || !strings.Contains(err.Error(), quantityBound) {
		t.Errorf("refusal = %q, want it to name the field (quantity) and the bound (%s)", err, quantityBound)
	}
}

// TestQuantityExactlyAtTheBoundIsAccepted is the other side of the same guard.
// A bound that refuses the value ON it withholds a figure that is perfectly
// representable, and says nothing about why — the same class of harm as
// accepting one that wraps.
func TestQuantityExactlyAtTheBoundIsAccepted(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// 10^15 units at one kopeck each: the notional is exactly the cap on
	// amount_minor, so nothing else in validate has anything to object to.
	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec(quantityBound), Price: dec("0.01"),
		AmountMinor: -1_000_000_000_000_000, Currency: "RUB",
	}
	created, err := svc.Create(f.ctx, f.spaceID, buy)
	if err != nil {
		t.Fatalf("buy of exactly %s units: %v — the value ON the bound is inside it", quantityBound, err)
	}
	if created.Quantity == nil || created.Quantity.String() != quantityBound {
		t.Errorf("stored quantity = %v, want %s", created.Quantity, quantityBound)
	}
}

// TestPriceBeyondTheBoundIsRefusedAtTheWrite covers the other factor. The
// quantity here is tiny, so their product is ordinary money and only the price
// itself is out of range — a price above the bound is more money for ONE unit
// than a whole operation is allowed to be for.
func TestPriceBeyondTheBoundIsRefusedAtTheWrite(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("0.0001"), Price: dec("10000000000001"),
		AmountMinor: -1_000_000, Currency: "RUB",
	}
	_, err := svc.Create(f.ctx, f.spaceID, buy)
	if !errors.Is(err, family.ErrValidation) {
		t.Fatalf("buy at a price of 10^13+1: err = %v, want ErrValidation", err)
	}
	if !strings.Contains(err.Error(), "price") || !strings.Contains(err.Error(), priceBound) {
		t.Errorf("refusal = %q, want it to name the field (price) and the bound (%s)", err, priceBound)
	}
}

// TestPriceExactlyAtTheBoundIsAccepted mirrors the quantity case at the other
// factor's edge.
func TestPriceExactlyAtTheBoundIsAccepted(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// One ten-thousandth of a unit at 10^13 apiece: 10^9 in money, ordinary.
	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("0.0001"), Price: dec(priceBound),
		AmountMinor: -100_000_000_000, Currency: "RUB",
	}
	if _, err := svc.Create(f.ctx, f.spaceID, buy); err != nil {
		t.Fatalf("buy at a price of exactly %s: %v — the value ON the bound is inside it", priceBound, err)
	}
}

// TestPriceTimesQuantityBeyondTheMoneyCapIsRefused is the check the two
// per-field bounds cannot make between them. Each factor here is far inside its
// own limit — 10^8 units is a ten-millionth of the quantity bound, 10^6 apiece a
// ten-millionth of the price bound — and the product is 10^16 minor units, ten
// times the most money one operation is allowed to be for.
//
// The margin is deliberately that narrow. The product is 10^14 in MAJOR units,
// which is inside the cap when the cap is read as major units, so a check that
// forgot to shift the product into minor ones — the units the cap is in — would
// wave this through and no other test here would notice.
func TestPriceTimesQuantityBeyondTheMoneyCapIsRefused(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("1e8"), Price: dec("1e6"),
		AmountMinor: -1_000_000, Currency: "RUB",
	}
	_, err := svc.Create(f.ctx, f.spaceID, buy)
	if !errors.Is(err, family.ErrValidation) {
		t.Fatalf("buy of 1e8 units at 1e6: err = %v, want ErrValidation — each factor fits, the product does not", err)
	}
	if !strings.Contains(err.Error(), "price") || !strings.Contains(err.Error(), "quantity") {
		t.Errorf("refusal = %q, want it to name both factors: neither field is wrong on its own", err)
	}
}

// TestTransferQuantityIsBoundedToo closes the other door into the same column.
// A transfer writes the moved quantity into two rows of its own, and it does
// not go through validate at all.
//
// It is not merely symmetry: the bound is per OPERATION, and a position is the
// sum of many, so a holding can legitimately grow past the bound one accepted
// buy at a time and then be moved in one transfer.
func TestTransferQuantityIsBoundedToo(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// Two accepted buys of 10^15 each leave a position of 2×10^15 — above the
	// per-operation bound, by rows every check passed.
	for _, on := range []string{"2026-07-01", "2026-07-02"} {
		buy := operation.Operation{
			AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
			OccurredOn: date(on), Quantity: dec(quantityBound), Price: dec("0.01"),
			AmountMinor: -1_000_000_000_000_000, Currency: "RUB",
		}
		if _, err := svc.Create(f.ctx, f.spaceID, buy); err != nil {
			t.Fatalf("buy on %s: %v", on, err)
		}
	}

	_, _, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID, InstrumentID: f.sberID,
		Quantity: *dec("2000000000000000"), OccurredOn: date("2026-07-03"),
	})
	if !errors.Is(err, family.ErrValidation) {
		t.Fatalf("transfer of 2e15 units: err = %v, want ErrValidation", err)
	}
	if !strings.Contains(err.Error(), "quantity") || !strings.Contains(err.Error(), quantityBound) {
		t.Errorf("refusal = %q, want it to name the field (quantity) and the bound (%s)", err, quantityBound)
	}
}

// TestRowsWrittenBeforeTheBoundAreStillWorkable is the promise the change owes
// the data that is already there. No migration comes with the bound: a journal
// written before it keeps whatever it holds, and the only thing that changes is
// what may be ADDED.
//
// Both halves matter. An account carrying such a row must still accept ordinary
// operations — validate looks at the candidate, never at the journal it joins —
// and the offending row must still be deletable, since deleting it is the one
// repair available. A bound applied on the way OUT of the database would take
// both away: nothing could be written to the account and nothing removed from
// it, which is the state this whole plan exists to avoid.
func TestRowsWrittenBeforeTheBoundAreStillWorkable(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// Straight through the store, which is how such a row got there: the bound
	// lives in the service, and the service is what did not have it.
	monster := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("1e17"), Price: dec("0.000001"),
		AmountMinor: -10_000_000, Currency: "RUB",
	}
	stored, err := f.store.Create(f.ctx, f.spaceID, monster, func(operation.Operation) error { return nil })
	if err != nil {
		t.Fatalf("seed the pre-existing row: %v", err)
	}

	ordinary := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-02"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB",
	}
	if _, err := svc.Create(f.ctx, f.spaceID, ordinary); err != nil {
		t.Fatalf("ordinary buy on an account holding an over-bound row: %v — the bound checks the candidate, not the journal", err)
	}

	if err := svc.Delete(f.ctx, f.spaceID, stored.ID); err != nil {
		t.Fatalf("delete the over-bound row: %v — deleting it is the only repair there is", err)
	}
}
