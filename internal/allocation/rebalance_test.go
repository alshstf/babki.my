package allocation

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/portfolio"
	"babki.my/babki/internal/structure"
)

// noteFamily is the family of the note (Р-37): 3 000 000 to share out by
// shares 30, bonds 30, funds 15, metals 10, deposits 15, brokers' money 0.
func noteFamily(unsellable map[string]int64) View {
	s := structure.Structure{
		BaseCurrency: "RUB", Targets: 3_000_000_00,
		ByKind: []structure.Slice{
			{Key: "shares", Minor: 1_050_000_00},
			{Key: "bonds", Minor: 600_000_00},
			{Key: "deposits", Minor: 600_000_00},
			{Key: "funds", Minor: 450_000_00},
			{Key: "broker_cash", Minor: 300_000_00},
		},
	}
	for k, v := range unsellable {
		s.Unsellable = append(s.Unsellable, structure.Slice{Key: k, Minor: v})
	}
	return view(s, ModeRebalance, []Target{
		{"shares", pct("30")},
		{"bonds", pct("30")},
		{"funds", pct("15")},
		{"metals", pct("10")},
		{"deposits", pct("15")},
		{"broker_cash", pct("0")},
	}, nil)
}

func movesByKey(moves []Move) map[string]Move {
	out := map[string]Move{}
	for _, m := range moves {
		out[m.Key] = m
	}
	return out
}

// Shares sell 150 000 down to their target and the brokers' money goes in;
// the deposits are above theirs by 150 000, to take as they end, not now —
// so the 450 000 at hand buys bonds and gold half each of the 600 000 they
// lack.
func TestRebalanceSellsDownToTheTargetsAndBuysWithIt(t *testing.T) {
	moves, err := Rebalance(noteFamily(nil), 0)
	if err != nil {
		t.Fatal(err)
	}
	got := movesByKey(moves)
	want := map[string]Move{
		"shares":      {Minor: -150_000_00, ShareAfter: pct("30")},
		"bonds":       {Minor: 225_000_00, ShareAfter: pct("27.5")},
		"metals":      {Minor: 225_000_00, ShareAfter: pct("7.5")},
		"funds":       {Minor: 0, ShareAfter: pct("15")},
		"deposits":    {Minor: 0, Later: 150_000_00, ShareAfter: pct("20")},
		"broker_cash": {Minor: -300_000_00, ShareAfter: pct("0")},
	}
	for k, w := range want {
		g := got[k]
		if g.Minor != w.Minor || g.Later != w.Later || g.Kept != w.Kept || !g.ShareAfter.Equal(w.ShareAfter) {
			t.Errorf("%s = %+v, want %+v", k, g, w)
		}
	}
	var sum int64
	for _, m := range moves {
		sum += m.Minor
	}
	if sum != 0 {
		t.Errorf("bought and sold do not meet: %d", sum)
	}
}

// New money goes in with the sales; with every excess sold, each kind lands
// on its target exactly.
func TestRebalanceTakesNewMoneyToo(t *testing.T) {
	v := noteFamily(nil)
	// Deposits at their target, so nothing is held back.
	for i := range v.Kinds {
		if v.Kinds[i].Key == "deposits" {
			v.Kinds[i].Minor = 450_000_00
		}
	}
	v.Worth = 2_850_000_00
	moves, err := Rebalance(v, 150_000_00)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range moves {
		r := v.Kinds[0]
		for _, k := range v.Kinds {
			if k.Key == m.Key {
				r = k
			}
		}
		if r.Target != nil && !m.ShareAfter.Equal(*r.Target) {
			t.Errorf("%s ends at %s %%, want its target %s", m.Key, m.ShareAfter, r.Target)
		}
	}
}

// A frozen paper valued in full cannot be sold: of the shares' 150 000
// above the target only what is not frozen goes, the rest is kept, and the
// kinds below get less.
func TestRebalanceKeepsWhatCannotBeSold(t *testing.T) {
	moves, err := Rebalance(noteFamily(map[string]int64{"shares": 1_000_000_00}), 0)
	if err != nil {
		t.Fatal(err)
	}
	got := movesByKey(moves)
	if s := got["shares"]; s.Minor != -50_000_00 || s.Kept != 100_000_00 {
		t.Errorf("shares = %+v, want 50 000 sold and 100 000 kept", s)
	}
	if b, m := got["bonds"], got["metals"]; b.Minor+m.Minor != 350_000_00 {
		t.Errorf("bought %d, want the 350 000 at hand", b.Minor+m.Minor)
	}
}

func TestRebalanceRefuses(t *testing.T) {
	if _, err := Rebalance(noteFamily(nil), -1); err == nil {
		t.Error("new money below zero taken")
	}
	v := noteFamily(nil)
	for i := range v.Kinds {
		v.Kinds[i].Target = nil
	}
	if _, err := Rebalance(v, 0); err == nil {
		t.Error("no targets taken")
	}
}

