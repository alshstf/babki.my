package taxyear

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/portfolio"
)

type fakeMoves map[uuid.UUID]portfolio.YearMoves

func (f fakeMoves) YearMoves(_ context.Context, _, id uuid.UUID, _ int) (portfolio.YearMoves, error) {
	return f[id], nil
}

// fakeDollar is the dollar at 80 on any day; no other currency.
type fakeDollar struct{}

func (fakeDollar) Rate(_ context.Context, from, to string, _ time.Time) (decimal.Decimal, time.Time, error) {
	if from == "USD" && to == "RUB" {
		return decimal.NewFromInt(80), time.Time{}, nil
	}
	return decimal.Zero, time.Time{}, marketdata.ErrNoRate
}

func (fakeDollar) RatesOn(context.Context, []marketdata.RateQuery) (marketdata.Rates, error) {
	return marketdata.Rates{}, nil
}

// The family's brokers abroad in 2026 (decision Р-45): the Kazakh one with
// $5 000 in — 400 000 ₽ at 80, within 600 000 in a country of the Union —
// needs no report; one somewhere else with $8 000 in — 640 000 ₽ — does; one
// with nothing in and $1 000 left is judged by that, free if its country is on
// the tax office's list; one in yuan cannot be told.
func TestTheReportOnAccountsAbroad(t *testing.T) {
	alex := member("Алекс", family.RoleOwner)
	kz, us, idle, cny := broker("Freedom KZ", &alex.ID, true), broker("IBKR", &alex.ID, true), broker("Спящий", &alex.ID, true), broker("Юани", &alex.ID, true)
	kz.Institution = "Freedom Finance Global"
	usd := func(start, in, out int64) []portfolio.CashMoves {
		return []portfolio.CashMoves{{Currency: "USD", Start: start, In: in, Out: out, End: start + in - out}}
	}
	moves := fakeMoves{
		kz.ID:   {Cash: usd(0, 500_000, 100_000), PapersEnd: map[string]int64{"USD": 300_000}},
		us.ID:   {Cash: usd(100_000, 800_000, 0)},
		idle.ID: {Cash: usd(100_000, 0, 0)},
		cny.ID:  {Cash: []portfolio.CashMoves{{Currency: "CNY", In: 100}}},
	}
	svc := NewService(fakeAccounts{kz, us, idle, cny}, fakeFacts{}, fakeSpaces{residency: "RU", members: []family.Member{alex}}, fakeKeys{}).
		WithReport(moves, fakeDollar{})
	got, err := svc.ReportOf(t.Context(), uuid.New(), 2026)
	if err != nil {
		t.Fatal(err)
	}
	if got.FreeUpTo != 600_000_00 || got.DueBy.Format(time.DateOnly) != "2027-06-01" || len(got.Accounts) != 4 {
		t.Fatalf("report = %+v", got)
	}
	verdicts := map[string]string{}
	for _, a := range got.Accounts {
		verdicts[a.Name] = a.Verdict
		if a.Name == "Freedom KZ" && (a.In != 400_000_00 || a.Country != "KZ") {
			t.Errorf("Freedom = %+v", a)
		}
		if a.Name == "Спящий" && (!a.NothingIn || a.In != 80_000_00) {
			t.Errorf("idle = %+v", a)
		}
	}
	want := map[string]string{"Freedom KZ": ReportFree, "IBKR": ReportDue, "Спящий": ReportFreeIfListed, "Юани": ReportUnknown}
	for name, v := range want {
		if verdicts[name] != v {
			t.Errorf("%s = %q, want %q", name, verdicts[name], v)
		}
	}

	// As the API writes it: the papers by currency with their codes.
	api := reportAPI(got)
	for _, a := range api.Accounts {
		if a.Name == "Freedom KZ" && (a.CountryCode != "398" || len(a.PapersEnd) != 1 || a.PapersEnd[0].CurrencyCode != "840" ||
			a.Cash[0].EndMinor != 400_000 || a.Cash[0].InThousands != "5.000" || a.PapersEnd[0].Thousands != "3.000" || a.Verdict != "free") {
			t.Errorf("API Freedom = %+v", a)
		}
	}
}
