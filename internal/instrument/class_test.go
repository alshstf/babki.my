package instrument_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/instrument"
)

// The broker's words brought to the program's list, as its passports read on
// 2026-10-13: a bond fund it files as "municipal" with focus "equity" goes by
// its name; gold by its focus; "other" is no sector.
func TestSectorFromTInvest(t *testing.T) {
	for _, c := range []struct {
		kind                instrument.Type
		name, sector, focus string
		want                instrument.Sector
	}{
		{instrument.TypeShare, "Сбербанк", "financial", "", instrument.SectorFinancials},
		{instrument.TypeShare, "Лукойл", "energy", "", instrument.SectorEnergy},
		{instrument.TypeShare, "Полюс", "materials", "", instrument.SectorMaterials},
		{instrument.TypeShare, "Кто-то", "other", "", instrument.SectorUnknown},
		{instrument.TypeBond, "ОФЗ 26238", "government", "", instrument.SectorGovernment},
		{instrument.TypeETF, "Альфа-Капитал Управляемые облигации", "municipal", "equity", instrument.SectorFundsBonds},
		{instrument.TypeETF, "Золото", "", "alternative_investment", instrument.SectorFundsGold},
		{instrument.TypeETF, "Крупнейшие компании РФ", "other", "equity", instrument.SectorFundsEquity},
		{instrument.TypeETF, "Вечный портфель", "", "mixed_allocation", instrument.SectorFundsMixed},
		{instrument.TypeETF, "Ликвидность", "", "money_market", instrument.SectorFundsMoney},
	} {
		if got := instrument.SectorFromTInvest(c.kind, c.name, c.sector, c.focus); got != c.want {
			t.Errorf("%s %q: %q, want %q", c.kind, c.name, got, c.want)
		}
	}
	if got := instrument.SectorFromYahoo("Consumer Defensive"); got != instrument.SectorConsumer {
		t.Errorf("yahoo: %q", got)
	}
}

// A class recorded, read back, replaced; what is due — the missing first,
// then the stale — and never a paper without an ISIN or of another kind.
func TestClassesAreStoredAndDue(t *testing.T) {
	st, ctx, _ := newStore(t)
	mk := func(kind instrument.Type, ticker, isin string) instrument.Instrument {
		i, err := st.Create(ctx, instrument.Instrument{Type: kind, Name: ticker, Ticker: ticker, ISIN: isin, Currency: "RUB"})
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	sber := mk(instrument.TypeShare, "SBER", "RU0009029540")
	fxus := mk(instrument.TypeETF, "FXUS", "IE00BD3QHZ91")
	mk(instrument.TypeShare, "NOISIN", "")
	mk(instrument.TypeCurrency, "USD000UTSTOM", "")

	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	if err := st.SetClass(ctx, fxus.ID, instrument.Class{Country: "us", Sector: instrument.SectorFundsEquity, Source: instrument.ClassFromTInvest, Raw: "other equity", FetchedAt: now.AddDate(0, -2, 0)}); err != nil {
		t.Fatal(err)
	}
	due, err := st.ToClassify(ctx, now.AddDate(0, -1, 0), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 2 || due[0].ID != sber.ID || due[1].ID != fxus.ID {
		t.Fatalf("due = %+v", due)
	}
	if err := st.SetClass(ctx, fxus.ID, instrument.Class{Country: "US", Sector: instrument.SectorFundsEquity, Source: instrument.ClassFromTInvest, FetchedAt: now}); err != nil {
		t.Fatal(err)
	}
	if due, _ := st.ToClassify(ctx, now.AddDate(0, -1, 0), 10); len(due) != 1 || due[0].ID != sber.ID {
		t.Errorf("due after refresh = %+v", due)
	}
	got, err := st.Classes(ctx, []uuid.UUID{sber.ID, fxus.ID})
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := got[fxus.ID]; len(got) != 1 || !ok || c.Country != "US" || c.Sector != instrument.SectorFundsEquity || !c.FetchedAt.Equal(now) {
		t.Errorf("classes = %+v", got)
	}
}
