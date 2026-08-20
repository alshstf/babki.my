package portfolio_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"babki.my/babki/internal/marketdata"
)

// The money an account holds is published beside the papers, as a holding: a
// balance, and what it is worth against what it cost. These tests are about the
// two rates that answer those two questions — today's for the value, the day
// each parcel arrived for the cost — and about what is said when one is missing.

// cashOf finds one currency's row, failing the test when the account does not
// hold that currency at all.
func cashOf(t *testing.T, body positionsResp, currency string) cashPositionResp {
	t.Helper()
	for _, c := range body.Cash {
		if c.Currency == currency {
			return c
		}
	}
	t.Fatalf("no %s among the account's money: %+v", currency, body.Cash)
	return cashPositionResp{}
}

// TestCashIsWorthTodaysRateAndCostTheRatesOfItsOwnDays is the heart of it. A
// dollar balance bought when the dollar was 50 and held while it went to 90 has
// made real money, and a screen valuing both ends at today's rate would report
// exactly nought — for ever, on every account.
//
//	fx USD->RUB: 50 from 2026-02-01, 80 from 2026-05-01, 90 from 2026-07-01
//	  (nothing newer, so today's lookup lands on 90)
//	deposit  $1 000.00 on 2026-03-10 (rate 50) -> a parcel of 100_000 minor
//	deposit    $500.00 on 2026-05-10 (rate 80) -> a parcel of  50_000 minor
//
//	balance          150_000 minor USD
//	value  150_000 * 90                        = 13_500_000
//	cost   100_000 * 50 + 50_000 * 80          =  9_000_000
//	profit                                     =  4_500_000
//
// The number a today's-rate cost would print instead is 13_500_000, and the
// profit it would print is 0.
func TestCashIsWorthTodaysRateAndCostTheRatesOfItsOwnDays(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes,
		datedRate{earlyRateOn, "50"}, datedRate{midRateOn, "80"}, datedRate{lateRateOn, "90"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"deposit",
		"occurred_on":"2026-03-10","amount_minor":100000,"currency":"USD"}`, acc.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"deposit",
		"occurred_on":"2026-05-10","amount_minor":50000,"currency":"USD"}`, acc.ID))

	usd := cashOf(t, accountPositions(t, c, url, acc.ID), "USD")

	if usd.AmountMinor != 150_000 {
		t.Errorf("balance = %d, want 150000", usd.AmountMinor)
	}
	if usd.InBase.ValueMinor == nil || *usd.InBase.ValueMinor != 13_500_000 {
		t.Errorf("value = %v, want 13500000 (150 000 at today's 90)", usd.InBase.ValueMinor)
	}
	switch cost := usd.InBase.CostMinor; {
	case cost == nil:
		t.Errorf("cost is null (%v) — every parcel here has a rate", usd.InBase.Gap)
	case *cost == 13_500_000:
		t.Errorf("cost = 13500000 — the parcels were valued at TODAY's rate, which makes every balance cost exactly what it is worth and every profit nought, for ever")
	case *cost == 7_500_000:
		t.Errorf("cost = 7500000 — the whole balance was valued at the FIRST parcel's rate. Each parcel carries its own day")
	case *cost != 9_000_000:
		t.Errorf("cost = %d, want 9000000 (100 000 at 50 plus 50 000 at 80)", *cost)
	}
	if usd.InBase.UnrealizedPnlMinor == nil || *usd.InBase.UnrealizedPnlMinor != 4_500_000 {
		t.Errorf("profit = %v, want 4500000 — the dollar's own move while this money sat there", usd.InBase.UnrealizedPnlMinor)
	}
	if usd.InBase.Gap != nil {
		t.Errorf("gap = %q, want null: nothing was missing", *usd.InBase.Gap)
	}
}

// TestCashSpendsTheOldestParcelsFirst pins the queue on the money, and does it
// through the figures rather than through the parcels: what is LEFT after
// spending is the newer money, so its cost is struck at the newer rate.
//
//	deposit $1 000.00 on 2026-03-10 (rate 50)
//	deposit $1 000.00 on 2026-05-10 (rate 80)
//	a share bought for $1 000.00 on 2026-05-20
//
//	what remains is the MAY parcel: cost 100_000 * 80 = 8_000_000
//	the March parcel would have cost                    5_000_000
func TestCashSpendsTheOldestParcelsFirst(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes,
		datedRate{earlyRateOn, "50"}, datedRate{midRateOn, "80"}, datedRate{lateRateOn, "90"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	acme := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"deposit",
		"occurred_on":"2026-03-10","amount_minor":100000,"currency":"USD"}`, acc.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"deposit",
		"occurred_on":"2026-05-10","amount_minor":100000,"currency":"USD"}`, acc.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-05-20","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, acme.ID))

	usd := cashOf(t, accountPositions(t, c, url, acc.ID), "USD")

	if usd.AmountMinor != 100_000 {
		t.Fatalf("balance = %d, want 100000 — the purchase took a thousand dollars off it", usd.AmountMinor)
	}
	switch cost := usd.InBase.CostMinor; {
	case cost == nil:
		t.Errorf("cost is null (%v)", usd.InBase.Gap)
	case *cost == 5_000_000:
		t.Errorf("cost = 5000000 — the NEWEST parcel was spent and the March money left standing. The queue takes the oldest first")
	case *cost != 8_000_000:
		t.Errorf("cost = %d, want 8000000 (what is left is the May parcel, struck at 80)", *cost)
	}
}

