package portfolio_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/testdb"
)

// The account's total is summed on the server like the realized one. Beyond
// the rows it adds interest, standalone commissions and account-level tax.

// accountFigure reads a bucket's amount, failing on null.
func accountFigure(t *testing.T, minor *int64) int64 {
	t.Helper()
	if minor == nil {
		t.Fatalf("amount_minor is null — this bucket has no figure, and the test that called this expected one")
	}
	return *minor
}

// Every kind of term in one currency:
//
//	ACME buy 10 @ 100 ₽; sell 5 @ 150 ₽ -> realized 25_000; dividend 5_000;
//	     5 left at 120 ₽ -> unrealized 10_000; row total 40_000
//	own charges: commission −1_500, tax −3_000, interest +700
//	account total = 36_200
//
// Wrong answers: 40_000 (charges forgotten), 30_000 (unrealized dropped).
func TestAccountTotalAddsThePositionsAndTheAccountsOwnCharges(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes)

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	acme := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"RUB"}`)
	acmeID, err := uuid.Parse(acme.ID)
	if err != nil {
		t.Fatalf("parse instrument id: %v", err)
	}
	quotes.byInstrument[acmeID] = marketdata.Quote{
		InstrumentID: acmeID, On: mustDate(t, "2026-07-20"),
		Price: decimal.RequireFromString("120"), Currency: "RUB",
	}

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-03-10","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"RUB"}`, acc.ID, acme.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":"2026-05-10","quantity":"5","price":"150",
		"amount_minor":75000,"currency":"RUB"}`, acc.ID, acme.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend",
		"occurred_on":"2026-06-01","amount_minor":5000,"currency":"RUB"}`, acc.ID, acme.ID))
	// The three that belong to the ACCOUNT and to no paper on it.
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"fee",
		"occurred_on":"2026-06-02","amount_minor":-1500,"currency":"RUB"}`, acc.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"tax",
		"occurred_on":"2026-06-03","amount_minor":-3000,"currency":"RUB"}`, acc.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"interest",
		"occurred_on":"2026-06-04","amount_minor":700,"currency":"RUB"}`, acc.ID))

	got := accountPositions(t, c, url, acc.ID).AccountTotal

	if len(got.ByCurrency) != 1 || got.ByCurrency[0].Currency != "RUB" {
		t.Fatalf("by_currency = %+v, want exactly one RUB entry", got.ByCurrency)
	}
	switch total := accountFigure(t, got.ByCurrency[0].AmountMinor); total {
	case 40_000:
		t.Errorf("total = 40000 — that is the rows alone: the commission, the tax and the interest never reached it, and they are exactly the money no position can see")
	case 30_000:
		t.Errorf("total = 30000 — that is the settled result with the unrealized half dropped, which is the half that makes this figure differ from the realized total beside it")
	default:
		if total != 36_200 {
			t.Errorf("total = %d, want 36200 (40000 - 1500 - 3000 + 700)", total)
		}
	}
	// The base currency is the account's, so both figures agree.
	if got.InBase == nil || *got.InBase != 36_200 {
		t.Errorf("in_base = %v, want 36200 — the space's base currency is RUB, so nothing was converted and the two forms are one number", got.InBase)
	}
	if got.InBaseGap != nil {
		t.Errorf("in_base_gap = %q, want null: nothing about this account is unvalued", *got.InBaseGap)
	}
	if got.ZeroValuedPositions != 0 || got.UnknownCostPositions != 0 {
		t.Errorf("zero_valued/unknown_cost = %d/%d, want 0/0: this paper is priced and knows what it cost",
			got.ZeroValuedPositions, got.UnknownCostPositions)
	}
}

// A trade's commission is already in the row and is not charged again.
//
//	buy 10 @ 1 000 ₽ + 2 ₽ -> 100_200; sell 10 @ 1 500 ₽ − 3 ₽ -> 49_500
func TestAccountTotalDoesNotChargeATradesCommissionTwice(t *testing.T) {
	url, c := newAPI(t)

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	acme := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"RUB"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-03-10","quantity":"10","price":"1000",
		"amount_minor":-100000,"fee_minor":200,"currency":"RUB"}`, acc.ID, acme.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":"2026-05-10","quantity":"10","price":"1500",
		"amount_minor":150000,"fee_minor":300,"currency":"RUB"}`, acc.ID, acme.ID))

	got := accountPositions(t, c, url, acc.ID).AccountTotal

	switch total := accountFigure(t, got.ByCurrency[0].AmountMinor); total {
	case 49_000:
		t.Errorf("total = 49000 — both commissions were taken a second time (49500 - 200 - 300). They are already inside the row: one is in the lot's cost, the other came off the proceeds")
	case 49_800:
		t.Errorf("total = 49800 — the sale's commission was dropped rather than counted once")
	default:
		if total != 49_500 {
			t.Errorf("total = %d, want 49500 (150000 - 300 - 100200)", total)
		}
	}
	// A sold-out position has no basis, so it is not counted at nought.
	if got.ZeroValuedPositions != 0 {
		t.Errorf("zero_valued_positions = %d, want 0: nothing is held here, so nothing was written off", got.ZeroValuedPositions)
	}
}

// An unpriced holding counts at nought, with the count published (the owner's
// decision).
//
//	ACME buy 10 @ 100 ₽, quoted 120 ₽ -> +20_000
//	DARK buy 10 @ 50 ₽, unpriced -> −50_000
//	account total −30_000
func TestAccountTotalCountsAnUnpricedHoldingAtNoughtAndSaysSo(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes)

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	acme := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"RUB"}`)
	dark := createInstrument(t, c, url, `{"type":"share","name":"Замороженная","ticker":"DARK","currency":"RUB"}`)
	acmeID, err := uuid.Parse(acme.ID)
	if err != nil {
		t.Fatalf("parse instrument id: %v", err)
	}
	quotes.byInstrument[acmeID] = marketdata.Quote{
		InstrumentID: acmeID, On: mustDate(t, "2026-07-20"),
		Price: decimal.RequireFromString("120"), Currency: "RUB",
	}

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-03-10","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"RUB"}`, acc.ID, acme.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-03-10","quantity":"10","price":"50",
		"amount_minor":-50000,"currency":"RUB"}`, acc.ID, dark.ID))

	got := accountPositions(t, c, url, acc.ID).AccountTotal

	switch total := accountFigure(t, got.ByCurrency[0].AmountMinor); total {
	case 20_000:
		t.Errorf("total = 20000 — the unpriced holding was skipped rather than written off. Skipping it says the money was never spent, which is a different claim from «it is worth nothing today» and a more flattering one")
	case -30_000:
	default:
		t.Errorf("total = %d, want -30000 (20000 gained on the priced paper, 50000 written off on the unpriced one)", total)
	}
	if got.ZeroValuedPositions != 1 {
		t.Errorf("zero_valued_positions = %d, want 1: the figure rests on writing one paper off, and a reader is told so rather than left to discover it", got.ZeroValuedPositions)
	}
	if len(got.ZeroValuedCostByCurrency) != 1 ||
		got.ZeroValuedCostByCurrency[0].Currency != "RUB" ||
		got.ZeroValuedCostByCurrency[0].AmountMinor != 50_000 {
		t.Errorf("zero_valued_cost_by_currency = %+v, want one RUB entry of 50000 — the exact amount by which this total understates", got.ZeroValuedCostByCurrency)
	}
}

