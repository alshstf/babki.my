package operation_test

import (
	"errors"
	"math"
	"testing"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/portfolio"
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
		// An unbounded amount poisons the basis and wraps realized P&L.
		"amount_minor at MinInt64": func(o operation.Operation) operation.Operation {
			o.AmountMinor = math.MinInt64
			return o
		},
		"amount_minor beyond cap": func(o operation.Operation) operation.Operation {
			o.Type = operation.TypeDeposit
			o.InstrumentID = nil
			o.Quantity = nil
			o.Price = nil
			o.AmountMinor = 1_000_000_000_000_001
			return o
		},
		"fee_minor beyond cap": func(o operation.Operation) operation.Operation {
			o.FeeMinor = 1_000_000_000_000_001
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

// A transfer that drains one lot and bites into a second hands the
// destination both pieces with their purchase days, and the pair's basis is
// their sum.
//
//	buy 10 @ 100.00 (07-01, cost 100_000), buy 10 @ 900.00 (07-03, 900_000)
//	transfer 15 on 07-05: lot 1 whole (100_000) + 5 of lot 2 (450_000) = 550_000
func TestTransferCarriesSourceLotDates(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy1 := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB",
	}
	buy2 := buy1
	buy2.OccurredOn = date("2026-07-03")
	buy2.Price = dec("900")
	buy2.AmountMinor = -900_000
	for _, op := range []operation.Operation{buy1, buy2} {
		if _, err := svc.Create(f.ctx, f.spaceID, op); err != nil {
			t.Fatalf("seed %s: %v", op.OccurredOn.Format("2006-01-02"), err)
		}
	}

	out, in, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("15"),
		OccurredOn: date("2026-07-05"),
	})
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}

	want := []operation.ReleasedLot{
		{Quantity: decimal.RequireFromString("10"), CostMinor: 100_000, AcquiredOn: datep("2026-07-01")},
		{Quantity: decimal.RequireFromString("5"), CostMinor: 450_000, AcquiredOn: datep("2026-07-03")},
	}
	checkLots := func(what string, got []operation.ReleasedLot) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s lots = %+v, want %d pieces (a single lump would be 1)", what, got, len(want))
		}
		for i, w := range want {
			g := got[i]
			if !g.Quantity.Equal(w.Quantity) || g.CostMinor != w.CostMinor || !sameAcquisition(g.AcquiredOn, w.AcquiredOn) {
				t.Errorf("%s lot %d = %s/%d/%s, want %s/%d/%s", what, i,
					g.Quantity, g.CostMinor, acquired(g.AcquiredOn),
					w.Quantity, w.CostMinor, acquired(w.AcquiredOn))
			}
		}
		var sum int64
		for _, g := range got {
			sum += g.CostMinor
		}
		if sum != in.AmountMinor || sum != out.AmountMinor {
			t.Errorf("%s: sum of piece costs = %d, but the pair records %d/%d",
				what, sum, out.AmountMinor, in.AmountMinor)
		}
	}
	checkLots("returned", in.TransferLots)
	// The departing leg returns the same parcel, piece for piece, as every
	// later read gives it (see TestTransferPairAnswersUndatedTheSameOnBothLegs).
	checkLots("returned transfer_out", out.TransferLots)
	// Carried on both, stored once, next to the arrival.
	if n := f.lotRows(t, out.ID); n != 0 {
		t.Errorf("stored lot rows on the departing leg = %d, want 0 — the breakdown is stored with the arrival and resolved onto its sibling at read time", n)
	}
	if n := f.lotRows(t, in.ID); n != len(want) {
		t.Errorf("stored lot rows on the arriving leg = %d, want %d", n, len(want))
	}
	if in.AmountMinor != 550_000 {
		t.Errorf("carried basis = %d, want 550000", in.AmountMinor)
	}

	// and the same read back the way the engine consumes the journal
	destOps, err := f.store.ListForEngine(f.ctx, f.spaceID, f.account2ID)
	if err != nil {
		t.Fatalf("list dest: %v", err)
	}
	recorded := findByType(destOps, operation.TypeTransferIn)
	if recorded == nil {
		t.Fatalf("no transfer_in recorded on dest account")
	}
	checkLots("stored", recorded.TransferLots)
}

