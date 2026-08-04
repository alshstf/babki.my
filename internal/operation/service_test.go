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
		// Bounds: an unbounded amount_minor poisons the cost basis and wraps
		// realized P&L on the next addition.
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

// TestTransferBackdatedUsesChronologicalBasis pins the fix for a basis leak:
// a transfer dated before existing operations must take its FIFO cost from
// the journal as of its own date, not from the journal's end state.
//
// Source journal: buy 10 @ 100.00 (01.07), buy 10 @ 900.00 (03.07),
// sell 10 (20.07) — the sell consumes lot 1 entirely.
// Transferring 5 units backdated to 05.07 must release from lot 1, which is
// still intact on that date: floor(100000 * 5 / 10) = 50000.
// Folding the whole journal instead would leave only lot 2 in front and
// release floor(900000 * 5 / 10) = 450000 — 400000 minor units of cost basis
// out of thin air.
func TestTransferBackdatedUsesChronologicalBasis(t *testing.T) {
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
	sell.Price = dec("1000")
	sell.AmountMinor = 1_000_000
	for _, op := range []operation.Operation{buy1, buy2, sell} {
		if _, err := svc.Create(f.ctx, f.spaceID, op); err != nil {
			t.Fatalf("seed %s %s: %v", op.Type, op.OccurredOn.Format("2006-01-02"), err)
		}
	}

	out, in, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID,
		InstrumentID: f.sberID, Quantity: decimal.RequireFromString("5"),
		OccurredOn: date("2026-07-05"),
	})
	if err != nil {
		t.Fatalf("backdated transfer: %v", err)
	}
	const wantCost = int64(50_000)
	if out.AmountMinor != wantCost || in.AmountMinor != wantCost {
		t.Errorf("cost = %d/%d, want %d/%d (end-state basis would be 450000)",
			out.AmountMinor, in.AmountMinor, wantCost, wantCost)
	}
}

// TestTransferCarriesSourceLotDates is the point of the whole change: a
// transfer that drains one lot and bites into a second must hand the
// destination both pieces with the days they were actually bought, not one
// lump sum dated on the transfer day. Their costs must add up to exactly the
// basis recorded on the pair — that number is the sum of the pieces, so a
// mismatch means it was computed a second, independent way.
//
// Source journal: buy 10 @ 100.00 (01.07, lot cost 100000),
// buy 10 @ 900.00 (03.07, lot cost 900000). Transferring 15 units on 05.07
// takes lot 1 whole (10 units, 100000) and 5 units of lot 2
// (floor(900000 * 5 / 10) = 450000) — 550000 in total.
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
	// The departing leg comes back describing the same parcel, piece for piece.
	// Both legs record ONE parcel — same shares, same basis, same purchases —
	// and Store.attachTransferLots has handed the pieces to both on every later
	// read since plan 7b; the create path returning them on one leg only left
	// the pair contradicting itself inside a single response, once whether a
	// transfer knows its purchase dates became something published per
	// operation (has_undated_lots — see the API contract and
	// TestTransferPairAnswersUndatedTheSameOnBothLegs).
	checkLots("returned transfer_out", out.TransferLots)
	// Carried on both, STORED once: the rows live next to the arrival, whose
	// account is the one that cannot recover the dates any other way. This is
	// the layout the assertion above used to stand in for, pinned where it
	// actually holds rather than through an in-memory struct.
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

// TestTransferManualCostHasNoLots pins the legitimate empty case: a basis
// typed in by hand has no source lots behind it, so there is nothing to
// break down and no dates to attach. Recording invented pieces here would be
// worse than recording none.
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

// TestServiceDeleteTransferRemovesLots covers the user-facing delete path:
// removing a transfer must not leave its breakdown behind describing an
// operation that no longer exists.
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

