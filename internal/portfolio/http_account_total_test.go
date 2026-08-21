package portfolio_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
)

// The account's TOTAL — what the whole account has made, all in — is added up
// by the server for the same reason its realized total is: a figure the
// interface shows has one definition, in one place.
//
// What separates it from the realized total is what it reaches for beyond the
// rows: interest credited on the cash, commissions booked as operations of
// their own, and the tax the broker took from the account rather than from a
// payment. None of those belongs to any position, and all of them are money the
// owner actually gained or lost.

// accountFigure reads a bucket's amount, failing the test on the null that
// means "this currency has no total at all".
func accountFigure(t *testing.T, minor *int64) int64 {
	t.Helper()
	if minor == nil {
		t.Fatalf("amount_minor is null — this bucket has no figure, and the test that called this expected one")
	}
	return *minor
}

// TestAccountTotalAddsThePositionsAndTheAccountsOwnCharges is the arithmetic in
// one fixture, in one currency, with every kind of term present at once.
//
//	ACME  buy 10 @ 100,00 ₽              lot cost  100_000
//	      sell 5 @ 150,00 ₽                proceeds  75_000, basis 50_000 -> realized 25_000
//	      dividend 50,00 ₽                                               -> income    5_000
//	      quote 120,00 ₽, 5 shares left    value 60_000 - basis 50_000   -> unrealized 10_000
//	      the row's own «Всего»                                          =   40_000
//
//	the account's own charges, which no row carries:
//	      a commission booked on its own            -1_500
//	      tax taken from the account                -3_000
//	      interest credited on the cash                700
//
//	account total = 40_000 - 1_500 - 3_000 + 700   =   36_200
//
// The numbers a wrong implementation prints are named by hand below: 40_000 is
// the rows alone with every charge forgotten, and 30_000 is the settled result
// with the unrealized half dropped.
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
	// The base currency IS this account's currency here, so the one figure and
	// the bucket must agree — a base sum computed from other terms would drift.
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

// TestAccountTotalDoesNotChargeATradesCommissionTwice pins the one exclusion
// that is easy to get wrong and impossible to see on screen. A commission
// charged ON a trade is already inside the row: a purchase capitalizes it into
// the lot's cost, a disposal subtracts it from the proceeds. Taking every
// commission again at the account level would charge the owner twice for one
// charge, and the result would still look like an ordinary number.
//
//	buy  10 @ 1000,00 ₽ with a 2,00 ₽ commission -> lot cost 100_200
//	sell 10 @ 1500,00 ₽ with a 3,00 ₽ commission -> 150_000 - 300 - 100_200 = 49_500
//
// The account has no charges of its own, so its total IS the row's.
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
	// A position sold out of has no basis left, so it is not one of the papers
	// counted at nought — that mark is about money still held, not about a row
	// with a valuation of zero because there is nothing left to value.
	if got.ZeroValuedPositions != 0 {
		t.Errorf("zero_valued_positions = %d, want 0: nothing is held here, so nothing was written off", got.ZeroValuedPositions)
	}
}

// TestAccountTotalCountsAnUnpricedHoldingAtNoughtAndSaysSo is the owner's
// decision made visible. A holding nothing prices goes in at a value of nought
// — its basis counted as spent, nothing counted as held — rather than
// suppressing the account's total altogether while a single frozen fund sits in
// it. The alternative was silence, and the owner chose the conservative number
// with the assumption published beside it.
//
//	ACME  buy 10 @ 100,00 ₽, quote 120,00 ₽    -> the row's total  +20_000
//	DARK  buy 10 @  50,00 ₽, nothing prices it -> counted at nought, -50_000
//	account total                                                 -30_000
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

// TestAccountTotalInBaseConvertsEachChargeOnItsOwnDay carries the rule the rest
// of this screen already follows down to the account's own charges: a commission
// taken in one year was that many rubles in that year, not at today's rate.
//
//	fx USD->RUB: 50 from 2026-02-01, 80 from 2026-05-01, 90 from 2026-07-01
//	buy  10 @ $100.00 on 2026-03-10 (rate 50)  -> basis    100_000 USD
//	sell 10 @ $120.00 on 2026-05-10 (rate 80)  -> proceeds 120_000 USD
//	      realized  20_000 USD; in_base 120_000*80 - 100_000*50 = 4_600_000
//	a commission of $50.00 booked on 2026-05-10 (rate 80)  -> -5_000 USD, -400_000 ₽
//
//	account in USD  =    20_000 -   5_000 =    15_000
//
// And the DOLLARS THEMSELVES are a holding: the 15 000 left on the account
// arrived with the sale on 2026-05-10 (rate 80) and are worth today's 90, so the
// currency has made 15_000 * (90 - 80) = 150_000 while they sat there. That term
// is the whole reason this total is not just the papers.
//
//	account in base = 4_600_000 - 400_000 + 150_000 = 4_350_000
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

// TestAccountTotalCountsTheCurrencyResultAlreadyBanked is the case that decided
// the shape of this whole figure. Money exchanged and exchanged BACK leaves
// nothing behind to revalue — the balances afterwards are rubles bought today
// and no dollars at all — so a total built from balances alone reports a gain
// of exactly nought on an account that plainly made money.
//
//	fx USD->RUB: 50 from 2026-02-01, 80 from 2026-05-01, 90 from 2026-07-01
//	deposit 1 000,00 $ on 2026-03-10 (rate 50) -> the dollars arrive worth 50 000
//	convert them away on 2026-05-10 (rate 80)  -> they leave worth 80 000
//
//	the account made                              30_000
//
// Nothing is held at the end and the unrealized half is nought; the whole result
// is in the departure. The 80 000 ₽ that arrived in exchange are NOT income —
// they are the same money in another currency, and the ruble side of a
// conversion is why they are not counted twice.
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
}

// TestAccountTotalAddsNoCurrencyResultOnItsOwnBaseCurrency: rubles in a ruble
// space cost rubles and are worth rubles, whatever they do. The total must not
// pick up a term for them — not because the term would be wrong, but because
// asking a rate of one to say something is how a rounding becomes a result.
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
}

// TestAccountTotalNamesTheMoneyItCouldNotValue is the difference between a gap
// that closes on its own and one that never closes at all.
//
// «Нет курса» alone sends a reader to wait for a backfill. But a rate SOURCE may
// not quote a currency at all — the Bank of Russia publishes none for XAU, the
// code the broker uses for gold — and on the owner's account that one holding
// took the total off three screens with nothing saying which money was
// responsible. The currency named turns "wait" into "this one is not coming".
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
}

// TestAccountTotalNamesNoCurrencyWhenNothingWasStoppedByARate: the list is about
// rates and nothing else, so an account whose total is missing for another
// reason entirely must not put a currency's name under a sentence about rates.
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