// A basis typed by hand has no source lots, so no pieces and no dates.
func TestTransferManualCostHasNoLots(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB",
	}
	if _, err := svc.Create(f.ctx, f.spaceID, buy); err != nil {
		t.Fatalf("buy: %v", err)
	}

	override := int64(12_345)
	_, in, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("4"),
		OccurredOn: date("2026-07-05"), CostMinorOverride: &override,
	})
	if err != nil {
		t.Fatalf("transfer with manual cost: %v", err)
	}
	if in.AmountMinor != override {
		t.Errorf("carried basis = %d, want %d", in.AmountMinor, override)
	}
	if len(in.TransferLots) != 0 {
		t.Errorf("returned lots = %+v, want none", in.TransferLots)
	}
	if n := f.lotRows(t, in.ID); n != 0 {
		t.Errorf("stored lot rows = %d, want 0", n)
	}
}

// Deleting a transfer removes its breakdown too.
func TestServiceDeleteTransferRemovesLots(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB",
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
	if n := f.lotRows(t, in.ID); n != 1 {
		t.Fatalf("lot rows after transfer = %d, want 1", n)
	}

	if err := svc.Delete(f.ctx, f.spaceID, out.ID); err != nil {
		t.Fatalf("delete transfer: %v", err)
	}
	if n := f.lotRows(t, in.ID); n != 0 {
		t.Errorf("lot rows after delete = %d, want 0", n)
	}
}

// Only the corporate-actions registry may write a split: it happened to the
// paper and is applied to every account that held it.
func TestServiceSplitValidation(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// A valid split; only its source decides which door takes it.
	validSplit := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSplit,
		OccurredOn: date("2026-07-01"), SplitRatio: dec("10"), AmountMinor: 0,
		Currency: "RUB", Source: operation.SourceRegistry,
	}
	seedSplit(t, f, svc, validSplit)

	// The hand-entry door refuses every source.
	for _, source := range []string{"manual", "", "csv", "tinvest"} {
		refused := validSplit
		refused.Source = source
		refused.OccurredOn = date("2026-07-02")
		if _, err := svc.Create(f.ctx, f.spaceID, refused); !errors.Is(err, family.ErrValidation) {
			t.Errorf("split entered by hand with source %q: err = %v, want ErrValidation — a split is the registry's to write", source, err)
		}
	}

	// The import door refuses every source but the registry's.
	for _, source := range []string{"tinvest", "csv"} {
		refused := validSplit
		refused.Source = source
		refused.OccurredOn = date("2026-07-03")
		id := source + "-split"
		refused.ExternalID = &id
		_, imported, err := svc.ApplyImportDelta(f.ctx, f.spaceID, operation.ImportDelta{
			Add: []operation.Operation{refused},
		})
		if err != nil {
			t.Fatalf("import a %s split: %v", source, err)
		}
		if len(imported) != 1 || !errors.Is(imported[0].Err, family.ErrValidation) {
			t.Errorf("split imported as %q: refusals = %v, want one ErrValidation", source, imported)
		}
	}

	// What a split must look like, checked at the registry door.
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
	}
	for name, mutate := range cases {
		broken := mutate(validSplit)
		broken.OccurredOn = date("2026-07-04")
		if err := trySplit(t, f, svc, broken); !errors.Is(err, family.ErrValidation) {
			t.Errorf("%s: err = %v, want ErrValidation", name, err)
		}
	}
}

// A new row is checked after every stored row of its day, whatever this
// process's clock says. Stored rows carry the database clock; one running ahead
// (a purchase stamped a minute in the future here) once put a same-day transfer
// in front of the purchase it moves.
func TestANewRowIsCheckedAfterRowsTheDatabaseClockedAhead(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	buy, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("buy: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE operations SET created_at = now() + interval '1 minute' WHERE id = $1`, buy.ID); err != nil {
		t.Fatalf("move the purchase's clock ahead: %v", err)
	}
	if _, _, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID, InstrumentID: f.sberID,
		Quantity: decimal.RequireFromString("4"), OccurredOn: date("2026-07-01"),
	}); err != nil {
		t.Fatalf("same-day transfer after a purchase the database clocked ahead: %v", err)
	}
}

