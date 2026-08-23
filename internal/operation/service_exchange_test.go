package operation_test

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/portfolio"
)

// newPaper adds a second instrument to the fixture: a conversion needs one to
// convert INTO, and it must be a different row from the one being converted.
func newPaper(t *testing.T, f fixture, ticker, name string) (id [16]byte) {
	t.Helper()
	inst, err := instrument.NewStore(f.pool).Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: name, Ticker: ticker, Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("instrument %s: %v", ticker, err)
	}
	return inst.ID
}

// TestCreateExchangeCarriesTheParcelToTheNewPaper is the owner's own case, run
// through the service: four depositary receipts bought on two days in 2021
// become four shares of the company that redomiciled, and the sale that follows
// is measured against what was paid for the RECEIPTS (НК РФ ст. 214.1 п. 13).
//
// Before this type existed that sale was refused outright — the account held
// nothing of the new paper — which is what the owner saw on live data.
func TestCreateExchangeCarriesTheParcelToTheNewPaper(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	newID := newPaper(t, f, "TCSG", "ТКС Холдинг")

	for _, op := range []operation.Operation{{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2021-07-02"), Quantity: dec("2"), Price: dec("6414.60"),
		AmountMinor: -1_282_920, FeeMinor: 641, Currency: "RUB",
	}, {
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2021-07-05"), Quantity: dec("2"), Price: dec("6567.20"),
		AmountMinor: -1_313_440, FeeMinor: 328, Currency: "RUB",
	}} {
		if _, err := svc.Create(f.ctx, f.spaceID, op); err != nil {
			t.Fatalf("seed buy: %v", err)
		}
	}

	out, in, err := svc.CreateExchange(f.ctx, f.spaceID, operation.ExchangeParams{
		AccountID:        f.accountID,
		FromInstrumentID: f.sberID,
		ToInstrumentID:   newID,
		Quantity:         decimal.RequireFromString("4"),
		ToQuantity:       decimal.RequireFromString("4"),
		OccurredOn:       date("2024-02-27"),
		Source:           operation.SourceRegistry,
		Note:             "Конвертация расписок в акции",
	})
	if err != nil {
		t.Fatalf("CreateExchange: %v", err)
	}

	// Both legs carry the same money — what was paid — and it is the sum of the
	// two purchases including their capitalized commissions.
	const paid = 1_282_920 + 641 + 1_313_440 + 328
	if out.AmountMinor != paid || in.AmountMinor != paid {
		t.Errorf("legs carry %d/%d, want %d on both", out.AmountMinor, in.AmountMinor, paid)
	}
	if out.Type != operation.TypeExchangeOut || in.Type != operation.TypeExchangeIn {
		t.Errorf("types = %s/%s", out.Type, in.Type)
	}
	if out.TransferGroupID == nil || in.TransferGroupID == nil || *out.TransferGroupID != *in.TransferGroupID {
		t.Error("the legs do not share a transfer group")
	}
	// EACH LEG STORES ITS OWN BREAKDOWN, because they describe different
	// parcels. Read back from the database rather than from the response.
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
	pos, err := portfolio.Compute(journal)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if old := pos[f.sberID]; !old.Quantity.IsZero() || old.CostMinor != 0 {
		t.Errorf("the receipts left %s units / %d minor behind", old.Quantity, old.CostMinor)
	}
	got := pos[newID]
	if got == nil {
		t.Fatal("no position in the new paper")
	}
	if !got.Quantity.Equal(decimal.RequireFromString("4")) || got.CostMinor != paid {
		t.Errorf("new position = %s units / %d minor, want 4 / %d", got.Quantity, got.CostMinor, paid)
	}
	if len(got.Lots) != 2 {
		t.Fatalf("lots = %d, want the two purchases", len(got.Lots))
	}
	if got.Lots[0].AcquiredOn == nil || !got.Lots[0].AcquiredOn.Equal(date("2021-07-02")) {
		t.Errorf("first lot acquired %v, want 2021-07-02", got.Lots[0].AcquiredOn)
	}
	if got.Lots[1].AcquiredOn == nil || !got.Lots[1].AcquiredOn.Equal(date("2021-07-05")) {
		t.Errorf("second lot acquired %v, want 2021-07-05", got.Lots[1].AcquiredOn)
	}
}