// Each account charge converts at its own day's rate, and the dollars left on
// the account are a holding too.
//
//	USD->RUB 50 (02-01), 80 (05-01), 90 (07-01)
//	buy 10 @ $100 (03-10), sell 10 @ $120 (05-10): 4_600_000
//	commission $50 on 05-10: −400_000
//	$15 000 held since 05-10 (80), worth 90 today: +150_000
//	base total 4_350_000
func TestAccountTotalInBaseConvertsEachChargeOnItsOwnDay(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes,
		datedRate{earlyRateOn, "50"}, datedRate{midRateOn, "80"}, datedRate{lateRateOn, "90"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	acme := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-03-10","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, acme.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":%q,"quantity":"10","price":"120",
		"amount_minor":120000,"currency":"USD"}`, acc.ID, acme.ID, sellOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"fee",
		"occurred_on":%q,"amount_minor":-5000,"currency":"USD"}`, acc.ID, sellOn))

	got := accountPositions(t, c, url, acc.ID).AccountTotal

	if total := accountFigure(t, got.ByCurrency[0].AmountMinor); total != 15_000 {
		t.Errorf("USD total = %d, want 15000 (20000 realized less the 5000 commission)", total)
	}
	if got.InBase == nil {
		t.Fatalf("in_base is null (%v) — every term here is valued, so there is a figure", got.InBaseGap)
	}
	switch *got.InBase {
	case 4_600_000:
		t.Errorf("in_base = 4600000 — neither the commission nor the money's own move reached the base figure")
	case 4_200_000:
		t.Errorf("in_base = 4200000 — the papers and the charge, with the dollars left on the account contributing nothing. Money is a holding: 15 000 $ that arrived at 80 and are worth 90 have made 150 000 ₽, and a total that omits it says the account did nothing with its cash")
	case 4_300_000:
		t.Errorf("in_base = 4300000 — the commission was converted at TODAY's rate (90) rather than at the rate of the day it was charged (80). Every other past event on this screen is valued on its own day")
	default:
		if *got.InBase != 4_350_000 {
			t.Errorf("in_base = %d, want 4350000 (4600000 - 400000 + 150000)", *got.InBase)
		}
	}
}