// TestTransferSameDayBoundary pins journalUpTo's inclusive boundary: a transfer
// sees the purchases and sales of its own day.
func TestTransferSameDayBoundary(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// (a) The only purchase is on the transfer's day:
	// buy 10 @ 100 on 07-01, transfer 4 on 07-01 releases 40_000.
	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB", FeeMinor: 0,
	}
	if _, err := svc.Create(f.ctx, f.spaceID, buy); err != nil {
		t.Fatalf("buy: %v", err)
	}

	out, in, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("4"),
		OccurredOn: date("2026-07-01"), // same day as buy
	})
	if err != nil {
		t.Fatalf("same-day transfer: %v", err)
	}
	const wantCostA = int64(40_000)
	if out.AmountMinor != wantCostA || in.AmountMinor != wantCostA {
		t.Errorf("case (a) same-day cost = %d/%d, want %d (Before would fail: 0)",
			out.AmountMinor, in.AmountMinor, wantCostA)
	}

	// (b) A same-day sale before the transfer moves the FIFO front:
	// buy 10 @ 100 (07-01), buy 10 @ 900 (07-03), sell 10 (07-05),
	// transfer 4 on 07-05 releases from lot 2: 360_000.
	f2 := newFixture(t)
	svc2 := operation.NewService(f2.store)

	buy1 := operation.Operation{
		AccountID: f2.accountID, InstrumentID: &f2.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB",
	}
	buy2 := buy1
	buy2.OccurredOn = date("2026-07-03")
	buy2.Price = dec("900")
	buy2.AmountMinor = -900_000

	sell := buy1
	sell.Type = operation.TypeSell
	sell.OccurredOn = date("2026-07-05")
	sell.Quantity = dec("10")
	sell.Price = dec("1000")
	sell.AmountMinor = 1_000_000

	for _, op := range []operation.Operation{buy1, buy2, sell} {
		if _, err := svc2.Create(f2.ctx, f2.spaceID, op); err != nil {
			t.Fatalf("seed %s %s: %v", op.Type, op.OccurredOn.Format("2006-01-02"), err)
		}
	}

	out2, in2, err := svc2.CreateTransfer(f2.ctx, f2.spaceID, operation.TransferParams{
		FromAccountID: f2.accountID, ToAccountID: f2.account2ID,
		InstrumentID: f2.sberID, Quantity: decimal.RequireFromString("4"),
		OccurredOn: date("2026-07-05"), // same day as sell
	})
	if err != nil {
		t.Fatalf("same-day transfer after sale: %v", err)
	}
	const wantCostB = int64(360_000)
	if out2.AmountMinor != wantCostB || in2.AmountMinor != wantCostB {
		t.Errorf("case (b) same-day cost = %d/%d, want %d (FIFO after-sell front)",
			out2.AmountMinor, in2.AmountMinor, wantCostB)
	}
}

