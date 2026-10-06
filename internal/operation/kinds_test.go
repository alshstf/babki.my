package operation_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
)

// A coupon happens to a bond and a dividend to a share; a hand entry naming
// the other kind of paper is refused, on creation and on an edit alike. Money
// is not bought as a paper. A fee is charged on anything.
func TestAHandEntryMustFitItsPapersKind(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	papers := instrument.NewStore(f.pool)
	kind := func(k instrument.Type, ticker string) uuid.UUID {
		p, err := papers.Create(f.ctx, instrument.Instrument{Type: k, Name: ticker, Ticker: ticker, Currency: "RUB"})
		if err != nil {
			t.Fatal(err)
		}
		return p.ID
	}
	bond, usd := kind(instrument.TypeBond, "OFZ"), kind(instrument.TypeCurrency, "USDRUB")

	for _, c := range []struct {
		name  string
		op    operation.Operation
		valid bool
	}{
		{"a coupon on a share", operation.Operation{Type: operation.TypeCoupon, InstrumentID: &f.sberID, AmountMinor: 1000}, false},
		{"an amortization on a share", operation.Operation{Type: operation.TypeAmortization, InstrumentID: &f.sberID, AmountMinor: 1000}, false},
		{"a dividend on a bond", operation.Operation{Type: operation.TypeDividend, InstrumentID: &bond, AmountMinor: 1000}, false},
		{"a purchase of money", operation.Operation{Type: operation.TypeBuy, InstrumentID: &usd, Quantity: dec("100"), Price: dec("90"), AmountMinor: -900_000}, false},
		{"a coupon on a bond", operation.Operation{Type: operation.TypeCoupon, InstrumentID: &bond, AmountMinor: 1000}, true},
		{"a dividend on a share", operation.Operation{Type: operation.TypeDividend, InstrumentID: &f.sberID, AmountMinor: 1000}, true},
		{"a fee on a bond", operation.Operation{Type: operation.TypeFee, InstrumentID: &bond, AmountMinor: -100}, true},
	} {
		c.op.AccountID, c.op.OccurredOn, c.op.Currency = f.accountID, date("2026-03-02"), "RUB"
		_, err := svc.Create(f.ctx, f.spaceID, c.op)
		if c.valid && err != nil {
			t.Errorf("%s: refused: %v", c.name, err)
		}
		if !c.valid && !errors.Is(err, family.ErrValidation) {
			t.Errorf("%s: err = %v, want a validation refusal", c.name, err)
		}
	}

	// An edit may not move an entry onto the wrong kind either.
	coupon, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, Type: operation.TypeCoupon, InstrumentID: &bond,
		OccurredOn: date("2026-03-03"), AmountMinor: 500, Currency: "RUB",
	})
	if err != nil {
		t.Fatal(err)
	}
	coupon.InstrumentID = &f.sberID
	if _, err := svc.Update(f.ctx, f.spaceID, coupon.ID, coupon); !errors.Is(err, family.ErrValidation) {
		t.Errorf("moving a coupon onto a share: err = %v, want a validation refusal", err)
	}
}
