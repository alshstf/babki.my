package operation_test

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/portfolio"
)

// The owner's case: Т-Капитал carved blocked assets out of a fund into a
// closed one, one for one, on 2023-12-22. The original keeps its units and part
// of its cost moves across with its days (НК РФ ст. 214.1 п. 13 абз. 8,
// ст. 277 п. 7).
func TestCreateSpinoffKeepsTheUnitsAndCarvesOutAShareOfTheMoney(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	carvedID := newPaper(t, f, "TECH2", "Тинькофф Технологии заблокированные активы")

	for _, op := range []operation.Operation{{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2020-12-30"), Quantity: dec("5400"), Price: dec("0.0957"),
		AmountMinor: -51_678, Currency: "RUB",
	}, {
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2021-01-25"), Quantity: dec("9668"), Price: dec("0.1028"),
		AmountMinor: -99_387, Currency: "RUB",
	}} {
		if _, err := svc.Create(f.ctx, f.spaceID, op); err != nil {
			t.Fatalf("seed buy: %v", err)
		}
	}

	out, in, err := svc.CreateSpinoff(f.ctx, f.spaceID, operation.SpinoffParams{
		AccountID:        f.accountID,
		FromInstrumentID: f.sberID,
		ToInstrumentID:   carvedID,
		RatioFrom:        decimal.RequireFromString("1"),
		RatioTo:          decimal.RequireFromString("1"),
		BasisShare:       decimal.RequireFromString("0.4"),
		OccurredOn:       date("2023-12-22"),
		Source:           operation.SourceRegistry,
		Note:             "Выделение заблокированных активов",
	})
	if err != nil {
		t.Fatalf("CreateSpinoff: %v", err)
	}

	// floor((51678 + 99387) x 0.4) = 60 426, and both legs say so.
	const moved = 60_426
	if out.AmountMinor != moved || in.AmountMinor != moved {
		t.Errorf("legs carry %d/%d, want %d on both", out.AmountMinor, in.AmountMinor, moved)
	}
	// THE DEPARTING LEG CARRIES NO QUANTITY. Nothing left, and a count here
	// would be drawn as units leaving on every screen that renders a journal.
	if out.Quantity != nil {
		t.Errorf("the departing leg carries a quantity of %s, want none — a spin-off moves no units", out.Quantity)
	}
	if in.Quantity == nil || !in.Quantity.Equal(decimal.RequireFromString("15068")) {
		t.Errorf("the arriving leg brings %v units, want 15068 — one for one against the holding", in.Quantity)
	}
	if out.TransferGroupID == nil || in.TransferGroupID == nil || *out.TransferGroupID != *in.TransferGroupID {
		t.Error("the legs do not share a transfer group")
	}
	// Each leg stores its own breakdown, read back from the database.
	if n := f.lotRows(t, out.ID); n != 2 {
		t.Errorf("departing leg stored %d pieces, want 2", n)
	}
	if n := f.lotRows(t, in.ID); n != 2 {
		t.Errorf("arriving leg stored %d pieces, want 2", n)
	}

	journal, err := f.store.ListForEngine(f.ctx, f.spaceID, f.accountID)
	if err != nil {
		t.Fatalf("ListForEngine: %v", err)
	}
	positions, err := portfolio.Compute(journal)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}

	original := positions[f.sberID]
	if !original.Quantity.Equal(decimal.RequireFromString("15068")) {
		t.Errorf("the original paper holds %s units, want the 15068 it held before", original.Quantity)
	}
	if original.CostMinor != 151_065-moved {
		t.Errorf("the original keeps %d minor, want %d", original.CostMinor, 151_065-moved)
	}

	carved := positions[carvedID]
	if carved.CostMinor != moved {
		t.Errorf("the carved-out paper carries %d minor, want %d", carved.CostMinor, moved)
	}
	if len(carved.Lots) != 2 {
		t.Fatalf("the carved-out paper has %d parcels, want 2", len(carved.Lots))
	}
	// The days are the purchases', not 2023-12-22.
	if carved.Lots[0].AcquiredOn == nil || carved.Lots[0].AcquiredOn.Format("2006-01-02") != "2020-12-30" {
		t.Errorf("first carved parcel acquired %v, want 2020-12-30", carved.Lots[0].AcquiredOn)
	}
	if carved.Lots[1].AcquiredOn == nil || carved.Lots[1].AcquiredOn.Format("2006-01-02") != "2021-01-25" {
		t.Errorf("second carved parcel acquired %v, want 2021-01-25", carved.Lots[1].AcquiredOn)
	}
	if total := original.CostMinor + carved.CostMinor; total != 151_065 {
		t.Errorf("the two papers hold %d between them, want the 151065 that was paid", total)
	}
}

