package allocation

import (
	"testing"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/structure"
)

func pct(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// The note's family (Р-37): 3 000 000 to share out, shares above their 30 %,
// bonds below theirs; gold targeted but not held; crypto held but not
// targeted; the kinds in their order, the rest after.
func TestTheWorthIsSetAgainstTheTargets(t *testing.T) {
	s := structure.Structure{
		BaseCurrency: "RUB", Targets: 3_000_000_00, Everyday: 200_000_00,
		ByKind: []structure.Slice{
			{Key: "shares", Minor: 1_050_000_00},
			{Key: "bonds", Minor: 600_000_00},
			{Key: "deposits", Minor: 600_000_00},
			{Key: "funds", Minor: 450_000_00},
			{Key: "broker_cash", Minor: 150_000_00},
			{Key: "crypto", Minor: 150_000_00},
		},
		ByTargetCurrency: []structure.Slice{{Key: "USD", Minor: 900_000_00}, {Key: "RUB", Minor: 2_100_000_00}},
	}
	v := view(s, ModeTopUp,
		[]Target{{"shares", pct("30")}, {"bonds", pct("30")}, {"funds", pct("15")}, {"deposits", pct("15")}, {"metals", pct("10")}},
		[]Target{{"RUB", pct("60")}, {"USD", pct("25")}, {"CNY", pct("15")}})
	var keys []string
	got := map[string]Row{}
	for _, r := range v.Kinds {
		keys = append(keys, r.Key)
		got[r.Key] = r
	}
	want := []string{"shares", "bonds", "funds", "metals", "deposits", "crypto", "broker_cash"}
	if len(keys) != len(want) {
		t.Fatalf("kinds %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("kinds %v, want %v", keys, want)
		}
	}
	if r := got["shares"]; r.Share.String() != "35" || r.Gap.String() != "5" || *r.Off != 150_000_00 {
		t.Errorf("shares = %+v", r)
	}
	if r := got["bonds"]; r.Gap.String() != "-10" || *r.Off != -300_000_00 {
		t.Errorf("bonds = %+v", r)
	}
	if r := got["metals"]; r.Minor != 0 || r.Share.String() != "0" || *r.Off != -300_000_00 {
		t.Errorf("metals, targeted not held = %+v", r)
	}
	if r := got["crypto"]; r.Target != nil || r.Share.String() != "5" {
		t.Errorf("crypto, held not targeted = %+v", r)
	}
	if v.Currencies[0].Key != "RUB" || v.Currencies[2].Key != "CNY" || !v.Currencies[1].Gap.Equal(pct("5")) {
		t.Errorf("currencies = %+v", v.Currencies)
	}
	if v.Worth != 3_000_000_00 || v.Everyday != 200_000_00 {
		t.Errorf("worth %d, everyday %d", v.Worth, v.Everyday)
	}
}

// Targets are none, or each key once and 100 % in all, to hundredths.
func TestTargetsAddUpToAHundred(t *testing.T) {
	ok := []Target{{"shares", pct("33.33")}, {"bonds", pct("33.33")}, {"funds", pct("33.34")}}
	if err := validTargets(ByKind, ok); err != nil {
		t.Errorf("valid targets refused: %v", err)
	}
	if err := validTargets(ByKind, nil); err != nil {
		t.Errorf("no targets refused: %v", err)
	}
	for name, bad := range map[string][]Target{
		"short of 100":  {{"shares", pct("60")}, {"bonds", pct("30")}},
		"unknown kind":  {{"stamps", pct("100")}},
		"twice":         {{"shares", pct("50")}, {"shares", pct("50")}},
		"thousandths":   {{"shares", pct("33.333")}, {"bonds", pct("66.667")}},
		"below zero":    {{"shares", pct("-10")}, {"bonds", pct("110")}},
		"everyday kind": {{"everyday", pct("100")}},
	} {
		if err := validTargets(ByKind, bad); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := validTargets(ByCurrency, []Target{{"rub", pct("100")}}); err == nil {
		t.Error("a lower-case currency accepted")
	}
	if err := validTargets(ByCurrency, []Target{{"RUB", pct("70")}, {"CNY", pct("30")}}); err != nil {
		t.Errorf("currency targets refused: %v", err)
	}
}
