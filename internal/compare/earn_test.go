package compare

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/deposit"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/interest"
)

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

var (
	key14   = func(time.Time) (decimal.Decimal, bool, error) { return decimal.NewFromInt(14), true, nil }
	nothing = func(string, time.Time) (int, bool) { return 0, false }
)

func fixed(pct string, from, to int) deposit.Cell {
	return deposit.Cell{From: from, To: to, AmountFrom: 10_000_00, Rate: deposit.Rate{Percent: pct}}
}

// A savings account at 12 % on 1 000 000 ₽ for three months from 19 October
// 2026: its interest added each month's end, the last stretch paid on the
// horizon's day — 3 945,21, 9 901,93, 10 332,91, 6 397,62 ₽.
func TestASavingsAccountOverTheHorizon(t *testing.T) {
	p := deposit.Programme{ID: "s", Kind: deposit.KindSavings, Editions: []deposit.Edition{{From: "2026-01-01", Variants: []deposit.Variant{
		{ID: "v", Accrual: deposit.AccrualDaily, Payouts: []deposit.Payout{deposit.PayoutCapitalize}, Grid: []deposit.Cell{{Rate: deposit.Rate{Percent: "12"}}}},
	}}}}
	e, err := earn(p, p.Editions[0].Variants[0], 1_000_000_00, day("2026-10-19"), day("2027-01-19"), key14, nothing)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{3_945_21, 9_901_93, 10_332_91, 6_397_62}
	if len(e.Payments) != len(want) || e.Income != 30_577_67 || e.Days != 92 {
		t.Fatalf("earning = %+v", e)
	}
	for i, p := range e.Payments {
		if p.Minor != want[i] {
			t.Errorf("payment %d: %d, want %d", i, p.Minor, want[i])
		}
	}
}

// A deposit of 91 days at 15 %, 181 at 14 %, 367 at 13 %, interest at the
// end, on 1 000 000 ₽ for a year from 19 October 2026: 91 days prolonged
// three times bring the most a day — 158 191,54 ₽ over 364 days (181 days
// twice: 143 669,10 over 362; 367 days once: 130 712,33), 15,87 % a year.
func TestADepositsBestTermProlonged(t *testing.T) {
	v := deposit.Variant{
		ID: "v", Payouts: []deposit.Payout{deposit.PayoutEnd}, Ops: deposit.OpsNone, TermUnit: deposit.TermDays,
		Grid: []deposit.Cell{fixed("15", 91, 91), fixed("14", 181, 181), fixed("13", 367, 367)},
	}
	p := deposit.Programme{ID: "d", Kind: deposit.KindDeposit, Editions: []deposit.Edition{{From: "2026-01-01", Variants: []deposit.Variant{v}}}}
	e, err := earn(p, v, 1_000_000_00, day("2026-10-19"), day("2027-10-19"), key14, nothing)
	if err != nil {
		t.Fatal(err)
	}
	if e.Term != 91 || e.Prolonged != 3 || e.Income != 158_191_54 || e.Days != 364 || e.Payout != deposit.PayoutEnd {
		t.Errorf("earning = %+v", e)
	}
	if y := yearly(1_000_000_00, e); !y.Equal(decimal.RequireFromString("15.87")) {
		t.Errorf("a year: %s", y)
	}
	// A year's deposit of 367 days runs two days past the horizon: still
	// opened, its 367 days counted.
	only := deposit.Variant{
		ID: "y", Payouts: []deposit.Payout{deposit.PayoutEnd}, Ops: deposit.OpsNone, TermUnit: deposit.TermDays,
		Grid: []deposit.Cell{fixed("13", 367, 367)},
	}
	year := deposit.Programme{ID: "y", Kind: deposit.KindDeposit, Editions: []deposit.Edition{{From: "2026-01-01", Variants: []deposit.Variant{only}}}}
	if y, err := earn(year, only, 1_000_000_00, day("2026-10-19"), day("2027-10-19"), key14, nothing); err != nil || y.Term != 367 || y.Days != 367 {
		t.Errorf("367 days: %+v, %v", y, err)
	}
	// On 5 000 ₽ the bank has no rate: left out.
	if _, err := earn(p, v, 5_000_00, day("2026-10-19"), day("2027-10-19"), key14, nothing); err == nil {
		t.Error("a sum under the grid: earned")
	}
}

