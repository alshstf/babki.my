package tinvest

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/portfolio"
)

// cashByCurrency is what the journal says the account's money did, per
// currency: every entry's amount less its fee.
func cashByCurrency(ops []operation.Operation) map[string]int64 {
	out := map[string]int64{}
	for _, o := range ops {
		if o.Type == operation.TypeTransferIn || o.Type == operation.TypeTransferOut {
			continue
		}
		out[o.Currency] += o.AmountMinor - o.FeeMinor
	}
	return out
}

// Decision Р-13 on a purchase: a paper kept in dollars, bought again in
// roubles. The purchase is restated in dollars at the official rate of its day
// — amount and commission — and a pair of exchange entries at that rate keeps
// the account's money where it really moved: roubles out, dollars untouched.
func TestRebuildRestatesAPurchaseInAnotherCurrency(t *testing.T) {
	f := newRebuildFixture(t)
	f.rates.byCode["RUB"] = decimal.RequireFromString("0.0125") // 80 ₽ per dollar

	first := loadOperationItem(t, "buy.json")
	first.ID = "op-buy-usd"
	first.Payment = MoneyValue{Currency: "usd", Units: -275}
	first.Price = MoneyValue{Currency: "usd", Units: 2, Nano: 750000000}
	first.Commission = MoneyValue{Currency: "usd", Units: 0, Nano: -100000000}
	first.Date = time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)

	second := loadOperationItem(t, "buy.json") // 100 shares for 27 500 ₽ + 8,25 ₽
	second.ID = "op-buy-rub"

	f.sync(t, f.link, first, second)
	f.rebuild(t)

	journal := f.journalOf(t, f.accountID)
	row := f.mirrorRow(t, f.link, "op-buy-rub")
	restated := byExternalID(t, journal, externalIDFor(row, 1))
	if restated.Currency != "USD" || restated.AmountMinor != -34375 || restated.FeeMinor != 10 {
		t.Errorf("the rouble purchase is %d (fee %d) %s, want -34375 (fee 10) USD — 27 500 ₽ and 8,25 ₽ at 80 ₽", restated.AmountMinor, restated.FeeMinor, restated.Currency)
	}
	paid := byExternalID(t, journal, externalIDFor(row, 2))
	got := byExternalID(t, journal, externalIDFor(row, 3))
	if paid.Type != operation.TypeConversion || paid.Currency != "RUB" || paid.AmountMinor != -2750825 {
		t.Errorf("the roubles leg is %s %d %s, want conversion -2750825 RUB", paid.Type, paid.AmountMinor, paid.Currency)
	}
	if got.Type != operation.TypeConversion || got.Currency != "USD" || got.AmountMinor != 34385 {
		t.Errorf("the dollars leg is %s %d %s, want conversion +34385 USD", got.Type, got.AmountMinor, got.Currency)
	}
	cash := cashByCurrency(journal)
	if cash["RUB"] != -2750825 || cash["USD"] != -27510 {
		t.Errorf("the account's money moved %v, want -2750825 RUB (the purchase in roubles) and -27510 USD (the first purchase only)", cash)
	}
	positions, err := portfolio.Compute(mustListForEngine(t, f, f.accountID))
	if err != nil {
		t.Fatalf("the journal does not replay: %v", err)
	}
	p := positions[*restated.InstrumentID]
	if p == nil || p.Currency != "USD" || p.Quantity.String() != "200" {
		t.Errorf("the position is %+v, want 200 shares kept in USD", p)
	}
}

// A yuan bond redeemed in roubles: the redemption is restated in yuan, its count
// still the position it closes, and the roubles arrive through the exchange.
func TestRebuildRestatesARedemptionInAnotherCurrency(t *testing.T) {
	f := newRebuildFixture(t)
	f.rates.byCode["RUB"] = decimal.RequireFromString("0.08") // 12,5 ₽ per yuan

	buy := loadOperationItem(t, "bond_buy.json") // 8 bonds
	buy.Payment = MoneyValue{Currency: "cny", Units: -800}
	buy.Price = MoneyValue{Currency: "cny", Units: 100}
	repaid := loadOperationItem(t, "bond_repayment_full_no_quantity.json") // 10 000 ₽

	f.sync(t, f.link, buy, repaid)
	f.rebuild(t)

	journal := f.journalOf(t, f.accountID)
	row := f.mirrorRow(t, f.link, repaid.ID)
	redemption := byExternalID(t, journal, externalIDFor(row, 1))
	if redemption.Type != operation.TypeRedemption || redemption.Currency != "CNY" || redemption.AmountMinor != 80000 {
		t.Errorf("the redemption is %s %d %s, want redemption 80000 CNY", redemption.Type, redemption.AmountMinor, redemption.Currency)
	}
	if redemption.Quantity == nil || redemption.Quantity.String() != "8" {
		t.Errorf("the redemption retired %v bonds, want the 8 held", redemption.Quantity)
	}
	cash := cashByCurrency(journal)
	if cash["RUB"] != 1000000 || cash["CNY"] != -80000 {
		t.Errorf("the account's money moved %v, want +1000000 RUB (the payout) and -80000 CNY (the purchase)", cash)
	}
	positions, err := portfolio.Compute(mustListForEngine(t, f, f.accountID))
	if err != nil {
		t.Fatalf("the journal does not replay: %v", err)
	}
	p := positions[*redemption.InstrumentID]
	if p == nil {
		t.Fatal("no position in the bond")
	}
	if realized, one := p.RealizedPnL(); !one || realized != 0 {
		t.Errorf("the bond's realized result is %d (in one currency: %v), want 0 CNY — bought and repaid at par", realized, one)
	}
}

