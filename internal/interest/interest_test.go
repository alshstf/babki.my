package interest_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/interest"
)

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

// fixed is a rate that does not move: pct from from on, none before.
func fixed(pct string, from string) interest.RateOn {
	return func(d time.Time, _ int64) (decimal.Decimal, bool, error) {
		if d.Before(day(from)) {
			return decimal.Zero, false, nil
		}
		return decimal.RequireFromString(pct), true, nil
	}
}

func minors(ps []interest.Payment) []int64 {
	out := make([]int64, len(ps))
	for i, p := range ps {
		out[i] = p.Minor
	}
	return out
}

func equal(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestASavingsAccountEarnsEachMonthOnItsBalance(t *testing.T) {
	savings := interest.Pays{Monthly: true, Capitalize: true}
	// 1 000 000 ₽ at 14 % for October 2026: 1 000 000 × 0,14 × 31 / 365 =
	// 11 890,41 ₽; November earns on the balance with October's interest in.
	got, unrated, err := interest.Earned([]interest.Point{{On: day("2026-10-01"), Minor: 1_000_000_00}},
		fixed("14", "2026-01-01"), savings, day("2026-10-01"), day("2026-11-30"))
	if err != nil || unrated != nil {
		t.Fatalf("err %v, unrated %v", err, unrated)
	}
	if !equal(minors(got), []int64{1_189_041, 1_164_367}) || !got[0].On.Equal(day("2026-10-31")) || !got[1].On.Equal(day("2026-11-30")) {
		t.Errorf("payments %+v", got)
	}
	// Not capitalized, November earns on the million alone: × 30 / 365.
	got, _, _ = interest.Earned([]interest.Point{{On: day("2026-10-01"), Minor: 1_000_000_00}},
		fixed("14", "2026-01-01"), interest.Pays{Monthly: true}, day("2026-10-01"), day("2026-11-30"))
	if !equal(minors(got), []int64{1_189_041, 1_150_685}) {
		t.Errorf("not capitalized: %v", minors(got))
	}
}

func TestEachDayEarnsItsOwnRate(t *testing.T) {
	// 14 % to 15 October, 12 % from the 16th: 1 000 000 × (0,14 × 15 + 0,12 × 16) / 365.
	rate := func(d time.Time, _ int64) (decimal.Decimal, bool, error) {
		if d.Before(day("2026-10-16")) {
			return decimal.NewFromInt(14), true, nil
		}
		return decimal.NewFromInt(12), true, nil
	}
	got, _, _ := interest.Earned([]interest.Point{{On: day("2026-01-01"), Minor: 1_000_000_00}}, rate,
		interest.Pays{Monthly: true, Capitalize: true}, day("2026-10-01"), day("2026-10-31"))
	if !equal(minors(got), []int64{1_101_370}) {
		t.Errorf("payments %v", minors(got))
	}
	// A leap year's day is a 366th: February 2028 at 10 % — × 29 / 366.
	got, _, _ = interest.Earned([]interest.Point{{On: day("2028-01-01"), Minor: 1_000_000_00}}, fixed("10", "2028-01-01"),
		interest.Pays{Monthly: true}, day("2028-02-01"), day("2028-02-29"))
	if !equal(minors(got), []int64{792_350}) {
		t.Errorf("leap February: %v", minors(got))
	}
}

func TestADepositPaysAtItsTermsEnd(t *testing.T) {
	// 500 000 ₽ at 13 % for 91 days from 16 October 2026: the 17th to 15 January
	// both in (ГК ст. 839), 91 / 365 — 16 205,48 ₽ on 15 January.
	pays := interest.Pays{Ends: day("2027-01-15")}
	points := []interest.Point{{On: day("2026-10-16"), Minor: 500_000_00}}
	got, _, _ := interest.Earned(points, fixed("13", "2026-01-01"), pays, day("2026-10-17"), day("2027-12-31"))
	if !equal(minors(got), []int64{1_620_548}) || !got[0].On.Equal(day("2027-01-15")) {
		t.Errorf("payments %+v", got)
	}
	// By the year's end nothing is paid yet.
	got, _, _ = interest.Earned(points, fixed("13", "2026-01-01"), pays, day("2026-10-17"), day("2026-12-31"))
	if len(got) != 0 {
		t.Errorf("paid by 31 December: %+v", got)
	}
}

func TestADayWithMoneyAndNoRateStopsTheReckoning(t *testing.T) {
	// The rate was written in from 10 October: September earns nothing known.
	got, unrated, err := interest.Earned([]interest.Point{{On: day("2026-09-01"), Minor: 100_000_00}},
		fixed("12", "2026-10-10"), interest.Pays{Monthly: true}, day("2026-09-01"), day("2026-12-31"))
	if err != nil || unrated == nil || !unrated.Equal(day("2026-09-01")) || len(got) != 0 {
		t.Errorf("got %+v, unrated %v, err %v", got, unrated, err)
	}
	// No money, no rate asked: an empty account earns nothing and lacks nothing.
	got, unrated, _ = interest.Earned([]interest.Point{{On: day("2026-09-01"), Minor: 0}, {On: day("2026-10-10"), Minor: 100_000_00}},
		fixed("12", "2026-10-10"), interest.Pays{Monthly: true}, day("2026-09-01"), day("2026-10-31"))
	if unrated != nil || !equal(minors(got), []int64{72_329}) {
		t.Errorf("got %v, unrated %v", minors(got), unrated)
	}
}

func TestANewMarkOfTheBalanceHoldsTheInterestBeforeIt(t *testing.T) {
	// October's interest is capitalized, then a mark on 1 November says
	// 2 000 000 ₽: November earns on the mark, not on the mark plus October.
	points := []interest.Point{{On: day("2026-10-01"), Minor: 1_000_000_00}, {On: day("2026-11-01"), Minor: 2_000_000_00}}
	got, _, _ := interest.Earned(points, fixed("14", "2026-01-01"), interest.Pays{Monthly: true, Capitalize: true},
		day("2026-10-01"), day("2026-11-30"))
	// 2 000 000 × 0,14 × 30 / 365 = 23 013,70 ₽.
	if !equal(minors(got), []int64{1_189_041, 2_301_370}) {
		t.Errorf("payments %v", minors(got))
	}
}

type store struct {
	rates    map[uuid.UUID][]account.Rate
	bindings map[uuid.UUID]account.Binding
}

func (s store) AllRates(context.Context, uuid.UUID) (map[uuid.UUID][]account.Rate, error) {
	return s.rates, nil
}

func (s store) AllBindings(context.Context, uuid.UUID) (map[uuid.UUID]account.Binding, error) {
	return s.bindings, nil
}

func (s store) AllMarks(context.Context, uuid.UUID) (map[uuid.UUID][]account.Mark, error) {
	return nil, nil
}

func TestHowTheAccountsInterestIsPaid(t *testing.T) {
	term := func(n int) *int { return &n }
	byDays, byMonths, twoWays, none := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	rates, err := interest.Load(context.Background(), uuid.New(), store{bindings: map[uuid.UUID]account.Binding{
		byDays:   {Programme: "novikom-kapital", Variant: "rub", OpenedOn: day("2026-10-16"), Term: term(91)},
		byMonths: {Programme: "sber-sbervklad", Variant: "end", OpenedOn: day("2026-10-16"), Term: term(6)},
		// «СберВклад» monthly: paid onto a card or added to the sum — the
		// family's choice, not known here.
		twoWays: {Programme: "sber-sbervklad", Variant: "monthly", OpenedOn: day("2026-10-16"), Term: term(6)},
	}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		acc    account.Account
		ok     bool
		pays   interest.Pays
		opened string
	}{
		{"savings", account.Account{ID: none, Type: account.TypeSavings}, true, interest.Pays{Monthly: true, Capitalize: true}, ""},
		{"91 days", account.Account{ID: byDays, Type: account.TypeDeposit}, true, interest.Pays{Ends: day("2027-01-15")}, "2026-10-17"},
		{"6 months", account.Account{ID: byMonths, Type: account.TypeDeposit}, true, interest.Pays{Ends: day("2027-04-16")}, "2026-10-17"},
		{"two ways", account.Account{ID: twoWays, Type: account.TypeDeposit}, false, interest.Pays{}, ""},
		{"no programme", account.Account{ID: none, Type: account.TypeDeposit}, false, interest.Pays{}, ""},
		{"a current account", account.Account{ID: none, Type: account.TypeChecking}, false, interest.Pays{}, ""},
	}
	for _, c := range cases {
		pays, opened, ok := rates.Pays(c.acc)
		if ok != c.ok || pays != c.pays {
			t.Errorf("%s: %+v %v, want %+v %v", c.name, pays, ok, c.pays, c.ok)
		}
		if c.opened != "" && !opened.Equal(day(c.opened)) {
			t.Errorf("%s: earns from %s, want %s", c.name, opened, c.opened)
		}
	}
}

type keys struct{ asked int }

func (k *keys) KeyRateOn(context.Context, time.Time) (decimal.Decimal, bool, error) {
	k.asked++
	return decimal.NewFromInt(16), true, nil
}

func TestTheKeyRateOfADayIsReadOnce(t *testing.T) {
	// Key rate 16 % less 1,5: 1 000 000 ₽ × 0,145 × 31 / 365 for October.
	id := uuid.New()
	k := &keys{}
	rates, err := interest.Load(context.Background(), uuid.New(), store{rates: map[uuid.UUID][]account.Rate{
		id: {{From: day("2026-01-01"), Kind: account.RateKey, Percent: decimal.RequireFromString("-1.5"), ByHand: true}},
	}}, k, nil)
	if err != nil {
		t.Fatal(err)
	}
	points := []interest.Point{{On: day("2026-10-01"), Minor: 1_000_000_00}}
	for range 2 {
		got, unrated, err := interest.Earned(points, rates.Of(id), interest.Pays{Monthly: true}, day("2026-10-01"), day("2026-10-31"))
		if err != nil || unrated != nil || !equal(minors(got), []int64{1_231_507}) {
			t.Fatalf("got %v, unrated %v, err %v", minors(got), unrated, err)
		}
	}
	if k.asked != 31 {
		t.Errorf("the key rate was read %d times for 31 days", k.asked)
	}
}