// TestCashInItsOwnBaseCurrencyHasNoProfit: rubles in a ruble space cost rubles
// and are worth rubles. The row is still published — the balance is a fact worth
// showing — and its profit is an honest nought rather than a gap.
func TestCashInItsOwnBaseCurrencyHasNoProfit(t *testing.T) {
	url, c := newAPI(t)

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"deposit",
		"occurred_on":"2026-03-10","amount_minor":500000,"currency":"RUB"}`, acc.ID))

	rub := cashOf(t, accountPositions(t, c, url, acc.ID), "RUB")

	if rub.AmountMinor != 500_000 {
		t.Errorf("balance = %d, want 500000", rub.AmountMinor)
	}
	if rub.InBase.ValueMinor == nil || *rub.InBase.ValueMinor != 500_000 {
		t.Errorf("value = %v, want 500000: the rate from a currency to itself is one", rub.InBase.ValueMinor)
	}
	if rub.InBase.UnrealizedPnlMinor == nil || *rub.InBase.UnrealizedPnlMinor != 0 {
		t.Errorf("profit = %v, want 0 — and a plain zero rather than a gap: nothing here is unknown", rub.InBase.UnrealizedPnlMinor)
	}
}

// TestCashGoesNegativeAndSaysSo is the owner's own account, where some currency
// purchases are trades the broker will not explain: the journal spends yuan it
// never saw arrive. The balance is published negative rather than floored,
// because that IS the discrepancy — and a floor at nought would hide it while
// the unparsed rows beside it say something is missing.
func TestCashGoesNegativeAndSaysSo(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes, datedRate{earlyRateOn, "50"}, datedRate{lateRateOn, "90"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2026-03-10","amount_minor":-20000,"currency":"USD"}`, acc.ID))

	usd := cashOf(t, accountPositions(t, c, url, acc.ID), "USD")

	if usd.AmountMinor != -20_000 {
		t.Errorf("balance = %d, want -20000", usd.AmountMinor)
	}
	if usd.InBase.CostMinor == nil || *usd.InBase.CostMinor != 0 {
		t.Errorf("cost = %v, want 0 — nothing is held, so nothing was paid for it", usd.InBase.CostMinor)
	}
	if usd.InBase.ValueMinor == nil || *usd.InBase.ValueMinor != -1_800_000 {
		t.Errorf("value = %v, want -1800000: money owed in dollars is worth something in rubles too", usd.InBase.ValueMinor)
	}
}

// TestCashNamesTheRateThatIsMissing. Two gaps, and they stop different figures:
// no rate for today leaves the whole row unvalued, while a parcel's day without
// one leaves the value standing and takes the cost and the profit with it. A
// screen that published a profit against a cost it could not strike would be
// subtracting from nothing.
func TestCashNamesTheRateThatIsMissing(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	// One rate, and it is NEWER than the deposit below: today resolves to it,
	// the deposit's own day resolves to nothing earlier.
	url, c := fxRateAPI(t, quotes, datedRate{lateRateOn, "90"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"deposit",
		"occurred_on":"2026-03-10","amount_minor":100000,"currency":"USD"}`, acc.ID))

	usd := cashOf(t, accountPositions(t, c, url, acc.ID), "USD")

	if usd.InBase.ValueMinor == nil || *usd.InBase.ValueMinor != 9_000_000 {
		t.Errorf("value = %v, want 9000000 — today's rate exists and the valuation stands", usd.InBase.ValueMinor)
	}
	if usd.InBase.CostMinor != nil {
		t.Errorf("cost = %d, want null: the day this money arrived has no rate, and a cost summed from the parcels that do have one is smaller than the truth", *usd.InBase.CostMinor)
	}
	if usd.InBase.UnrealizedPnlMinor != nil {
		t.Errorf("profit = %d, want null: it is the difference of a figure and one that does not exist", *usd.InBase.UnrealizedPnlMinor)
	}
	if usd.InBase.Gap == nil || *usd.InBase.Gap != "no_rate_lot_date" {
		t.Errorf("gap = %v, want no_rate_lot_date — the reader is told which rate was missing, not left to guess from a null", usd.InBase.Gap)
	}
}