// TestCreateExchangeRestatesUnitsAndKeepsTheMoney: 5 old units become 100 new
// ones, and the pieces must sum to 100 EXACTLY while carrying the same basis.
// The ratio 100/5 divides evenly; the next test takes one that does not.
func TestCreateExchangeRestatesUnitsAndKeepsTheMoney(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	newID := newPaper(t, f, "NEWP", "Новая бумага")

	for _, op := range []operation.Operation{{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2021-01-08"), Quantity: dec("3"), Price: dec("100"),
		AmountMinor: -30_000, Currency: "RUB",
	}, {
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2021-07-26"), Quantity: dec("2"), Price: dec("150"),
		AmountMinor: -30_000, Currency: "RUB",
	}} {
		if _, err := svc.Create(f.ctx, f.spaceID, op); err != nil {
			t.Fatalf("seed buy: %v", err)
		}
	}

	_, in, err := svc.CreateExchange(f.ctx, f.spaceID, operation.ExchangeParams{
		AccountID: f.accountID, FromInstrumentID: f.sberID, ToInstrumentID: newID,
		Quantity: decimal.RequireFromString("5"), ToQuantity: decimal.RequireFromString("100"),
		OccurredOn: date("2021-10-07"), Source: operation.SourceRegistry,
	})
	if err != nil {
		t.Fatalf("CreateExchange: %v", err)
	}
	if in.AmountMinor != 60_000 {
		t.Errorf("basis = %d, want 60000 — a conversion moves no money", in.AmountMinor)
	}
	if len(in.TransferLots) != 2 {
		t.Fatalf("pieces = %d, want 2", len(in.TransferLots))
	}
	// 3 → 60, 2 → 40, each keeping its own money and its own day.
	if !in.TransferLots[0].Quantity.Equal(decimal.RequireFromString("60")) {
		t.Errorf("first piece = %s units, want 60", in.TransferLots[0].Quantity)
	}
	if !in.TransferLots[1].Quantity.Equal(decimal.RequireFromString("40")) {
		t.Errorf("second piece = %s units, want 40", in.TransferLots[1].Quantity)
	}
	if in.TransferLots[0].CostMinor != 30_000 || in.TransferLots[1].CostMinor != 30_000 {
		t.Errorf("piece costs = %d/%d, want 30000/30000 — the money is not scaled",
			in.TransferLots[0].CostMinor, in.TransferLots[1].CostMinor)
	}
}