// TestTransferSameDayBoundary pins the journalUpTo boundary:
// !o.OccurredOn.After(day) (INCLUSIVE) is the critical filter.
// If changed to Before(day) (exclusive), same-day transfers would not see
// same-day purchases, silently losing cost basis.
//
// Note on case (b): its journal has no operation dated AFTER the transfer
// date at CreateTransfer time (the sell is same-day, not later), so
// journalUpTo(ops, day) and the unfiltered ops are identical sets for this
// case — dropping the filter entirely (the C1 bug: folding the whole,
// possibly-future-inclusive journal) would NOT change case (b)'s result.
// What case (b) actually pins is that (1) a same-day operation created
// *before* the transfer is included (inclusive boundary, same as case (a)),
// and (2) the FIFO front correctly reflects that prior same-day sale (lot 2,
// not lot 1) — i.e. same-day ordering by created_at, not the "no filter at
// all" regression. TestTransferBasisConservation below is the test that
// discriminates the filter being dropped entirely.
func TestTransferSameDayBoundary(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// Case (a): Transfer on same day as only purchase.
	// buy 10 @ 100 = -100_000 on 2026-07-01
	// transfer 4 on 2026-07-01 → should see the buy and release floor(100_000 * 4 / 10) = 40_000
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

	// Case (b): Sale and transfer on same day; sale precedes transfer.
	// buy 10 @ 100 = -100_000 on 2026-07-01
	// buy 10 @ 900 = -900_000 on 2026-07-03
	// sell 10 on 2026-07-05 (consumes lot 1 completely)
	// transfer 4 on 2026-07-05 (FIFO front is now lot 2; release floor(900_000 * 4 / 10) = 360_000)
	// (This pins same-day inclusion + intra-day FIFO ordering, not the
	// filter-dropped-entirely bug — see the func-level comment above.)
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

// TestTransferBasisConservation pins the C1 basis-leak fix with a scenario
// that actually discriminates it. A prior version of this test (commit
// 7ac995e) did not: it used a single lot, and a "conservation" formula that
// turned out to be a tautology. Both problems, and why the replacement
// below fixes them, are documented here.
//
// Problem 1 — non-discriminating scenario: with a single lot, the FIFO
// front on the transfer's own date and on the whole journal's end state is
// the same lot, so removing the journalUpTo filter changes nothing. C1 only
// shows up when the front lot at the transfer date differs from the front
// lot when the whole journal (including operations dated AFTER the
// transfer) is folded — i.e. two lots at different prices, plus a
// later-dated operation that is already sitting in the account's stored
// journal by the time the transfer is created.
//
// Problem 2 — self-fulfilling formula: the old check computed
//
//	costReleased := totalProceeds - totalRealizedPnL - totalFees
//
// but the engine computes RealizedPnLMinor as exactly
// `proceeds - released - fee` (internal/portfolio/engine.go, TypeSell), so
// algebraically totalProceeds - totalRealizedPnL - totalFees ≡ released,
// for ANY value the engine used internally as "released" — correct or
// C1-corrupted. The check was verifying the engine's own bookkeeping is
// self-consistent, which it always is; it could never fail from a wrong
// transferred basis.
//
// Why the fix below is a direct check on the recorded value rather than a
// "purer" conservation formula: a real conservation invariant avoiding the
// RealizedPnL identity was worked through by hand, and it collapses to the
// same direct check anyway. When this test was written the source account's
// post-transfer state was IDENTICAL whether the recorded transfer cost was
// correct or C1-corrupted — the engine discarded transfer_out's own figures
// and re-derived a release from the quantity alone — so only the
// destination's recorded transfer_in.AmountMinor differed between the two,
// and there was no conservation law across source+dest+releases with any
// discriminating power over asserting the recorded value directly. Per the
// task's own guidance that was treated as an informed fallback, not a
// shortcut.
//
// Issue #60 has since changed the premise: the departing leg releases the
// lots the breakdown records (see portfolio.Position.releaseRecorded), so a
// wrong basis is now visible on the source account as well, and conservation
// across the two accounts IS assertable — see
// TestTransferReleasesTheParcelItRecorded, which asserts exactly that for the
// fault #60 describes. The direct check below is kept because it is still the
// sharpest statement of C1 in particular: it names the one number the leak
// moves, at the moment it is recorded.
//
// Scenario (source account):
//
//	buy1: 10 @ 100.00 → -100_000  (07-01, lot1 cost 100_000)
//	buy2: 10 @ 900.00 → -900_000  (07-03, lot2 cost 900_000)
//	sell: 10 units    → +200_000  (07-20 — AFTER the transfer's own date,
//	                                but created BEFORE CreateTransfer runs
//	                                below, so it is already present in the
//	                                source's stored journal at that point)
//	transfer 5 units, backdated to 07-05
//
// Correct (journalUpTo, ops with occurred_on <= 07-05 → [buy1, buy2]):
// FIFO front is lot1, still fully intact: release 5 units →
// floor(100_000 * 5 / 10) = 50_000.
//
// C1-buggy (no filter, full journal folded → [buy1, buy2, sell]): the sell
// (qty 10) exactly drains lot1 when folded chronologically ahead of the
// transfer, leaving only lot2 in front: release 5 units →
// floor(900_000 * 5 / 10) = 450_000.
//
// So the correct transfer_in.AmountMinor is 50_000; C1 would record
// 450_000 — a 400_000 minor-unit basis conjured out of thin air. Verified
// live: with journalUpTo's filter stubbed out (service.go temporarily
// passing sourceJournal instead of journalUpTo(sourceJournal, ...)) this
// test fails with exactly "= 450000, want 50000" on all three assertions
// below; restoring the filter turns it green again (see the fix commit's
// message / final-review-3a-fixes.md for the captured failure output).
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

	// Independent oracle: reimplement the *intent* of the journalUpTo filter
	// locally (occurred_on <= transfer date), not by calling production's
	// unexported journalUpTo, and run it through portfolio.ReleasedCost
	// directly. This does not depend on Service.CreateTransfer's internal
	// wiring at all, so it stays correct even if CreateTransfer regresses to
	// folding the full, unfiltered journal — the two would then diverge.
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

	// Cross-check the value actually PERSISTED in the store (not just the
	// in-memory return value), read back the same way the rest of the app
	// consumes the journal: store.ListForEngine.
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

	// Supplementary and NOT discriminating for C1 on its own (it would pass
	// under the bug too, since it just reflects whatever got recorded): dest's
	// computed position must match the recorded basis exactly, confirming
	// Compute() wires the stored transfer_in value straight into the new lot.
	destPos, err := portfolio.Compute(destOps)
	if err != nil {
		t.Fatalf("compute dest: %v", err)
	}
	if destPos[f.sberID].CostMinor != wantCost {
		t.Errorf("dest position CostMinor = %d, want %d", destPos[f.sberID].CostMinor, wantCost)
	}
}

// TestServiceDeleteRefusesAnImportedOperation pins the door that has to close
// the moment an importer can write rows of its own.
//
// The projection is rebuilt from the broker's mirror, and a rebuild writes back
// whatever the mirror still says exists. Deleting one of its rows by hand would
// therefore be undone by the next sync without a word — the row reappears, and
// "deleted" was a lie the program told. The honest answer is to refuse, and to
// say who owns the row.
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
