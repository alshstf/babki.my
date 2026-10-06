package corporateaction_test

import (
	"log/slog"
	"strings"
	"testing"

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

	w := corporateaction.NewRecordKnownConversionsWorker(f.store, f.materializer, slog.Default())
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