// TestCreateExchangeSumsToTheNewQuantityOnAnUnevenRatio is the arithmetic that
// makes the stored breakdown legal at all: three equal lots converted 3-for-1
// cannot each take a third of one unit, and the pieces must still sum to
// exactly what the row claims — or portfolio.CheckTransferLots refuses the
// account's positions on every later read.
func TestCreateExchangeSumsToTheNewQuantityOnAnUnevenRatio(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	newID := newPaper(t, f, "NEWP", "Новая бумага")

	for i, day := range []string{"2021-01-08", "2021-02-08", "2021-03-08"} {
		if _, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
			AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
			OccurredOn: date(day), Quantity: dec("1"), Price: dec("100"),
			AmountMinor: int64(-10_000 - i), Currency: "RUB",
		}); err != nil {
			t.Fatalf("seed buy %d: %v", i, err)
		}
	}

	_, in, err := svc.CreateExchange(f.ctx, f.spaceID, operation.ExchangeParams{
		AccountID: f.accountID, FromInstrumentID: f.sberID, ToInstrumentID: newID,
		Quantity: decimal.RequireFromString("3"), ToQuantity: decimal.RequireFromString("1"),
		OccurredOn: date("2022-06-06"), Source: operation.SourceRegistry,
	})
	if err != nil {
		t.Fatalf("CreateExchange: %v", err)
	}
	// 1/3 truncated to ten places is 0.3333333333; the running totals give
	// 0.3333333333, 0.3333333333 and — the last piece being pinned to the
	// quantity of the row — 0.3333333334.
	want := []string{"0.3333333333", "0.3333333333", "0.3333333334"}
	if len(in.TransferLots) != len(want) {
		t.Fatalf("pieces = %d, want %d", len(in.TransferLots), len(want))
	}
	sum := decimal.Zero
	for i, w := range want {
		if !in.TransferLots[i].Quantity.Equal(decimal.RequireFromString(w)) {
			t.Errorf("piece %d = %s, want %s", i, in.TransferLots[i].Quantity, w)
		}
		sum = sum.Add(in.TransferLots[i].Quantity)
	}
	if !sum.Equal(decimal.RequireFromString("1")) {
		t.Errorf("pieces sum to %s, want exactly 1", sum)
	}
	if in.AmountMinor != 30_003 {
		t.Errorf("basis = %d, want 30003 — every kopeck of the three purchases", in.AmountMinor)
	}
	// And the account still replays: the read path folds the STORED pieces.
	journal, err := f.store.ListForEngine(f.ctx, f.spaceID, f.accountID)
	if err != nil {
		t.Fatalf("ListForEngine: %v", err)
	}
	pos, err := portfolio.Compute(journal)
	if err != nil {
		t.Fatalf("Compute over the stored journal: %v", err)
	}
	if got := pos[newID]; got.CostMinor != 30_003 || !got.Quantity.Equal(decimal.RequireFromString("1")) {
		t.Errorf("stored position = %s / %d, want 1 / 30003", got.Quantity, got.CostMinor)
	}
}

func TestCreateExchangeRefusals(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	newID := newPaper(t, f, "NEWP", "Новая бумага")

	if _, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2021-01-08"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB",
	}); err != nil {
		t.Fatalf("seed buy: %v", err)
	}

	base := operation.ExchangeParams{
		AccountID: f.accountID, FromInstrumentID: f.sberID, ToInstrumentID: newID,
		Quantity: decimal.RequireFromString("10"), ToQuantity: decimal.RequireFromString("10"),
		OccurredOn: date("2022-01-01"), Source: operation.SourceRegistry,
	}
	for _, tc := range []struct {
		name   string
		mutate func(p *operation.ExchangeParams)
		want   string
	}{
		{"a source other than the registry", func(p *operation.ExchangeParams) {
			p.Source = "manual"
		}, "only written by source=registry"},
		{"the same paper on both sides", func(p *operation.ExchangeParams) {
			p.ToInstrumentID = p.FromInstrumentID
		}, "must differ"},
		{"more units than are held", func(p *operation.ExchangeParams) {
			p.Quantity = decimal.RequireFromString("11")
		}, "not enough quantity"},
		{"a paper the account never held", func(p *operation.ExchangeParams) {
			p.FromInstrumentID = newID
			p.ToInstrumentID = f.sberID
		}, "no history for the instrument being converted"},
		{"a zero new quantity", func(p *operation.ExchangeParams) {
			p.ToQuantity = decimal.Zero
		}, "must be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.mutate(&p)
			_, _, err := svc.CreateExchange(f.ctx, f.spaceID, p)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestExchangeLegsCannotBeTypedInByHand: the hand-entry path has no door for a
// conversion at all, and says why rather than falling through to a generic
// refusal.
func TestExchangeLegsCannotBeTypedInByHand(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	for _, typ := range []operation.Type{operation.TypeExchangeOut, operation.TypeExchangeIn} {
		_, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
			AccountID: f.accountID, InstrumentID: &f.sberID, Type: typ,
			OccurredOn: date("2022-01-01"), Quantity: dec("1"),
			AmountMinor: 0, Currency: "RUB",
		})
		if err == nil {
			t.Fatalf("%s was accepted through the hand-entry path", typ)
		}
		if !strings.Contains(err.Error(), "corporate-actions registry") {
			t.Errorf("%s: err = %v, want it to name the registry", typ, err)
		}
	}
}