// Whom a variant is open to; how a condition is taken.
func TestAudiencesAndConditions(t *testing.T) {
	for _, c := range []struct {
		of, groups []deposit.Audience
		want       bool
	}{
		{[]deposit.Audience{deposit.AudienceAll}, nil, true},
		{[]deposit.Audience{deposit.AudiencePensioner}, nil, false},
		{[]deposit.Audience{deposit.AudiencePensioner}, []deposit.Audience{deposit.AudiencePensioner}, true},
		{[]deposit.Audience{deposit.AudienceNew, deposit.AudiencePensioner}, []deposit.Audience{deposit.AudiencePensioner}, false},
		{[]deposit.Audience{deposit.AudienceClosed}, []deposit.Audience{deposit.AudienceClosed}, false},
	} {
		if got := open(c.of, c.groups); got != c.want {
			t.Errorf("%v for %v: %v", c.of, c.groups, got)
		}
	}
	spend := Condition{ID: "spend", Check: deposit.CheckCardSpend, Levels: []deposit.Level{{From: 20_000_00, Plus: "1"}, {From: 50_000_00, Plus: "4"}}}
	spend.Now = told(spend, "vtb", nil, func(string, deposit.Check, time.Time) (int64, bool) { return 30_000_00, true }, day("2026-09-01"))
	for _, c := range []struct {
		choice Choice
		met    bool
		level  int
	}{
		{Choice{StateNow, -1}, true, 0},
		{Choice{StateYes, -1}, true, 1},
		{Choice{StateYes, 0}, true, 0},
		{Choice{StateNo, -1}, false, 0},
	} {
		if met, level := resolve(spend, c.choice); met != c.met || level != c.level {
			t.Errorf("%v: %v %d", c.choice, met, level)
		}
	}
	// A condition only a mark tells is not met «as now».
	sub := Condition{ID: "subscription", Check: deposit.CheckManual}
	sub.Now = told(sub, "vtb", nil, nil, day("2026-09-01"))
	if met, _ := resolve(sub, Choice{StateNow, -1}); met || sub.Now.Known {
		t.Errorf("a mark's condition as now: %+v", sub.Now)
	}
	// New to the bank: no account of the family there.
	fresh := Condition{ID: newClient, Requirement: true}
	if n := told(fresh, "vtb", map[string]bool{"sber": true}, nil, day("2026-09-01")); !n.Known || !n.Met {
		t.Errorf("new to ВТБ: %+v", n)
	}
	if n := told(fresh, "sber", map[string]bool{"sber": true}, nil, day("2026-09-01")); !n.Known || n.Met {
		t.Errorf("new to Сбер: %+v", n)
	}
}

// The tax a deposit adds: the person's interest this year 100 000, the free
// part 160 000; a deposit paying 150 000 this year adds 13 % of 90 000 — 11 700;
// paying 150 000 next year, where the scenario's key 10 % frees 100 000,
// adds 13 % of the 150 000 above it on top of the person's 100 000 — 19 500.
func TestTheTaxADepositAdds(t *testing.T) {
	tc := taxContext{
		known: true, rules: family.TaxRulesFor("RU"), year: 2026, other: 100_000_00, freeNow: 160_000_00,
		freeIn: func(int) int64 { return 100_000_00 },
	}
	this, ok := tc.added([]interest.Payment{{On: day("2026-12-20"), Minor: 150_000_00}})
	if !ok || this != 11_700_00 {
		t.Errorf("this year: %d", this)
	}
	next, _ := tc.added([]interest.Payment{{On: day("2027-12-20"), Minor: 150_000_00}})
	if next != 19_500_00 {
		t.Errorf("next year: %d", next)
	}
	if _, ok := (taxContext{}).added(nil); ok {
		t.Error("unknown: added")
	}
}
