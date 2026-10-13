package allocation

import (
	"testing"

	"github.com/google/uuid"
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

// The note's top-up (Р-37, variant Б): 100 000 to the kinds below their
// target in 3 100 000 — bonds lack 330 000, gold 160 000, funds 15 000 — in
// that proportion; shares, deposits and the brokers' money above their
// targets get nothing.
func TestATopUpGoesWhereTheWorthIsShort(t *testing.T) {
	s := structure.Structure{
		BaseCurrency: "RUB", Targets: 3_000_000_00,
		ByKind: []structure.Slice{
			{Key: "shares", Minor: 1_050_000_00},
			{Key: "bonds", Minor: 600_000_00},
			{Key: "funds", Minor: 450_000_00},
			{Key: "deposits", Minor: 600_000_00},
			{Key: "metals", Minor: 150_000_00},
			{Key: "broker_cash", Minor: 150_000_00},
		},
	}
	v := view(s, ModeTopUp, []Target{
		{"shares", pct("30")}, {"bonds", pct("30")}, {"funds", pct("15")}, {"deposits", pct("15")}, {"metals", pct("10")}, {"broker_cash", pct("0")},
	}, nil)
	buys, err := TopUp(v, 100_000_00, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Change{}
	var total int64
	for _, b := range buys {
		got[b.Key] = b
		total += b.Minor
	}
	want := map[string]int64{"bonds": 65_346_55, "metals": 31_683_16, "funds": 2_970_29, "shares": 0, "deposits": 0, "broker_cash": 0}
	for k, w := range want {
		if got[k].Minor != w {
			t.Errorf("%s: %d, want %d", k, got[k].Minor, w)
		}
	}
	if total != 100_000_00 || len(buys) != 6 {
		t.Errorf("put in %d over %d kinds, want all 100 000 over 6", total, len(buys))
	}
	if got["bonds"].ShareAfter.String() != "21.46" || got["shares"].ShareAfter.String() != "33.87" {
		t.Errorf("shares after: bonds %s, shares %s", got["bonds"].ShareAfter, got["shares"].ShareAfter)
	}
	if _, err := TopUp(v, 0, 0); err == nil {
		t.Error("a top-up of nothing accepted")
	}
	if _, err := TopUp(view(s, ModeTopUp, nil, nil), 100, 0); err == nil {
		t.Error("a top-up without targets accepted")
	}

	// The brokers' 150 000 free put in too: it leaves the brokers' free
	// money and goes where the worth is short; the worth stays 3 100 000.
	moved, err := TopUp(v, 100_000_00, 150_000_00)
	if err != nil {
		t.Fatal(err)
	}
	got, total = map[string]Change{}, 0
	for _, c := range moved {
		got[c.Key] = c
		total += c.Minor
	}
	if got["broker_cash"].Minor != -150_000_00 || got["broker_cash"].ShareAfter.String() != "0" || total != 100_000_00 {
		t.Errorf("brokers' money = %+v, net %d; want -150 000 to nothing, 100 000 net", got["broker_cash"], total)
	}
	if got["bonds"].Minor+got["metals"].Minor+got["funds"].Minor != 250_000_00 || got["shares"].Minor != 0 {
		t.Errorf("moved = %+v", got)
	}
	if _, err := TopUp(v, 0, 200_000_00); err == nil {
		t.Error("more than the brokers hold free accepted")
	}
}

// A target is approached from either side; a ceiling only matters above it;
// a hundredth of a point is no move.
func TestVerdict(t *testing.T) {
	d := decimal.RequireFromString
	for _, c := range []struct {
		before, after, target string
		ceiling               bool
		want                  Verdict
	}{
		{"35", "31", "30", false, Closer},
		{"25", "29", "30", false, Closer},
		{"25", "35", "30", false, Same},
		{"30", "40", "30", false, Further},
		{"25", "28", "20", true, Further},
		{"28", "22", "20", true, Closer},
		{"10", "18", "20", true, Same},
		{"30.004", "30.001", "20", true, Same},
	} {
		if got := verdict(d(c.before), d(c.after), d(c.target), c.ceiling); got != c.want {
			t.Errorf("%s → %s against %s (ceiling %v): %s, want %s", c.before, c.after, c.target, c.ceiling, got, c.want)
		}
	}
}

// A plan that raises what is over the insurance limit is said; one that does
// not, or at a bank the catalog does not know, is not.
func TestUninsured(t *testing.T) {
	maria, alex := uuid.New(), uuid.New()
	before := []structure.InsuranceRow{
		{Person: maria, Bank: "tbank", Held: 1_300_000_00, Insured: 1_300_000_00, Known: true},
		{Person: alex, Bank: "sber", Held: 1_500_000_00, Insured: 1_400_000_00, Over: 100_000_00, Known: true},
	}
	after := []structure.InsuranceRow{
		{Person: maria, Bank: "tbank", Held: 1_800_000_00, Insured: 1_400_000_00, Over: 400_000_00, Known: true},
		{Person: alex, Bank: "sber", Held: 1_450_000_00, Insured: 1_400_000_00, Over: 50_000_00, Known: true},
		{Person: alex, Bank: "-", Held: 2_000_000_00},
	}
	got := uninsured(before, after)
	if len(got) != 1 || got[0].Person != maria || got[0].Before != 0 || got[0].After != 400_000_00 {
		t.Errorf("uninsured = %+v", got)
	}
}
