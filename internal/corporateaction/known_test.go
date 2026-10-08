package corporateaction_test

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/corporateaction"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/portfolio"
)

// Each known conversion is an exchange's one-for-one replacement of a foreign
// receipt by a Russian share, with the notice that says so.
func TestEveryKnownConversionIsAOneForOneReplacementWithItsNotice(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range corporateaction.KnownConversions {
		if err := e.Validate(); err != nil {
			t.Errorf("%s: %v", e.ISIN, err)
		}
		if e.Kind != corporateaction.KindConversion || e.RatioFrom != 1 || e.RatioTo != 1 || e.Source != corporateaction.SourceKnown {
			t.Errorf("%s = %+v, want a one-for-one conversion from the known list", e.ISIN, e)
		}
		if !instrument.ForeignISIN(e.ISIN) || !strings.HasPrefix(e.ResultISIN, "RU") {
			t.Errorf("%s → %s, want a foreign receipt replaced by a Russian share", e.ISIN, e.ResultISIN)
		}
		if !strings.HasPrefix(e.SourceRef, "https://www.moex.com/n") {
			t.Errorf("%s: evidence %q, want the exchange's notice", e.ISIN, e.SourceRef)
		}
		if seen[e.ISIN] {
			t.Errorf("%s is listed twice", e.ISIN)
		}
		seen[e.ISIN] = true
	}
}

// tcsReceipt and tcsShare: the exchange replaced TCS Group's receipts by
// МКПАО «ТКС Холдинг» shares on 2024-02-27.
const (
	tcsReceipt = "US87238U2033"
	tcsShare   = "RU000A107UL4"
)

// A receipt bought for roubles becomes the Russian share on the notice's day,
// so a later sale of the share finds it. One bought for dollars at a foreign
// broker stayed a receipt: the exchange replaced only what Russian
// depositories held.
func TestAKnownConversionReplacesOnlyAReceiptBoughtForRoubles(t *testing.T) {
	f := newFixture(t)
	catalog := instrument.NewStore(f.pool)
	receipt, err := catalog.Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "ГДР TCS Group Holding ORD SHS", Ticker: "TCS-ME", ISIN: tcsReceipt, Currency: "RUB",
	})
	if err != nil {
		t.Fatal(err)
	}
	share, err := catalog.Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Т-Технологии", Ticker: "T", ISIN: tcsShare, Currency: "RUB",
	})
	if err != nil {
		t.Fatal(err)
	}
	buy := func(accountID uuid.UUID, currency string, amount int64) {
		t.Helper()
		if _, err := f.svc.Create(f.ctx, f.spaceID, operation.Operation{
			AccountID: accountID, InstrumentID: &receipt.ID, Type: operation.TypeBuy,
			OccurredOn: date("2021-06-01"), Quantity: dec("10"), AmountMinor: amount, Currency: currency,
		}); err != nil {
			t.Fatal(err)
		}
	}
	buy(f.accountID, "RUB", -6_000_000)
	buy(f.otherID, "USD", -80_000)

	w := corporateaction.NewRecordKnownConversionsWorker(f.store, f.materializer, catalog, slog.Default())
	if err := w.Work(f.ctx, &river.Job[corporateaction.RecordKnownConversionsArgs]{JobRow: &rivertype.JobRow{ID: 1}}); err != nil {
		t.Fatal(err)
	}

	holding := func(accountID, paper uuid.UUID) decimal.Decimal {
		t.Helper()
		journal, err := f.ops.ListForEngine(f.ctx, f.spaceID, accountID)
		if err != nil {
			t.Fatal(err)
		}
		positions, err := portfolio.Compute(journal)
		if err != nil {
			t.Fatal(err)
		}
		if p, ok := positions[paper]; ok {
			return p.Quantity
		}
		return decimal.Zero
	}
	if r, s := holding(f.accountID, receipt.ID), holding(f.accountID, share.ID); !r.IsZero() || !s.Equal(decimal.NewFromInt(10)) {
		t.Errorf("rouble account holds %s receipts and %s shares, want 0 and 10", r, s)
	}
	if r, s := holding(f.otherID, receipt.ID), holding(f.otherID, share.ID); !r.Equal(decimal.NewFromInt(10)) || !s.IsZero() {
		t.Errorf("dollar account holds %s receipts and %s shares, want 10 and 0: a receipt abroad was not replaced", r, s)
	}

	// The share can now be sold where it is held.
	if _, err := f.svc.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, InstrumentID: &share.ID, Type: operation.TypeSell,
		OccurredOn: date("2024-10-15"), Quantity: dec("10"), AmountMinor: 2_500_000, Currency: "RUB",
	}); err != nil {
		t.Errorf("selling the replaced shares was refused: %v", err)
	}
}