// A backdated transfer takes its basis from the journal as of its own date,
// not the end state.
//
//	buy1 10 @ 100 (07-01, cost 100_000)
//	buy2 10 @ 900 (07-03, cost 900_000)
//	sell 10       (07-20, already stored when the transfer is made)
//	transfer 5 backdated to 07-05
//
// As of 07-05 lot 1 is intact: 100_000 * 5 / 10 = 50_000. Folding the whole
// journal would put lot 2 in front: 450_000, a basis out of nothing.
func TestTransferBasisConservation(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy1 := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB",
	}
	buy2 := buy1
	buy2.OccurredOn = date("2026-07-03")
	buy2.Price = dec("900")
	buy2.AmountMinor = -900_000

	sell := buy1
	sell.Type = operation.TypeSell
	sell.OccurredOn = date("2026-07-20")
	sell.Quantity = dec("10")
	sell.Price = dec("20")
	sell.AmountMinor = 200_000

	for _, op := range []operation.Operation{buy1, buy2, sell} {
		if _, err := svc.Create(f.ctx, f.spaceID, op); err != nil {
			t.Fatalf("seed %s %s: %v", op.Type, op.OccurredOn.Format("2006-01-02"), err)
		}
	}

	// An independent oracle: the date filter reimplemented here and run through
	// portfolio.ReleasedCost.
	srcOps, err := f.store.ListForEngine(f.ctx, f.spaceID, f.accountID)
	if err != nil {
		t.Fatalf("list source: %v", err)
	}
	transferDate := date("2026-07-05")
	var truncated []operation.Operation
	for _, o := range srcOps {
		if !o.OccurredOn.After(transferDate) {
			truncated = append(truncated, o)
		}
	}
	wantCost, err := portfolio.ReleasedCost(truncated, f.sberID, decimal.RequireFromString("5"))
	if err != nil {
		t.Fatalf("oracle ReleasedCost: %v", err)
	}
	if wantCost != 50_000 {
		t.Fatalf("oracle sanity: wantCost = %d, want 50000 (see scenario comment)", wantCost)
	}

	out, in, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("5"),
		OccurredOn: transferDate,
	})
	if err != nil {
		t.Fatalf("backdated transfer: %v", err)
	}
	if out.AmountMinor != wantCost {
		t.Errorf("transfer_out.AmountMinor = %d, want %d (C1 leak would give 450000)", out.AmountMinor, wantCost)
	}
	if in.AmountMinor != wantCost {
		t.Errorf("transfer_in.AmountMinor = %d, want %d (C1 leak would give 450000)", in.AmountMinor, wantCost)
	}

	// And the value as persisted, read the way the app reads it.
	destOps, err := f.store.ListForEngine(f.ctx, f.spaceID, f.account2ID)
	if err != nil {
		t.Fatalf("list dest: %v", err)
	}
	var recordedIn *operation.Operation
	for i := range destOps {
		if destOps[i].Type == operation.TypeTransferIn {
			recordedIn = &destOps[i]
		}
	}
	if recordedIn == nil {
		t.Fatalf("no transfer_in recorded on dest account")
	}
	if recordedIn.AmountMinor != wantCost {
		t.Errorf("recorded transfer_in.AmountMinor = %d, want %d (C1 leak would give 450000)",
			recordedIn.AmountMinor, wantCost)
	}

	// The destination lot carries the recorded basis.
	destPos, err := portfolio.Compute(destOps)
	if err != nil {
		t.Fatalf("compute dest: %v", err)
	}
	if destPos[f.sberID].CostMinor != wantCost {
		t.Errorf("dest position CostMinor = %d, want %d", destPos[f.sberID].CostMinor, wantCost)
	}
}

// A row an importer wrote is refused: the next sync would write it back.
func TestServiceDeleteRefusesAnImportedOperation(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	external := "op-1"
	imported, err := f.store.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit, OccurredOn: date("2026-07-01"),
		AmountMinor: 1_000, Currency: "RUB", Source: "tinvest", ExternalID: &external,
	}, nil)
	if err != nil {
		t.Fatalf("imported row: %v", err)
	}
	byHand, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit, OccurredOn: date("2026-07-02"),
		AmountMinor: 2_000, Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("manual row: %v", err)
	}

	if err := svc.Delete(f.ctx, f.spaceID, imported.ID); !errors.Is(err, family.ErrValidation) {
		t.Errorf("deleting an imported operation: err = %v, want ErrValidation", err)
	}
	list, err := f.store.ListForEngine(f.ctx, f.spaceID, f.accountID)
	if err != nil {
		t.Fatalf("list journal: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("journal holds %d operations after a refused delete, want both", len(list))
	}

	// The manual row is still the owner's to remove.
	if err := svc.Delete(f.ctx, f.spaceID, byHand.ID); err != nil {
		t.Errorf("deleting a manual operation: %v", err)
	}
}