// Money exchanged and exchanged back made money though nothing is left to
// revalue: the result is in the departure.
//
//	USD->RUB 50 (02-01), 80 (05-01)
//	deposit $1 000 on 03-10, converted away on 05-10: +30_000
//
// The roubles received are the same money, not income.
func TestAccountTotalCountsTheCurrencyResultAlreadyBanked(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes,
		datedRate{earlyRateOn, "50"}, datedRate{midRateOn, "80"}, datedRate{lateRateOn, "90"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"deposit",
		"occurred_on":"2026-03-10","amount_minor":100000,"currency":"USD"}`, acc.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"conversion",
		"occurred_on":%q,"amount_minor":-100000,"currency":"USD"}`, acc.ID, sellOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"conversion",
		"occurred_on":%q,"amount_minor":8000000,"currency":"RUB"}`, acc.ID, sellOn))

	body := accountPositions(t, c, url, acc.ID)

	usd := cashOf(t, body, "USD")
	if usd.AmountMinor != 0 {
		t.Fatalf("the dollar balance is %d, want 0 — they were all exchanged back", usd.AmountMinor)
	}
	if usd.InBase.RealizedPnlMinor == nil || *usd.InBase.RealizedPnlMinor != 3_000_000 {
		t.Errorf("the dollars' banked result is %v, want 3000000 (they left at 80 having arrived at 50)", usd.InBase.RealizedPnlMinor)
	}
	if usd.InBase.UnrealizedPnlMinor == nil || *usd.InBase.UnrealizedPnlMinor != 0 {
		t.Errorf("the dollars' unrealized result is %v, want 0 — nothing is held", usd.InBase.UnrealizedPnlMinor)
	}

	got := body.AccountTotal
	if got.InBase == nil {
		t.Fatalf("in_base is null (%v) — every rate this needs is seeded", got.InBaseGap)
	}
	switch *got.InBase {
	case 0:
		t.Errorf("in_base = 0 — the account's whole result was the currency's, and a total built from what is still held reports nought on money that has already been turned back. That is the case this term exists for")
	case 3_000_000:
	default:
		t.Errorf("in_base = %d, want 3000000", *got.InBase)
	}
	// All of it is the currency's (decision Р-5).
	if got.CashFxInBase == nil || *got.CashFxInBase != 3_000_000 {
		t.Errorf("cash_fx_in_base = %v, want 3000000 — the whole result is the dollars' move", got.CashFxInBase)
	}
}

// Base-currency money adds no currency result.
func TestAccountTotalAddsNoCurrencyResultOnItsOwnBaseCurrency(t *testing.T) {
	url, c := newAPI(t)

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"deposit",
		"occurred_on":"2026-03-10","amount_minor":500000,"currency":"RUB"}`, acc.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2026-04-10","amount_minor":-200000,"currency":"RUB"}`, acc.ID))

	got := accountPositions(t, c, url, acc.ID).AccountTotal
	if got.InBase == nil || *got.InBase != 0 {
		t.Errorf("in_base = %v, want 0: putting money in and taking it out is not earning it", got.InBase)
	}
	if got.InBaseGap != nil {
		t.Errorf("in_base_gap = %q, want null — nothing here needed a rate at all", *got.InBaseGap)
	}
	if got.CashFxInBase != nil {
		t.Errorf("cash_fx_in_base = %d, want null — the account holds no money in another currency", *got.CashFxInBase)
	}
}

