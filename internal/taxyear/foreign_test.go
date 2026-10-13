package taxyear

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/portfolio"
)

type fakeRows map[uuid.UUID]portfolio.YearRows

func (f fakeRows) YearRows(_ context.Context, _, id uuid.UUID, _ int, _ string, _ int) (portfolio.YearRows, error) {
	return f[id], nil
}

type fakePapers map[uuid.UUID]instrument.Instrument

func (f fakePapers) ByID(_ context.Context, id uuid.UUID) (instrument.Instrument, error) {
	return f[id], nil
}

// Alex's broker abroad in 2026 (decision Р-45): an Apple dividend of $10 at
// 85 with $3 withheld — 30 % abroad, of which only what the Russian 13 % takes,
// in whole roubles (111), offsets; a Eurobond's coupon as operations with
// securities; the broker's interest as interest from abroad; a sale — its
// proceeds Приложение 8's income, its fee and lots' cost the expenses; a sale
// with a lot of no day left out. The Russian broker is not here.
func TestTheForeignLines(t *testing.T) {
	alex := member("Алекс", family.RoleOwner)
	kz, ru := broker("Freedom KZ", &alex.ID, true), broker("Т-Банк", &alex.ID, false)
	apple, euro := uuid.New(), uuid.New()
	day := func(s string) time.Time { d, _ := time.Parse(time.DateOnly, s); return d }
	r85, r80 := decimal.NewFromInt(85), decimal.NewFromInt(80)
	rows := fakeRows{
		kz.ID: {
			Payouts: []portfolio.Payout{
				{On: day("2026-05-15"), Type: portfolio.TypeDividend, Instrument: &apple, Currency: "USD", Minor: 1000, Withheld: 300, Rate: r85, Known: true, InBase: 85_000, WithheldInBase: 25_500},
				{On: day("2026-06-01"), Type: portfolio.TypeCoupon, Instrument: &euro, Currency: "USD", Minor: 500, Rate: r85, Known: true, InBase: 42_500},
				{On: day("2026-06-30"), Type: portfolio.TypeInterest, Currency: "USD", Minor: 200, Known: false},
			},
			Sales: []portfolio.Sale{
				{
					On: day("2026-02-10"), Instrument: apple, Known: true, Rate: r80, ProceedsInBase: 14_400_000, FeeInBase: 16_000, Result: 2_984_000,
					Lots: []portfolio.SoldLot{{CostInBase: 9_000_000}, {CostInBase: 2_400_000}},
				},
				{On: day("2026-03-10"), Instrument: apple, Known: false, ProceedsInBase: 1_000_000},
			},
		},
		ru.ID: {Payouts: []portfolio.Payout{{Type: portfolio.TypeDividend, Minor: 100}}},
	}
	svc := NewService(fakeAccounts{ru, kz}, fakeFacts{}, fakeSpaces{residency: "RU", members: []family.Member{alex}}, fakeKeys{}).
		WithForeign(rows, fakePapers{apple: {Name: "Apple", ISIN: "US0378331005"}, euro: {Name: "Евробонд", ISIN: "XS1234567890"}})
	got, err := svc.ForeignOf(t.Context(), uuid.New(), 2026)
	if err != nil {
		t.Fatal(err)
	}
	if got.Currency != "RUB" || len(got.Accounts) != 1 {
		t.Fatalf("foreign = %+v", got)
	}
	a := got.Accounts[0]
	if a.ID != kz.ID || a.Person != alex.ID || len(a.Payouts) != 3 || len(a.Sales) != 2 {
		t.Fatalf("account = %+v", a)
	}
	if d := a.Payouts[0]; d.Kind != KindDividends || d.Paper != "Apple" || d.Country != "US" || d.TaxRU != 11_100 || d.Credit != 11_100 {
		t.Errorf("dividend = %+v", d)
	}
	if c := a.Payouts[1]; c.Kind != KindSecurities || c.Country != "XS" || c.Credit != 0 {
		t.Errorf("coupon = %+v", c)
	}
	if i := a.Payouts[2]; i.Kind != KindInterest || i.Paper != "" || i.TaxRU != 0 {
		t.Errorf("interest = %+v", i)
	}
	if a.Income != 14_400_000 || a.Expenses != 11_416_000 || a.Unvalued != 2 {
		t.Errorf("income %d, expenses %d, unvalued %d; want 14 400 000, 11 416 000 and 2", a.Income, a.Expenses, a.Unvalued)
	}

	// As the API writes it: the day's rate, none where it is missing; a sale
	// that cannot be valued has no result.
	api := foreignAPI(got).Accounts[0]
	if p := api.Payouts[0]; p.Rate.MustGet() != "85" || p.Type != "dividend" || p.Kind != "001" || p.On != "2026-05-15" || p.CreditMinor != 11_100 {
		t.Errorf("API dividend = %+v", p)
	}
	if !api.Payouts[2].Rate.IsNull() || !api.Payouts[2].InstrumentId.IsNull() {
		t.Errorf("API interest = %+v", api.Payouts[2])
	}
	if api.Sales[0].ResultBaseMinor.MustGet() != 2_984_000 || !api.Sales[1].ResultBaseMinor.IsNull() {
		t.Errorf("API sales = %+v", api.Sales)
	}
}