// A reverse split can leave the last parcel with no units and real money.
// Restating it in the new paper's units must not lose that money, or the arriving
// pieces fall short of the row's basis and the pair is refused; quantizeLots folds
// a trailing remainder back into the last parcel with units.
func TestCreateSpinoffOutOfAPositionWhoseLastParcelIsShareless(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	carvedID := newPaper(t, f, "TIPO2", "Тинькофф индекс IPO заблокированные активы")

	// 1.5 and 0.4 units reversed by 1e-10, the finest ratio the journal keeps:
	// both running totals truncate to 1e-10, so the second parcel keeps no units
	// and all its money. Whole numbers cannot produce this.
	for _, op := range []operation.Operation{{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2021-01-04"), Quantity: dec("1.5"), Price: dec("100"),
		AmountMinor: -15_000, Currency: "RUB",
	}, {
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2021-02-04"), Quantity: dec("0.4"), Price: dec("100"),
		AmountMinor: -4_000, Currency: "RUB",
	}} {
		if _, err := svc.Create(f.ctx, f.spaceID, op); err != nil {
			t.Fatalf("seed buy: %v", err)
		}
	}
	if _, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSplit,
		OccurredOn: date("2021-03-04"), SplitRatio: dec("0.0000000001"),
		Currency: "RUB", Source: operation.SourceRegistry,
	}); err != nil {
		t.Fatalf("seed reverse split: %v", err)
	}

	journal, err := f.store.ListForEngine(f.ctx, f.spaceID, f.accountID)
	if err != nil {
		t.Fatalf("ListForEngine: %v", err)
	}
	before, err := portfolio.Compute(journal)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	lots := before[f.sberID].Lots
	if len(lots) != 2 || !lots[1].Quantity.IsZero() || lots[1].CostMinor == 0 {
		t.Fatalf("the fixture did not leave a shareless LAST parcel with money in it: %+v", lots)
	}

	out, in, err := svc.CreateSpinoff(f.ctx, f.spaceID, operation.SpinoffParams{
		AccountID:        f.accountID,
		FromInstrumentID: f.sberID,
		ToInstrumentID:   carvedID,
		RatioFrom:        decimal.RequireFromString("1"),
		RatioTo:          decimal.RequireFromString("1"),
		BasisShare:       decimal.RequireFromString("0.5"),
		OccurredOn:       date("2023-12-22"),
		Source:           operation.SourceRegistry,
	})
	if err != nil {
		t.Fatalf("CreateSpinoff: %v — the shareless parcel's money had nowhere to go", err)
	}

	// Half of the 19 000 paid, and every kopeck of it arrives.
	if out.AmountMinor != 9_500 || in.AmountMinor != 9_500 {
		t.Errorf("legs carry %d/%d, want 9500 on both", out.AmountMinor, in.AmountMinor)
	}

	after, err := f.store.ListForEngine(f.ctx, f.spaceID, f.accountID)
	if err != nil {
		t.Fatalf("ListForEngine: %v", err)
	}
	positions, err := portfolio.Compute(after)
	if err != nil {
		t.Fatalf("Compute after the spin-off: %v", err)
	}
	if got := positions[carvedID].CostMinor; got != 9_500 {
		t.Errorf("the carved-out paper carries %d, want 9500 — including the shareless parcel's half", got)
	}
	if total := positions[f.sberID].CostMinor + positions[carvedID].CostMinor; total != 19_000 {
		t.Errorf("the two papers hold %d between them, want the 19000 that was paid", total)
	}
}

// Each refusal is a rule of its own.
func TestCreateSpinoffRefusesWhatItCannotAccountFor(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	carvedID := newPaper(t, f, "TSPX2", "Тинькофф США 500 заблокированные активы")

	if _, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2021-01-04"), Quantity: dec("100"), Price: dec("10"),
		AmountMinor: -100_000, Currency: "RUB",
	}); err != nil {
		t.Fatalf("seed buy: %v", err)
	}

	base := operation.SpinoffParams{
		AccountID:        f.accountID,
		FromInstrumentID: f.sberID,
		ToInstrumentID:   carvedID,
		RatioFrom:        decimal.RequireFromString("1"),
		RatioTo:          decimal.RequireFromString("1"),
		BasisShare:       decimal.RequireFromString("0.4"),
		OccurredOn:       date("2023-12-22"),
		Source:           operation.SourceRegistry,
	}

	cases := []struct {
		name string
		want string
		edit func(*operation.SpinoffParams)
	}{{
		name: "a share of everything is a conversion, not a spin-off",
		want: "at least 0 and less than 1",
		edit: func(p *operation.SpinoffParams) { p.BasisShare = decimal.RequireFromString("1") },
	}, {
		name: "a negative share",
		want: "at least 0 and less than 1",
		edit: func(p *operation.SpinoffParams) { p.BasisShare = decimal.RequireFromString("-0.1") },
	}, {
		name: "the same paper on both sides",
		want: "different paper",
		edit: func(p *operation.SpinoffParams) { p.ToInstrumentID = f.sberID },
	}, {
		name: "hand entry, which is the registry's alone",
		want: "only written by source=registry",
		edit: func(p *operation.SpinoffParams) { p.Source = "manual" },
	}, {
		name: "nothing held on the day it took effect",
		want: "held nothing of the paper",
		edit: func(p *operation.SpinoffParams) { p.OccurredOn = date("2020-01-01") },
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := base
			c.edit(&p)
			_, _, err := svc.CreateSpinoff(f.ctx, f.spaceID, p)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to name the rule (%q)", err, c.want)
			}
		})
	}
}

