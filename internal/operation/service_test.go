package operation_test

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
)

func TestServiceValidation(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	valid := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB", FeeMinor: 10,
	}
	if _, err := svc.Create(f.ctx, f.spaceID, valid); err != nil {
		t.Fatalf("valid buy: %v", err)
	}

	cases := map[string]func(o operation.Operation) operation.Operation{
		"buy positive amount": func(o operation.Operation) operation.Operation {
			o.AmountMinor = 100
			return o
		},
		"buy without instrument": func(o operation.Operation) operation.Operation {
			o.InstrumentID = nil
			return o
		},
		"sell without instrument": func(o operation.Operation) operation.Operation {
			o.Type = operation.TypeSell
			o.InstrumentID = nil
			o.AmountMinor = 100_000
			return o
		},
		"bad currency": func(o operation.Operation) operation.Operation {
			o.Currency = "rub"
			return o
		},
		"future date": func(o operation.Operation) operation.Operation {
			o.OccurredOn = date("2099-01-01")
			return o
		},
		"negative fee": func(o operation.Operation) operation.Operation {
			o.FeeMinor = -1
			return o
		},
		"transfer_in via create": func(o operation.Operation) operation.Operation {
			o.Type = operation.TypeTransferIn
			o.AmountMinor = 100
			return o
		},
		"transfer_out via create": func(o operation.Operation) operation.Operation {
			o.Type = operation.TypeTransferOut
			o.AmountMinor = 100
			return o
		},
		"zero dividend amount": func(o operation.Operation) operation.Operation {
			o.Type = operation.TypeDividend
			o.AmountMinor = 0
			return o
		},
		"positive tax amount": func(o operation.Operation) operation.Operation {
			o.Type = operation.TypeTax
			o.AmountMinor = 100
			return o
		},
	}
	for name, mutate := range cases {
		if _, err := svc.Create(f.ctx, f.spaceID, mutate(valid)); !errors.Is(err, family.ErrValidation) {
			t.Errorf("%s: err = %v, want ErrValidation", name, err)
		}
	}

	// oversell rejected on write
	oversell := valid
	oversell.Type = operation.TypeSell
	oversell.Quantity = dec("999")
	oversell.AmountMinor = 999_000
	if _, err := svc.Create(f.ctx, f.spaceID, oversell); !errors.Is(err, operation.ErrInconsistent) {
		t.Errorf("oversell: err = %v, want ErrInconsistent", err)
	}
}

func TestServiceDeleteConsistency(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB",
	}
	createdBuy, err := svc.Create(f.ctx, f.spaceID, buy)
	if err != nil {
		t.Fatalf("buy: %v", err)
	}
	sell := buy
	sell.Type = operation.TypeSell
	sell.OccurredOn = date("2026-07-02")
	sell.Quantity = dec("5")
	sell.AmountMinor = 55_000
	createdSell, err := svc.Create(f.ctx, f.spaceID, sell)
	if err != nil {
		t.Fatalf("sell: %v", err)
	}

	// deleting buy breaks sell → 409
	if err := svc.Delete(f.ctx, f.spaceID, createdBuy.ID); !errors.Is(err, operation.ErrInconsistent) {
		t.Errorf("delete buy: err = %v, want ErrInconsistent", err)
	}
	// deleting sell — ok, then buy — ok
	if err := svc.Delete(f.ctx, f.spaceID, createdSell.ID); err != nil {
		t.Fatalf("delete sell: %v", err)
	}
	if err := svc.Delete(f.ctx, f.spaceID, createdBuy.ID); err != nil {
		t.Fatalf("delete buy after sell: %v", err)
	}
}

func TestServiceTransfer(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB", FeeMinor: 10,
	}
	if _, err := svc.Create(f.ctx, f.spaceID, buy); err != nil {
		t.Fatalf("buy: %v", err)
	}

	out, in, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("4"),
		OccurredOn: date("2026-07-05"),
	})
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	// auto cost: floor((100000+10)*4/10) = 40004
	if out.AmountMinor != 40_004 || in.AmountMinor != 40_004 {
		t.Errorf("cost = %d/%d, want 40004", out.AmountMinor, in.AmountMinor)
	}
	if out.Currency != "RUB" || in.AccountID != f.account2ID {
		t.Errorf("pair = %+v %+v", out, in)
	}

	// transfer exceeding balance → ErrInconsistent
	if _, _, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("100"),
		OccurredOn: date("2026-07-06"),
	}); !errors.Is(err, operation.ErrInconsistent) {
		t.Errorf("oversell transfer: %v", err)
	}
	// from == to
	if _, _, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.accountID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("1"),
		OccurredOn: date("2026-07-06"),
	}); !errors.Is(err, family.ErrValidation) {
		t.Errorf("same account: %v", err)
	}
}

func TestServiceSplitValidation(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// Valid split: instrument + positive ratio + amount 0 + source manual or empty
	validSplit := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSplit,
		OccurredOn: date("2026-07-01"), SplitRatio: dec("10"), AmountMinor: 0,
		Currency: "RUB", Source: "manual",
	}
	if _, err := svc.Create(f.ctx, f.spaceID, validSplit); err != nil {
		t.Fatalf("valid split with source=manual: %v", err)
	}

	validSplitNoSource := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSplit,
		OccurredOn: date("2026-07-02"), SplitRatio: dec("2"), AmountMinor: 0,
		Currency: "RUB", Source: "",
	}
	if _, err := svc.Create(f.ctx, f.spaceID, validSplitNoSource); err != nil {
		t.Fatalf("valid split with empty source: %v", err)
	}

	cases := map[string]func(o operation.Operation) operation.Operation{
		"split without instrument": func(o operation.Operation) operation.Operation {
			o.InstrumentID = nil
			return o
		},
		"split with nil ratio": func(o operation.Operation) operation.Operation {
			o.SplitRatio = nil
			return o
		},
		"split with zero ratio": func(o operation.Operation) operation.Operation {
			o.SplitRatio = dec("0")
			return o
		},
		"split with negative ratio": func(o operation.Operation) operation.Operation {
			o.SplitRatio = dec("-5")
			return o
		},
		"split with non-zero amount": func(o operation.Operation) operation.Operation {
			o.AmountMinor = 100
			return o
		},
		"split with csv source": func(o operation.Operation) operation.Operation {
			o.Source = "csv"
			return o
		},
	}
	for name, mutate := range cases {
		if _, err := svc.Create(f.ctx, f.spaceID, mutate(validSplit)); !errors.Is(err, family.ErrValidation) {
			t.Errorf("%s: err = %v, want ErrValidation", name, err)
		}
	}
}