// Each known spin-off is a fund's move of its blocked assets into a new fund:
// one unit for one, none of the cost, the manager's notice as evidence, and the
// new fund's catalog row ready to file.
func TestEveryKnownSpinOffIsAFundsOneForOneSpinOffAtNoCost(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range corporateaction.KnownSpinOffs {
		e := s.Event
		if err := e.Validate(); err != nil {
			t.Errorf("%s: %v", e.ISIN, err)
		}
		if e.Kind != corporateaction.KindSpinOff || e.RatioFrom != 1 || e.RatioTo != 1 ||
			e.BasisShare == nil || !e.BasisShare.IsZero() || e.Source != corporateaction.SourceKnown {
			t.Errorf("%s = %+v, want a one-for-one spin-off at no cost from the known list", e.ISIN, e)
		}
		if !strings.HasPrefix(e.SourceRef, "https://cdn.t-capital-funds.ru/") {
			t.Errorf("%s: evidence %q, want the management company's notice", e.ISIN, e.SourceRef)
		}
		if p := s.Paper; p.ISIN != e.ResultISIN || p.Type != instrument.TypeETF || p.Ticker == "" || p.Name == "" || p.Currency != "RUB" {
			t.Errorf("%s: new paper %+v, want the fund %s with its ticker and name", e.ISIN, p, e.ResultISIN)
		}
		if seen[e.ISIN] {
			t.Errorf("%s is listed twice", e.ISIN)
		}
		seen[e.ISIN] = true
	}
}

// techFund and techBlocked: T-Capital moved TECH's blocked assets into TECH2
// for the holders on its list of 2023-10-16.
const (
	techFund    = "RU000A101X68"
	techBlocked = "RU000A1071G8"
)

// A holder on the list date gets one TECH2 per TECH from the next day, at no
// cost, whichever currency the fund was bought for; the new fund is catalogued
// for them, since no operation brings it. A purchase after the list date gets
// nothing.
func TestAKnownSpinOffCataloguesTheNewFundAndReachesEveryHolder(t *testing.T) {
	f := newFixture(t)
	catalog := instrument.NewStore(f.pool)
	tech, err := catalog.Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeETF, Name: "Технологии Америки", Ticker: "TECH", ISIN: techFund, Currency: "RUB",
	})
	if err != nil {
		t.Fatal(err)
	}
	buy := func(accountID uuid.UUID, on, quantity, currency string, amount int64) {
		t.Helper()
		if _, err := f.svc.Create(f.ctx, f.spaceID, operation.Operation{
			AccountID: accountID, InstrumentID: &tech.ID, Type: operation.TypeBuy,
			OccurredOn: date(on), Quantity: dec(quantity), AmountMinor: amount, Currency: currency,
		}); err != nil {
			t.Fatal(err)
		}
	}
	buy(f.accountID, "2022-03-01", "100", "RUB", -6_000_000)
	buy(f.accountID, "2023-10-18", "5", "RUB", -250_000)
	buy(f.otherID, "2021-06-01", "40", "USD", -40_000)

	w := corporateaction.NewRecordKnownConversionsWorker(f.store, f.materializer, catalog, slog.Default())
	if err := w.Work(f.ctx, &river.Job[corporateaction.RecordKnownConversionsArgs]{JobRow: &rivertype.JobRow{ID: 1}}); err != nil {
		t.Fatal(err)
	}

	blocked, err := catalog.ByISIN(f.ctx, techBlocked)
	if err != nil || blocked.Ticker != "TECH2" || blocked.Type != instrument.TypeETF {
		t.Fatalf("the new fund in the catalog: %+v, %v; want TECH2", blocked, err)
	}
	position := func(accountID, paper uuid.UUID) *portfolio.Position {
		t.Helper()
		journal, err := f.ops.ListForEngine(f.ctx, f.spaceID, accountID)
		if err != nil {
			t.Fatal(err)
		}
		positions, err := portfolio.Compute(journal)
		if err != nil {
			t.Fatal(err)
		}
		if p, ok := positions[paper]; ok {
			return p
		}
		return &portfolio.Position{}
	}
	for _, c := range []struct {
		name          string
		account       uuid.UUID
		fund, blocked int64
		fundCost      int64
	}{
		{"rouble account", f.accountID, 105, 100, 6_250_000},
		{"dollar account", f.otherID, 40, 40, 40_000},
	} {
		fund, spun := position(c.account, tech.ID), position(c.account, blocked.ID)
		if !fund.Quantity.Equal(decimal.NewFromInt(c.fund)) || fund.CostMinor != c.fundCost {
			t.Errorf("%s: TECH %s at cost %d, want %d at %d: the fund stays with all its cost",
				c.name, fund.Quantity, fund.CostMinor, c.fund, c.fundCost)
		}
		if !spun.Quantity.Equal(decimal.NewFromInt(c.blocked)) || spun.CostMinor != 0 {
			t.Errorf("%s: TECH2 %s at cost %d, want %d at no cost", c.name, spun.Quantity, spun.CostMinor, c.blocked)
		}
	}

	journal, err := f.ops.ListForEngine(f.ctx, f.spaceID, f.accountID)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range journal {
		if o.Type == operation.TypeSpinoffIn && (!o.OccurredOn.Equal(date("2023-10-17")) ||
			!strings.Contains(o.Note, "раскрытие управляющей компании фонда")) {
			t.Errorf("the arriving row: %s, %q; want 2023-10-17 and the manager's notice named", o.OccurredOn.Format(time.DateOnly), o.Note)
		}
	}
}