// A spin-off dated years back is struck against that day's parcels; a later
// purchase is no part of it, and the end state would name a position the journal
// never had then.
func TestCreateSpinoffResolvesAgainstTheDayItTookEffect(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	carvedID := newPaper(t, f, "TUSD2", "Заблокированные активы «Вечный портфель USD»")

	for _, op := range []operation.Operation{{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2021-01-04"), Quantity: dec("100"), Price: dec("10"),
		AmountMinor: -100_000, Currency: "RUB",
	}, {
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2024-06-01"), Quantity: dec("900"), Price: dec("10"),
		AmountMinor: -900_000, Currency: "RUB",
	}} {
		if _, err := svc.Create(f.ctx, f.spaceID, op); err != nil {
			t.Fatalf("seed buy: %v", err)
		}
	}

	out, in, err := svc.CreateSpinoff(f.ctx, f.spaceID, operation.SpinoffParams{
		AccountID:        f.accountID,
		FromInstrumentID: f.sberID,
		ToInstrumentID:   carvedID,
		RatioFrom:        decimal.RequireFromString("1"),
		RatioTo:          decimal.RequireFromString("1"),
		BasisShare:       decimal.RequireFromString("0.5"),
		OccurredOn:       date("2023-12-22"),
		Source:           operation.SourceRegistry,
	})
	if err != nil {
		t.Fatalf("CreateSpinoff: %v", err)
	}

	// Half of the 100 000 held in 2023, not half of the 1 000 000 held today.
	if out.AmountMinor != 50_000 {
		t.Errorf("the spin-off moved %d, want 50000 — half of what was held on the day it took effect", out.AmountMinor)
	}
	if in.Quantity == nil || !in.Quantity.Equal(decimal.RequireFromString("100")) {
		t.Errorf("the arriving leg brings %v units, want 100 — one for every unit held on the day", in.Quantity)
	}

	// And the journal still replays with the later purchase in it.
	journal, err := f.store.ListForEngine(f.ctx, f.spaceID, f.accountID)
	if err != nil {
		t.Fatalf("ListForEngine: %v", err)
	}
	positions, err := portfolio.Compute(journal)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if positions[f.sberID].CostMinor != 950_000 {
		t.Errorf("the original keeps %d, want 950000", positions[f.sberID].CostMinor)
	}
	if positions[carvedID].CostMinor != 50_000 {
		t.Errorf("the carved-out paper carries %d, want 50000", positions[carvedID].CostMinor)
	}
}

// Р-16: a share of 0. The new fund arrives with no cost, the original keeps
// its whole basis, and the journal replays.
func TestCreateSpinoffWithNoShareKeepsTheWholeBasisOnTheOriginal(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	carvedID := newPaper(t, f, "TECH2", "Тинькофф Технологии заблокированные активы")
	if _, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2021-01-25"), Quantity: dec("100"), Price: dec("60"),
		AmountMinor: -600_000, Currency: "RUB",
	}); err != nil {
		t.Fatalf("seed buy: %v", err)
	}
	out, in, err := svc.CreateSpinoff(f.ctx, f.spaceID, operation.SpinoffParams{
		AccountID: f.accountID, FromInstrumentID: f.sberID, ToInstrumentID: carvedID,
		RatioFrom: decimal.RequireFromString("1"), RatioTo: decimal.RequireFromString("1"),
		BasisShare: decimal.Zero, OccurredOn: date("2023-12-22"), Source: operation.SourceRegistry,
	})
	if err != nil {
		t.Fatalf("CreateSpinoff with a share of 0: %v", err)
	}
	if out.AmountMinor != 0 || in.AmountMinor != 0 {
		t.Errorf("legs carry %d/%d, want 0 on both — nothing moves", out.AmountMinor, in.AmountMinor)
	}
	journal, err := f.store.ListForEngine(f.ctx, f.spaceID, f.accountID)
	if err != nil {
		t.Fatal(err)
	}
	positions, err := portfolio.Compute(journal)
	if err != nil {
		t.Fatalf("the journal does not replay: %v", err)
	}
	if p := positions[f.sberID]; p == nil || p.CostMinor != 600_000 {
		t.Errorf("the original's cost is %v, want all 600000", p)
	}
	if p := positions[carvedID]; p == nil || p.CostMinor != 0 || p.Quantity.String() != "100" {
		t.Errorf("the carved-out fund is %+v, want 100 units at no cost", p)
	}
}
