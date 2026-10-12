package valuables

import (
	"slices"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func day(s string) time.Time {
	d, _ := time.Parse(time.DateOnly, s)
	return d
}

// The note's bar (Р-39): 100 g of gold 999.9 from Krastsvetmet, bought in
// March 2023 for 520 000.
func noteBar() Item {
	return Item{
		Kind: KindBar, Metal: marketdata.Gold, Weight: dec("100"), Fineness: dec("999.9"), Maker: "Красцветмет",
		Certificate: true, Sealed: true, BoughtOn: day("2023-03-15"), CostMinor: 520_000_00, Currency: "RUB",
	}
}

var cbrGold = &marketdata.MetalPrice{Metal: marketdata.Gold, On: day("2026-10-12"), Price: dec("10500")}

func TestAPieceIsItsPureMetal(t *testing.T) {
	if p := noteBar().Pure(); !p.Equal(dec("99.99")) {
		t.Errorf("pure = %s", p)
	}
	coin := Item{Weight: dec("7.89"), Fineness: dec("999")}
	if p := coin.Pure(); !p.Equal(dec("7.88211")) {
		t.Errorf("a George coin = %s", p)
	}
	if d := noteBar().TaxFreeFrom(); d.Format(time.DateOnly) != "2026-03-15" {
		t.Errorf("free of tax from %s", d)
	}
}

func TestWhatMakesAPieceHardToSell(t *testing.T) {
	if h := noteBar().HardToSell(); len(h) != 0 {
		t.Errorf("a sealed Russian bar with its certificate: %v", h)
	}
	pamp := noteBar()
	pamp.ForeignMade, pamp.Certificate, pamp.Sealed = true, false, false
	if h := pamp.HardToSell(); !slices.Equal(h, []Hard{HardForeign, HardNoCertificate, HardOpened}) {
		t.Errorf("all three: %v", h)
	}
}

// The guide by metal and weight, at each edge.
func TestTheGuide(t *testing.T) {
	for _, c := range []struct {
		metal  marketdata.Metal
		weight string
		want   string
	}{
		{marketdata.Gold, "1000", "5"},
		{marketdata.Gold, "100", "5"},
		{marketdata.Gold, "99.99", "7"},
		{marketdata.Gold, "20", "7"},
		{marketdata.Gold, "19.9", "10"},
		{marketdata.Gold, "5", "10"},
		{marketdata.Gold, "4.99", "15"},
		{marketdata.Gold, "1", "15"},
		{marketdata.Silver, "1000", "25"},
		{marketdata.Platinum, "100", "15"},
		{marketdata.Palladium, "10", "15"},
	} {
		if got := Guide(c.metal, dec(c.weight)); !got.Equal(dec(c.want)) {
			t.Errorf("%s %s g: %s %%, want %s", c.metal, c.weight, got, c.want)
		}
	}
}

// By the central bank: 99,99 g at 10 500 — 1 049 895; as it would sell now,
// the guide's 5 % below — 997 400,25; the storage's own 3 % stands before
// the guide.
func TestAPieceIsValuedTwice(t *testing.T) {
	today := day("2026-10-13")
	v := Valuing(noteBar(), cbrGold, Storage{Place: PlaceSafeBox}, nil, today)
	if *v.Market != 1_049_895_00 || *v.Sell != 99_740_025 || v.By != SellByGuide || !v.Percent.Equal(dec("5")) || v.PriceOn.Format(time.DateOnly) != "2026-10-12" {
		t.Errorf("by the guide: %+v", v)
	}
	three := dec("3")
	v = Valuing(noteBar(), cbrGold, Storage{Place: PlaceSafeBox, Buyback: &three}, nil, today)
	if *v.Sell != 101_839_815 || v.By != SellByStorage {
		t.Errorf("by the storage's percent: %+v", v)
	}
}

// The bank's own price stands while it is no older than 30 days: 99,99 g at
// 10 100 — 1 009 899; a day older, the guide again. A price of another
// metal does not count.
func TestTheBanksPriceStandsForAMonth(t *testing.T) {
	today := day("2026-10-13")
	bank := []BankPrice{{Metal: marketdata.Silver, Price: dec("150"), On: today}, {Metal: marketdata.Gold, Price: dec("10100"), On: day("2026-09-13")}}
	v := Valuing(noteBar(), cbrGold, Storage{}, bank, today)
	if v.By != SellByBankPrice || *v.Sell != 1_009_899_00 || *v.Market != 1_049_895_00 || v.BankOn.Format(time.DateOnly) != "2026-09-13" || v.Percent != nil {
		t.Errorf("by the bank's price: %+v", v)
	}
	bank[1].On = day("2026-09-12")
	if v := Valuing(noteBar(), cbrGold, Storage{}, bank, today); v.By != SellByGuide {
		t.Errorf("a price 31 days old: %+v", v)
	}
}

// Before the central bank has a price nothing is claimed — unless the
// family wrote in its bank's, which gives what it sells for.
func TestNoPriceYet(t *testing.T) {
	today := day("2026-10-13")
	if v := Valuing(noteBar(), nil, Storage{}, nil, today); v.Market != nil || v.Sell != nil || v.By != SellByNoPriceYet {
		t.Errorf("no price: %+v", v)
	}
	bank := []BankPrice{{Metal: marketdata.Gold, Price: dec("10100"), On: today}}
	if v := Valuing(noteBar(), nil, Storage{}, bank, today); v.Market != nil || *v.Sell != 1_009_899_00 {
		t.Errorf("only the bank's price: %+v", v)
	}
}