// A day with no official rate yet: the row stays unparsed, naming why, and
// leaves no entry behind.
func TestRebuildLeavesAForeignCurrencyOperationUnparsedWithoutARate(t *testing.T) {
	f := newRebuildFixture(t)
	f.rates.err = marketdata.ErrNoRate

	first := loadOperationItem(t, "buy.json")
	first.ID = "op-buy-usd"
	first.Payment = MoneyValue{Currency: "usd", Units: -275}
	first.Price = MoneyValue{Currency: "usd", Units: 2, Nano: 750000000}
	first.Commission = MoneyValue{Currency: "usd", Units: 0, Nano: -100000000}
	first.Date = time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	second := loadOperationItem(t, "buy.json")
	second.ID = "op-buy-rub"

	f.sync(t, f.link, first, second)
	f.rebuild(t)

	if row := f.mirrorRow(t, f.link, "op-buy-rub"); row.UnparsedReason != string(ReasonForeignCurrencyNoRate) {
		t.Errorf("the rouble purchase carries %q, want %s", row.UnparsedReason, ReasonForeignCurrencyNoRate)
	}
	if n := len(f.journalOf(t, f.accountID)); n != 1 {
		t.Errorf("journal holds %d entries, want only the dollar purchase", n)
	}
}

// A transfer moves no money, so only its currency is restated: shares kept in
// dollars leave for another depository on a row the broker wrote in roubles
// (TSPX, October 2025), and the journal takes it instead of refusing it.
func TestRebuildRestatesATransferInAnotherCurrency(t *testing.T) {
	f := newRebuildFixture(t)
	buy := loadOperationItem(t, "buy.json")
	buy.Payment = MoneyValue{Currency: "usd", Units: -275}
	buy.Price = MoneyValue{Currency: "usd", Units: 2, Nano: 750000000}
	buy.Commission = MoneyValue{Currency: "usd", Units: 0, Nano: -100000000}
	out := loadOperationItem(t, "output_securities.json")
	out.InstrumentUID, out.FIGI, out.InstrumentType, out.AssetUID = buy.InstrumentUID, buy.FIGI, buy.InstrumentType, ""
	out.Payment = MoneyValue{Currency: "rub"}
	out.Quantity = 40
	out.Description = "Вывод 40 лотов в другой депозитарий"
	out.Date = buy.Date.Add(24 * time.Hour)

	f.sync(t, f.link, buy, out)
	if stats := f.rebuild(t); stats.Unparsed != 0 {
		t.Errorf("left %d rows unparsed, want none", stats.Unparsed)
	}
	moved := byExternalID(t, f.journalOf(t, f.accountID), externalIDFor(f.mirrorRow(t, f.link, out.ID), 1))
	if moved.Type != operation.TypeTransferOut || moved.Currency != "USD" {
		t.Errorf("the withdrawal is %s in %s, want transfer_out in USD", moved.Type, moved.Currency)
	}
}

// Shares that arrived from another broker with no cost fix nothing: the
// position's currency is the first purchase's, and that purchase — in dollars
// — is not restated into the roubles the arrival's row happened to name.
func TestRebuildTakesThePositionsCurrencyFromWhatFixesIt(t *testing.T) {
	f := newRebuildFixture(t)
	f.rates.byCode["USD"] = decimal.RequireFromString("80")

	buy := loadOperationItem(t, "buy.json")
	arrived := loadOperationItem(t, "input_securities.json")
	arrived.InstrumentUID, arrived.FIGI, arrived.InstrumentType, arrived.AssetUID = buy.InstrumentUID, buy.FIGI, buy.InstrumentType, ""
	arrived.Payment = MoneyValue{Currency: "rub"}
	arrived.Date = buy.Date.Add(-24 * time.Hour)
	buy.Payment = MoneyValue{Currency: "usd", Units: -275}
	buy.Price = MoneyValue{Currency: "usd", Units: 2, Nano: 750000000}
	buy.Commission = MoneyValue{Currency: "usd", Units: 0, Nano: -100000000}

	f.sync(t, f.link, arrived, buy)
	f.rebuild(t)

	bought := byExternalID(t, f.journalOf(t, f.accountID), externalIDFor(f.mirrorRow(t, f.link, buy.ID), 1))
	if bought.Currency != "USD" || bought.AmountMinor != -27500 {
		t.Errorf("the purchase is %d %s, want -27500 USD as the broker reported it", bought.AmountMinor, bought.Currency)
	}
}
