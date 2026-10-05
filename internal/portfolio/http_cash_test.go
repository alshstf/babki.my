package portfolio_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"babki.my/babki/internal/marketdata"
)

// An account's money is published as a holding: worth at today's rate, cost
// at each parcel's arrival rate, and what is said when a rate is missing.

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

// Dollars bought at 50 and held to 90 made money.
//
//	USD->RUB 50 (02-01), 80 (05-01), 90 (07-01)
//	deposit $1 000 (03-10), $500 (05-10)
//	value 150_000 × 90 = 13_500_000; cost 100_000×50 + 50_000×80 = 9_000_000
//	profit 4_500_000 (today's-rate cost would make it 0)
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

// Spending takes the oldest money, so what remains is costed at the newer
// rate.
//
//	deposit $1 000 at 50 (03-10), $1 000 at 80 (05-10); buy a share for $1 000
//	remaining May parcel: 8_000_000 (the March one would be 5_000_000)
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

// Base-currency money is published with a zero profit, not a gap.
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

// Spending money never seen arriving makes the balance negative, published
// as such.
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
	// And no gain: value less a zero cost would show the debt as profit
	// (−18 000 ₽ on the owner's account).
	if usd.InBase.UnrealizedPnlMinor != nil {
		t.Errorf("unrealized = %d, want null: there is no gain on money the account does not have, and this figure is the debt wearing a profit's name", *usd.InBase.UnrealizedPnlMinor)
	}
	if usd.InBase.Gap == nil || *usd.InBase.Gap != "negative_balance" {
		t.Errorf("gap = %v, want negative_balance — and NOT one of the rate gaps: no rate is missing here, and a reader sent looking for one would find nothing wrong", usd.InBase.Gap)
	}
}

// An overdraft is not a loss in the account total (it once added 185 000 ₽);
// the currency still contributes what it earned leaving.
func TestAccountTotalDoesNotCountAnOverdraftAsALoss(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes,
		datedRate{earlyRateOn, "50"}, datedRate{midRateOn, "80"}, datedRate{lateRateOn, "90"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	// $1 000 arrive at 50 and leave at 80: +30 000 ₽ banked, and nothing held.
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"deposit",
		"occurred_on":"2026-03-10","amount_minor":100000,"currency":"USD"}`, acc.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":%q,"amount_minor":-100000,"currency":"USD"}`, acc.ID, sellOn))
	// And then $500 more are spent that the journal never saw arrive.
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":%q,"amount_minor":-50000,"currency":"USD"}`, acc.ID, sellOn))

	got := accountPositions(t, c, url, acc.ID).AccountTotal

	if got.InBase == nil {
		t.Fatalf("in_base is null (%v) — nothing here is unvalued", got.InBaseGap)
	}
	switch *got.InBase {
	case -1_500_000:
		t.Errorf("in_base = -1500000 — the debt was counted as a loss and the banked gain with it. Every kopeck of that debt is the journal missing an operation, not money the account lost")
	case 3_000_000:
	default:
		t.Errorf("in_base = %d, want 3000000: the dollars that were held earned 30 000 ₽, and the ones the account never had earned nothing either way", *got.InBase)
	}
	if len(got.NoRateCurrencies) != 0 {
		t.Errorf("no_rate_currencies = %v, want empty — an overdraft is not a missing rate, and naming it would send a reader looking for a backfill that has nothing to fetch", got.NoRateCurrencies)
	}
}

// No rate today leaves the row unvalued; a parcel day without one leaves the
// value and nulls the cost and profit.
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