// The total names the currency it could not value: the CBR publishes no XAU
// rate, so "wait" would be false.
func TestAccountTotalNamesTheMoneyItCouldNotValue(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	// USD has rates; nothing anywhere quotes XAU.
	url, c := fxRateAPI(t, quotes, datedRate{earlyRateOn, "50"}, datedRate{lateRateOn, "90"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"deposit",
		"occurred_on":"2026-03-10","amount_minor":100000,"currency":"USD"}`, acc.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"deposit",
		"occurred_on":"2026-03-10","amount_minor":2600,"currency":"XAU"}`, acc.ID))

	got := accountPositions(t, c, url, acc.ID).AccountTotal

	if got.InBase != nil {
		t.Fatalf("in_base = %d, want null: a term of it could not be valued", *got.InBase)
	}
	if len(got.NoRateCurrencies) != 1 || got.NoRateCurrencies[0] != "XAU" {
		t.Errorf("no_rate_currencies = %v, want [XAU] — the dollars have rates, and naming them too would send a reader looking at money that is fine", got.NoRateCurrencies)
	}
	if got.CashFxInBase != nil {
		t.Errorf("cash_fx_in_base = %d, want null: part of the money has no rate, so its currency result has no figure either", *got.CashFxInBase)
	}
}

// No currency is named when nothing was stopped by a rate.
func TestAccountTotalNamesNoCurrencyWhenNothingWasStoppedByARate(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes, datedRate{earlyRateOn, "50"}, datedRate{lateRateOn, "90"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"deposit",
		"occurred_on":"2026-03-10","amount_minor":100000,"currency":"USD"}`, acc.ID))

	got := accountPositions(t, c, url, acc.ID).AccountTotal

	if len(got.NoRateCurrencies) != 0 {
		t.Errorf("no_rate_currencies = %v, want empty: every rate this account needed exists", got.NoRateCurrencies)
	}
}

// A paper with unknown purchase dates is left out and counted, not allowed to
// blank the account (five of the owner's six accounts were blank).
//
//	USD->RUB 60 (02-01), 90 (07-01)
//	ACME transferred in without dates -> left out
//	BETA bought $2 000 on 07-10, no quote -> nought: −200_000 × 90
//	in_base −18_000_000
func TestAccountTotalLeavesOutAPaperNobodyKnowsTheDatesOf(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))
	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: mustDate(t, earlyRateOn), Rate: decimal.RequireFromString("60"), Source: "test"},
		{Base: "USD", Quote: "RUB", On: mustDate(t, lateRateOn), Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rates: %v", err)
	}

	from := createAccount(t, c, url, `{"name":"Старый брокер","type":"brokerage","currency":"USD"}`)
	to := createAccount(t, c, url, `{"name":"Новый брокер","type":"brokerage","currency":"USD"}`)
	acme := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	beta := createInstrument(t, c, url, `{"type":"share","name":"Бета","ticker":"BETA","currency":"USD"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, from.ID, acme.ID, earlyBuyOn))
	createTransfer(t, c, url, fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,
		"quantity":"10","occurred_on":%q}`, from.ID, to.ID, acme.ID, transferOn))
	// A pre-breakdown transfer: basis kept, dates gone.
	if _, err := pool.Exec(t.Context(), `DELETE FROM operation_transfer_lots`); err != nil {
		t.Fatalf("drop the stored breakdown: %v", err)
	}
	// Funded first, so the money ends at nought and adds nothing.
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"deposit",
		"occurred_on":%q,"amount_minor":200000,"currency":"USD"}`, to.ID, lateBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"200",
		"amount_minor":-200000,"currency":"USD"}`, to.ID, beta.ID, lateBuyOn))

	got := accountPositions(t, c, url, to.ID).AccountTotal

	if got.InBase == nil {
		t.Fatalf("in_base is null (%v) — one paper's dates are unknown, and withholding the account's whole figure over it withholds it for ever", got.InBaseGap)
	}
	if *got.InBase != -18_000_000 {
		t.Errorf("in_base = %d, want -18000000 (BETA's basis of 200000 at 90, written off for want of a quote)", *got.InBase)
	}
	if got.UndatedPositions != 1 {
		t.Errorf("undated_positions = %d, want 1: the figure covers one paper of two, and a reader is told so", got.UndatedPositions)
	}
}