func day(s string) *time.Time {
	d, _ := time.Parse(time.DateOnly, s)
	return &d
}

// 100 shares worth 10 500 each: 50 bought in 2020 at 4 000, 50 in 2025 at
// 8 000.
func twoLots() portfolio.SaleLots {
	worth := int64(1_050_000_00)
	return portfolio.SaleLots{
		InstrumentID: uuid.New(), Type: instrument.TypeShare, Quantity: decimal.NewFromInt(100), LiquidInBase: &worth,
		Lots: []portfolio.HeldLot{
			{Quantity: decimal.NewFromInt(50), AcquiredOn: day("2020-01-15"), CostInBase: 200_000_00, CostKnown: true},
			{Quantity: decimal.NewFromInt(50), AcquiredOn: day("2025-06-01"), CostInBase: 400_000_00, CostKnown: true},
		},
	}
}

var russia = family.TaxRulesFor("RU")

// The earliest lots go first: a seventh of the shares comes out of the
// 2020 lot alone, held over three years, so it gains free of tax.
func TestTheEstimateFreesLotsHeldLongEnough(t *testing.T) {
	today := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	est := Estimating([]Move{{Key: "shares", Minor: -150_000_00}}, []portfolio.SaleLots{twoLots()}, russia, today)
	s := est.Sales["shares"]
	// 14,2857 shares: 150 000 for what cost 57 142,86.
	if !s.Known || s.Gain != 9_285_714 || s.Free != 9_285_714 || est.Tax == nil || *est.Tax != 0 {
		t.Errorf("sale = %+v, tax %v", s, est.Tax)
	}
}

// Six sevenths: the whole 2020 lot (325 000 free of tax) and 35,71 shares of
// the 2025 one, 375 000 for 285 714,29 — 13 % of 89 285,71 in tax.
func TestTheEstimateTaxesTheRecentLots(t *testing.T) {
	today := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	est := Estimating([]Move{{Key: "shares", Minor: -900_000_00}}, []portfolio.SaleLots{twoLots()}, russia, today)
	s := est.Sales["shares"]
	if !s.Known || s.Gain != 41_428_571 || s.Free != 32_500_000 || est.Tax == nil || *est.Tax != 1_160_714 {
		t.Errorf("sale = %+v, tax %v", s, est.Tax)
	}
	// Exactly three years is not over three: the 2020 lot bought on this day
	// in 2023 would be taxed.
	h := twoLots()
	h.Lots[0].AcquiredOn = day("2023-10-13")
	est = Estimating([]Move{{Key: "shares", Minor: -150_000_00}}, []portfolio.SaleLots{h}, russia, today)
	if s := est.Sales["shares"]; s.Free != 0 || *est.Tax != 1_207_143 {
		t.Errorf("held three years to the day: %+v, tax %d", s, *est.Tax)
	}
}

// A loss on a lot held long still offsets: the 2020 lot bought at 12 000
// sells at a loss of 75 000, which takes that much off the 89 285,71 the
// 2025 one gains.
func TestTheEstimateOffsetsALossOnALotHeldLong(t *testing.T) {
	today := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	h := twoLots()
	h.Lots[0].CostInBase = 600_000_00
	est := Estimating([]Move{{Key: "shares", Minor: -900_000_00}}, []portfolio.SaleLots{h}, russia, today)
	if s := est.Sales["shares"]; s.Gain != 1_428_571 || s.Free != 0 || *est.Tax != 185_714 {
		t.Errorf("sale = %+v, tax %d", s, *est.Tax)
	}
}

// A lot whose cost cannot be struck claims nothing: no gain, no tax. Rules
// with no estimate give the gain without a tax.
func TestTheEstimateClaimsNothingItCannotStrike(t *testing.T) {
	today := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	h := twoLots()
	h.Lots[0].CostKnown = false
	est := Estimating([]Move{{Key: "shares", Minor: -150_000_00}}, []portfolio.SaleLots{h}, russia, today)
	if s := est.Sales["shares"]; s.Known || s.Gain != 0 || est.Tax != nil {
		t.Errorf("unknown cost: %+v, tax %v", s, est.Tax)
	}
	est = Estimating([]Move{{Key: "shares", Minor: -150_000_00}}, []portfolio.SaleLots{twoLots()}, family.TaxRulesFor("DE"), today)
	if s := est.Sales["shares"]; !s.Known || s.Gain != 9_285_714 || s.Free != 0 || est.Tax != nil {
		t.Errorf("no estimate for the country: %+v, tax %v", s, est.Tax)
	}
	// Money and a paper not to be sold now are not estimated.
	frozen := twoLots()
	frozen.LiquidInBase = nil
	est = Estimating([]Move{{Key: "broker_cash", Minor: -100}, {Key: "shares", Minor: -100}}, []portfolio.SaleLots{frozen}, russia, today)
	if len(est.Sales) != 0 {
		t.Errorf("estimated %+v", est.Sales)
	}
}
