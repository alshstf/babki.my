package portfolio_test

import (
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/testdb"
)

// The account's realized total is added up by the SERVER and rendered by
// whoever reads it. These tests are about that total: what it adds, what it
// refuses to add, and what it says when it refuses.
//
// The rule the total exists to keep is the project's own — money arithmetic
// happens once, on the server — and the reason it matters here is not that
// int64 addition is dangerous (it is not; the terms are already converted and
// already rounded) but that a figure the interface shows must have exactly one
// definition. The alternative, every client adding the rows itself, makes the
// rounding policy, the gap policy and "which positions count" three decisions
// re-taken in every reader.

// accountPositions fetches the whole positions payload — the total lives on the
// response, next to cost_basis_rules, because it describes the list rather than
// any one row (see PositionsResponse in the API contract), so the tests below
// need more than onlyPosition/positionsByTicker hand back.
func accountPositions(t *testing.T, c *http.Client, url, accountID string) positionsResp {
	t.Helper()
	resp := do(t, c, "GET", url+"/api/v1/accounts/"+accountID+"/positions", "")
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET positions = %d, want 200: %s", resp.StatusCode, b)
	}
	var got positionsResp
	decodeJSON(t, resp, &got)
	return got
}

// gapOf renders in_base/in_base_gap as one short string for error messages:
// "4560000", "null (undated)", "null (no gap named)".
func gapOf(rt realizedTotalResp) string {
	if rt.InBase != nil {
		return fmt.Sprintf("%d", *rt.InBase)
	}
	if rt.InBaseGap == nil {
		return "null (no gap named)"
	}
	return fmt.Sprintf("null (%s)", *rt.InBaseGap)
}

// TestRealizedTotalAddsEachCurrencyOnItsOwn pins the by-currency form: one
// figure per currency the account's positions are denominated in, each an exact
// sum inside that currency, and never one integer made of two.
//
// A dollar result and a euro result cannot be added: the sum would be a number
// denominated in nothing, and it would look exactly like a real total on
// screen. The fixture makes that visible — 15_000 + 2_000 = 17_000 is the
// number a single accumulator prints, and it is the answer to no question.
//
// The second account pins the other end: an account with no positions gets an
// empty by_currency, which is how a reader tells "there is nothing to say here"
// apart from a genuine zero. A zero over an empty account answers a question
// nobody asked.
func TestRealizedTotalAddsEachCurrencyOnItsOwn(t *testing.T) {
	url, c := newAPI(t)

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	empty := createAccount(t, c, url, `{"name":"Пустой","type":"brokerage","currency":"USD"}`)
	acme := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	beta := createInstrument(t, c, url, `{"type":"share","name":"Бета","ticker":"BETA","currency":"USD"}`)
	euro := createInstrument(t, c, url, `{"type":"share","name":"Европейка","ticker":"EURO","currency":"EUR"}`)

	// +20_000 USD
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-03-10","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, acme.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":"2026-05-10","quantity":"10","price":"120",
		"amount_minor":120000,"currency":"USD"}`, acc.ID, acme.ID))
	// -5_000 USD: a loss, so the total is a sum and not a max or a first-wins.
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-03-10","quantity":"5","price":"100",
		"amount_minor":-50000,"currency":"USD"}`, acc.ID, beta.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":"2026-05-10","quantity":"5","price":"90",
		"amount_minor":45000,"currency":"USD"}`, acc.ID, beta.ID))
	// +2_000 EUR, in the same account: the currency of a position is the
	// currency of its operations, not of the account it sits in.
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-03-10","quantity":"10","price":"10",
		"amount_minor":-10000,"currency":"EUR"}`, acc.ID, euro.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":"2026-05-10","quantity":"10","price":"12",
		"amount_minor":12000,"currency":"EUR"}`, acc.ID, euro.ID))

	got := accountPositions(t, c, url, acc.ID).RealizedTotal

	if len(got.ByCurrency) != 2 {
		t.Fatalf("by_currency = %+v, want exactly two entries (EUR and USD). One entry means the currencies were added into a single integer denominated in nothing; three means the positions of one currency were not folded together", got.ByCurrency)
	}
	// Ordered by currency code, so the same account always reads the same way.
	if got.ByCurrency[0].Currency != "EUR" || got.ByCurrency[1].Currency != "USD" {
		t.Fatalf("by_currency currencies = %q/%q, want EUR then USD (ordered by code)",
			got.ByCurrency[0].Currency, got.ByCurrency[1].Currency)
	}
	if got.ByCurrency[0].RealizedPnlMinor != 2_000 {
		t.Errorf("EUR total = %d, want 2000 (12000 - 10000)", got.ByCurrency[0].RealizedPnlMinor)
	}
	switch usd := got.ByCurrency[1].RealizedPnlMinor; usd {
	case 20_000:
		t.Errorf("USD total = 20000 — that is the winning position alone; the losing one (-5000) was dropped rather than added")
	case 17_000:
		t.Errorf("USD total = 17000 — that is 15000 USD plus 2000 EUR in one integer. Dollars and euros are not addable, and the sum is denominated in nothing")
	default:
		if usd != 15_000 {
			t.Errorf("USD total = %d, want 15000 (20000 - 5000)", usd)
		}
	}
	if got.BaseCurrency != "RUB" {
		t.Errorf("base_currency = %q, want RUB (the space's own, so a reader can format in_base without going back to the session)", got.BaseCurrency)
	}

	// The empty account: nothing to add up, and nothing withheld either.
	none := accountPositions(t, c, url, empty.ID).RealizedTotal
	if len(none.ByCurrency) != 0 {
		t.Errorf("by_currency on an account with no positions = %+v, want empty", none.ByCurrency)
	}
	if none.InBase == nil || *none.InBase != 0 {
		t.Errorf("in_base on an account with no positions = %s, want 0: nothing is missing, so nothing is withheld — the sum of no deals is zero", gapOf(none))
	}
	if none.InBaseGap != nil {
		t.Errorf("in_base_gap on an account with no positions = %q, want null: an empty account has no gap, it has no deals", *none.InBaseGap)
	}
}

// TestRealizedTotalInBaseAddsTheConvertedFigures is the heart of the
// base-currency form. The account's total is the sum of the positions' OWN
// base-currency figures — each struck at the rates of its own days — and not
// the position-currency total scaled by any single rate.
//
//	fx USD->RUB: 50 from 2026-02-01, 80 from 2026-05-01, 90 from 2026-07-01
//	  (nothing newer, so today's lookup lands on 90)
//	ACME  buy 10 @ $100.00 on 2026-03-10 (rate 50); sell 10 @ $120.00 on
//	      2026-05-10 with a $5.00 fee (rate 80)
//	      realized  19_500 USD;  in_base 120_000*80 - 500*80 - 100_000*50 = 4_560_000
//	BETA  buy 10 @ $50.00 on 2026-03-10 (rate 50); sell 10 @ $60.00 on
//	      2026-05-10 (rate 80)
//	      realized  10_000 USD;  in_base  60_000*80 - 50_000*50           = 2_300_000
//
//	account in_base = 4_560_000 + 2_300_000 = 6_860_000
//
// The numbers a scaled implementation prints instead are named by hand:
// 29_500 * 90 = 2_655_000 (the USD total at today's rate) and 29_500 * 80 =
// 2_360_000 (at the sale day's). Both are less than half the truth, because
// most of the result is the dollar's own move between purchase and sale — and
// both look exactly like ordinary numbers on screen.
func TestRealizedTotalInBaseAddsTheConvertedFigures(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes,
		datedRate{earlyRateOn, "50"}, datedRate{midRateOn, "80"}, datedRate{lateRateOn, "90"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	acme := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	beta := createInstrument(t, c, url, `{"type":"share","name":"Бета","ticker":"BETA","currency":"USD"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, acme.ID, earlyBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":%q,"quantity":"10","price":"120",
		"amount_minor":120000,"fee_minor":500,"currency":"USD"}`, acc.ID, acme.ID, sellOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"50",
		"amount_minor":-50000,"currency":"USD"}`, acc.ID, beta.ID, earlyBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":%q,"quantity":"10","price":"60",
		"amount_minor":60000,"currency":"USD"}`, acc.ID, beta.ID, sellOn))

	resp := accountPositions(t, c, url, acc.ID)
	got := resp.RealizedTotal

	if got.InBaseGap != nil {
		t.Fatalf("in_base_gap = %q, want null: every date both sums need has a rate", *got.InBaseGap)
	}
	if got.InBase == nil {
		t.Fatalf("in_base = null, want 6860000 — nothing is missing here")
	}
	switch total := *got.InBase; total {
	case 2_655_000:
		t.Fatalf("in_base = 2655000 — that is the USD total times TODAY's rate (29500 * 90). A settled result has nothing to do with today")
	case 2_360_000:
		t.Fatalf("in_base = 2360000 — that is the USD total times the SALE DAY's rate (29500 * 80). The proceeds belong to that day; the basis belongs to the day the shares were bought")
	case 4_560_000:
		t.Fatalf("in_base = 4560000 — that is ACME alone; BETA's 2300000 was assigned over the running total instead of added to it")
	default:
		if total != 6_860_000 {
			t.Errorf("in_base = %d, want 6860000 (4560000 + 2300000)", total)
		}
	}

	// The same figure, reached the other way: the total IS the sum of the
	// per-position figures published one field away in this same response.
	var sum int64
	for _, p := range resp.Positions {
		if p.InBase == nil || p.InBase.RealizedPnlMinor == nil {
			t.Fatalf("%s.in_base.realized_pnl_minor = null, want a figure — the fixture is not testing what it means to", p.Instrument.Ticker)
		}
		sum += *p.InBase.RealizedPnlMinor
	}
	if got.InBase != nil && *got.InBase != sum {
		t.Errorf("in_base = %d but the positions in the same response add up to %d. A header that disagrees with the rows it stands over is a number nobody can check", *got.InBase, sum)
	}
}

// TestRealizedTotalInBaseCountsAPositionAlreadyInTheBaseCurrency covers the
// term a reader of `in_base` alone would silently lose. A position denominated
// in the base currency carries no in_base block at all — there is nothing to
// convert — and its realized result is not unknown, it is already the
// base-currency figure. A total assembled only from in_base blocks drops it and
// under-reports the account by exactly that amount.
//
//	fx USD->RUB: 90, one rate for every date this test touches
//	ACME (USD)  buy $1000.00, sell $1200.00 -> realized  20_000 USD
//	                                           in_base  (120_000-100_000)*90 = 1_800_000
//	RUBL (RUB)  arrives by a transfer whose per-lot breakdown was never kept,
//	            then sells for 1300.00₽  -> realized 30_000 RUB, and that IS the
//	            base-currency figure
//
//	account in_base = 1_800_000 + 30_000 = 1_830_000
//
// The ruble position deliberately retires a parcel with no purchase date. That
// is what makes the fixture discriminating: a total assembled by converting
// every position without first asking whether a conversion is needed reports an
// unrecorded purchase as the gap that stopped it — over a figure that never
// needed a rate for any date at all, and which the same response publishes in
// full one field away.
func TestRealizedTotalInBaseCountsAPositionAlreadyInTheBaseCurrency(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))
	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: mustDate(t, earlyRateOn), Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rates: %v", err)
	}

	src := createAccount(t, c, url, `{"name":"Старый брокер","type":"brokerage","currency":"RUB"}`)
	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	acme := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	rubl := createInstrument(t, c, url, `{"type":"share","name":"Рублевая","ticker":"RUBL","currency":"RUB"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, acme.ID, earlyBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":%q,"quantity":"10","price":"120",
		"amount_minor":120000,"currency":"USD"}`, acc.ID, acme.ID, lateBuyOn))

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"RUB"}`, src.ID, rubl.ID, earlyBuyOn))
	createTransfer(t, c, url, fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,
		"quantity":"10","occurred_on":%q}`, src.ID, acc.ID, rubl.ID, transferOn))
	if _, err := pool.Exec(t.Context(), `DELETE FROM operation_transfer_lots`); err != nil {
		t.Fatalf("drop the stored breakdown: %v", err)
	}
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":"2026-07-28","quantity":"10","price":"130",
		"amount_minor":130000,"currency":"RUB"}`, acc.ID, rubl.ID))

	resp := accountPositions(t, c, url, acc.ID)
	got := resp.RealizedTotal

	// Pin the fixture: the ruble position publishes no in_base — and that null
	// means "nothing to convert", not "unknown" — while the parcel its sale
	// retired really does have no purchase date.
	for _, p := range resp.Positions {
		if p.Instrument.Ticker != "RUBL" {
			continue
		}
		if p.InBase != nil {
			t.Fatalf("RUBL.in_base = %+v, want null: it is already in the base currency", *p.InBase)
		}
		if !p.HasUndatedRealizations || p.RealizedPnlMinor != 30_000 {
			t.Fatalf("RUBL = {has_undated_realizations %v, realized_pnl_minor %d}, want {true, 30000} — the fixture is not testing what it means to",
				p.HasUndatedRealizations, p.RealizedPnlMinor)
		}
	}

	if got.InBaseGap != nil {
		t.Fatalf("in_base_gap = %q, want null. A position already in the base currency needs no rate for any date, so no missing date can stop its figure: its own realized_pnl_minor IS the answer", *got.InBaseGap)
	}
	if got.InBase == nil {
		t.Fatalf("in_base = %s, want 1830000", gapOf(got))
	}
	switch total := *got.InBase; total {
	case 1_800_000:
		t.Errorf("in_base = 1800000 — the ruble position was dropped because it has no in_base block to read. Its own realized_pnl_minor already IS the base-currency figure")
	case 2_700_000:
		t.Errorf("in_base = 2700000 — the ruble result was converted a second time (30000 * 90), as if rubles had to be turned into rubles")
	default:
		if total != 1_830_000 {
			t.Errorf("in_base = %d, want 1830000 (1800000 + 30000)", total)
		}
	}
}

// TestRealizedTotalNamesWhichGapStoppedTheBaseSum is the finding this response
// field exists for. When the base-currency total cannot be struck, nothing is
// published in its place — a total missing one of its terms reads as a smaller
// result, not as a gap — and the REASON travels as a fact from the code that
// knows it, not as something a reader re-derives from the positions' flags.
//
// The two reasons are not interchangeable, and one of them cannot be
// reconstructed downstream at all. `has_undated_lots` speaks about the lots
// still HELD, while a parcel that stopped a realized sum has by definition
// already been sold; a client reading that flag would say "no rate yet" over a
// permanent, unrecoverable gap and promise a number that is never coming.
//
// Three accounts in one fixture, one for each answer:
//
//	fx USD->RUB: 60 from 2026-02-01, 90 from 2026-07-01 (nothing earlier)
//	undated — receives 10 shares by a transfer whose per-lot breakdown was
//	          never kept, then sells them: the retired parcel has no purchase
//	          date, and no rate answers for a date nobody recorded
//	norate  — buys on 2026-01-05, before the fx table begins, then sells: every
//	          date is known and one of them has no rate YET
//	both    — one position of each kind
func TestRealizedTotalNamesWhichGapStoppedTheBaseSum(t *testing.T) {
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

	src := createAccount(t, c, url, `{"name":"Старый брокер","type":"brokerage","currency":"USD"}`)
	undated := createAccount(t, c, url, `{"name":"Без дат","type":"brokerage","currency":"USD"}`)
	norate := createAccount(t, c, url, `{"name":"Без курса","type":"brokerage","currency":"USD"}`)
	both := createAccount(t, c, url, `{"name":"И то и то","type":"brokerage","currency":"USD"}`)
	acme := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	beta := createInstrument(t, c, url, `{"type":"share","name":"Бета","ticker":"BETA","currency":"USD"}`)
	gamma := createInstrument(t, c, url, `{"type":"share","name":"Гамма","ticker":"GAMMA","currency":"USD"}`)

	for _, in := range []struct{ instrument, to string }{{acme.ID, undated.ID}, {beta.ID, both.ID}} {
		createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
			"occurred_on":%q,"quantity":"10","price":"100",
			"amount_minor":-100000,"currency":"USD"}`, src.ID, in.instrument, earlyBuyOn))
		createTransfer(t, c, url, fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,
			"quantity":"10","occurred_on":%q}`, src.ID, in.to, in.instrument, transferOn))
	}
	// Turn both transfers into ones recorded before breakdowns were kept: the
	// basis survives on the operation, the dates behind it do not.
	if _, err := pool.Exec(t.Context(), `DELETE FROM operation_transfer_lots`); err != nil {
		t.Fatalf("drop the stored breakdowns: %v", err)
	}
	for _, sale := range []struct{ account, instrument string }{{undated.ID, acme.ID}, {both.ID, beta.ID}} {
		createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
			"occurred_on":"2026-07-28","quantity":"10","price":"200",
			"amount_minor":200000,"currency":"USD"}`, sale.account, sale.instrument))
	}
	// The dated-but-unrated pair, in two accounts.
	for _, acc := range []string{norate.ID, both.ID} {
		createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
			"occurred_on":"2026-01-05","quantity":"10","price":"30",
			"amount_minor":-30000,"currency":"USD"}`, acc, gamma.ID))
		createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
			"occurred_on":"2026-07-28","quantity":"10","price":"40",
			"amount_minor":40000,"currency":"USD"}`, acc, gamma.ID))
	}

	for _, tc := range []struct {
		name       string
		account    string
		wantGap    string
		wantNative int64
		why        string
	}{
		{
			"undated", undated.ID, "undated", 100_000,
			"the sale retired a parcel whose purchase date nobody recorded; no rate answers for a date that does not exist, and none ever will",
		},
		{
			"norate", norate.ID, "no_rate", 10_000,
			"every date is known and the fx table simply does not reach 2026-01-05 yet — the backfill closes that gap on its own",
		},
		{
			"both", both.ID, "both", 110_000,
			"one position was stopped by an unrecorded purchase and another by a rate that has not arrived; naming only one of them promises the whole total is coming when half of it never is",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := accountPositions(t, c, url, tc.account).RealizedTotal

			if got.InBase != nil {
				t.Errorf("in_base = %d, want null: %s. A total missing one of its terms is an invented number, indistinguishable from a real one on screen", *got.InBase, tc.why)
			}
			if got.InBaseGap == nil {
				t.Fatalf("in_base_gap = null while in_base is %s — the reader is left to guess, which is exactly what this field exists to prevent", gapOf(got))
			}
			if *got.InBaseGap != tc.wantGap {
				t.Errorf("in_base_gap = %q, want %q: %s", *got.InBaseGap, tc.wantGap, tc.why)
			}
			// Whatever the conversion is missing, the positions' own currency
			// always has a complete answer: an unrecorded purchase date costs
			// no money and no shares.
			if len(got.ByCurrency) != 1 || got.ByCurrency[0].RealizedPnlMinor != tc.wantNative {
				t.Errorf("by_currency = %+v, want one USD entry of %d — the by-currency form has no gaps and must not be withheld along with the converted one", got.ByCurrency, tc.wantNative)
			}
		})
	}
}

// TestRealizedTotalStandsWhenAPositionsConversionBlockIsAbsent pins the choice
// that separates this total from a client-side sum over `in_base` blocks.
//
// A position's in_base object goes null as a whole for reasons that have
// nothing to do with disposals already made: no rate for today, no quote, or —
// as here — a lot still HELD whose purchase date was never recorded, which sits
// inside cost_minor's sum and takes the object down with it. A settled result
// does not become unknowable because of any of that, and suppressing the
// account's total over it would be silence about a figure the server has in
// hand.
//
// It is also the only way to avoid explaining a realized gap with
// `has_undated_lots`, a flag that speaks about lots still held and can never be
// the true cause of a missing realized figure.
//
//	fx USD->RUB: 60 from 2026-02-01, 90 from 2026-07-01
//	ACME — 10 shares in by a transfer with no stored breakdown, never sold:
//	       has_undated_lots true, in_base null, realized 0
//	BETA — an ordinary buy at $100.00 and sale at $150.00, both dated after
//	       2026-07-01: realized 5_000 USD, in_base (150_000-100_000)*90 = 4_500_000
//
//	account in_base = 4_500_000, no gap
func TestRealizedTotalStandsWhenAPositionsConversionBlockIsAbsent(t *testing.T) {
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

	src := createAccount(t, c, url, `{"name":"Старый брокер","type":"brokerage","currency":"USD"}`)
	acc := createAccount(t, c, url, `{"name":"Новый брокер","type":"brokerage","currency":"USD"}`)
	acme := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	beta := createInstrument(t, c, url, `{"type":"share","name":"Бета","ticker":"BETA","currency":"USD"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, src.ID, acme.ID, earlyBuyOn))
	createTransfer(t, c, url, fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,
		"quantity":"10","occurred_on":%q}`, src.ID, acc.ID, acme.ID, transferOn))
	if _, err := pool.Exec(t.Context(), `DELETE FROM operation_transfer_lots`); err != nil {
		t.Fatalf("drop the stored breakdown: %v", err)
	}

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, beta.ID, lateBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":"2026-07-28","quantity":"10","price":"150",
		"amount_minor":150000,"currency":"USD"}`, acc.ID, beta.ID))

	resp := accountPositions(t, c, url, acc.ID)

	// Pin the fixture: ACME really is the position whose whole conversion
	// block is gone, and really is held rather than sold.
	byTicker := make(map[string]positionResp, len(resp.Positions))
	for _, p := range resp.Positions {
		byTicker[p.Instrument.Ticker] = p
	}
	if !byTicker["ACME"].HasUndatedLots || byTicker["ACME"].InBase != nil {
		t.Fatalf("ACME = {has_undated_lots %v, in_base %v}, want {true, null} — the fixture is not testing what it means to",
			byTicker["ACME"].HasUndatedLots, byTicker["ACME"].InBase)
	}

	got := resp.RealizedTotal
	if got.InBaseGap != nil {
		t.Fatalf("in_base_gap = %q, want null. The lot that has no purchase date is still HELD; nothing was sold out of it, and the account's settled result is fully known", *got.InBaseGap)
	}
	if got.InBase == nil || *got.InBase != 4_500_000 {
		t.Errorf("in_base = %s, want 4500000. A settled result is not made unknowable by a position whose in_base block died of a missing basis", gapOf(got))
	}
}

// TestRealizedTotalIsTheSumOfTheFiguresItStandsOver pins the rounding decision,
// which is the one place this total could honestly have been computed two ways.
//
// Rounding once for the whole account — multiplying every disposal's terms as
// decimals and rounding the account's total at the end — is the more accurate
// single number, and it is deliberately NOT what this is. Each position's
// figure is itself published in this same response; the total is defined as
// their sum, and a header a minor unit away from the rows one field below it
// would be a number nobody could check. The residual is at most half a minor
// unit per position, and it errs the way НК РФ ст. 210 п. 5 does: a base is
// struck per disposal and the disposals are summed, not the reverse.
//
//	fx USD->RUB = 90.5 for every date, so this test says nothing about WHICH
//	  date's rate is used (that is pinned elsewhere) — only about rounding
//	two instruments, each bought for $123.45 and sold for $246.90
//	  per position: 24_690*90.5 - 12_345*90.5 = 1_117_222.5 -> 1_117_223
//
//	sum of the published figures:  1_117_223 + 1_117_223 = 2_234_446
//	rounded once for the account:  1_117_222.5 + 1_117_222.5 = 2_234_445
func TestRealizedTotalIsTheSumOfTheFiguresItStandsOver(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes, datedRate{earlyRateOn, "90.5"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	acme := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	beta := createInstrument(t, c, url, `{"type":"share","name":"Бета","ticker":"BETA","currency":"USD"}`)

	for _, id := range []string{acme.ID, beta.ID} {
		createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
			"occurred_on":%q,"quantity":"1","price":"123.45",
			"amount_minor":-12345,"currency":"USD"}`, acc.ID, id, earlyBuyOn))
		createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
			"occurred_on":%q,"quantity":"1","price":"246.90",
			"amount_minor":24690,"currency":"USD"}`, acc.ID, id, lateBuyOn))
	}

	resp := accountPositions(t, c, url, acc.ID)

	for _, p := range resp.Positions {
		if p.InBase == nil || p.InBase.RealizedPnlMinor == nil {
			t.Fatalf("%s.in_base.realized_pnl_minor = null, want 1117223 — the fixture is not testing what it means to", p.Instrument.Ticker)
		}
		if *p.InBase.RealizedPnlMinor != 1_117_223 {
			t.Fatalf("%s.in_base.realized_pnl_minor = %d, want 1117223 (1117222.5 rounded half away from zero) — the per-position rounding this total is built on changed",
				p.Instrument.Ticker, *p.InBase.RealizedPnlMinor)
		}
	}

	got := resp.RealizedTotal
	if got.InBase == nil {
		t.Fatalf("in_base = %s, want 2234446", gapOf(got))
	}
	if *got.InBase == 2_234_445 {
		t.Fatalf("in_base = 2234445 — that is the account rounded once from the raw terms. It is the more accurate number and it disagrees by a kopeck with the two figures published beside it in this very response")
	}
	if *got.InBase != 2_234_446 {
		t.Errorf("in_base = %d, want 2234446 (1117223 + 1117223)", *got.InBase)
	}
}

// TestRealizedTotalRefusesToPublishASumThatWouldWrap stands at the CALL SITE of
// the guard on realizedTotals.add, which the guard is only as good as: neutered,
// the addition wraps, the request finishes 200, and the account header shows a
// figure of the wrong magnitude and quite possibly the wrong sign — over rows
// that are each perfectly correct.
//
// The fixture is deliberately built in two steps. One position proves the term
// itself is publishable: ~5×10^18 kopecks is an ordinary int64 and the response
// carries it. The second position is its identical twin, and only their sum
// leaves the range — which is the whole claim, that a total of publishable
// figures need not be publishable.
//
// The rate is 5000 ₽/$ and no such rate exists. It is the shortest way to reach
// the edge from amounts the journal accepts: an operation is capped at 10^15
// minor units (operation.maxAmountMinor) and an fx rate is capped at nothing, so
// this is what the arithmetic actually looks like when the two meet. Real
// hyperinflation would take longer to write down and would fail here in exactly
// the same place.
func TestRealizedTotalRefusesToPublishASumThatWouldWrap(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes, datedRate{earlyRateOn, "5000"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	acme := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	beta := createInstrument(t, c, url, `{"type":"share","name":"Бета","ticker":"BETA","currency":"USD"}`)

	// No price on either leg: what this test is about is amount_minor, and a
	// price would only have to be kept consistent with it (see
	// operation.maxPrice) without changing anything here.
	sellAndBuy := func(instrumentID string) {
		createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
			"occurred_on":%q,"quantity":"1","amount_minor":-12345,"currency":"USD"}`, acc.ID, instrumentID, earlyBuyOn))
		createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
			"occurred_on":%q,"quantity":"1","amount_minor":1000000000000000,"currency":"USD"}`, acc.ID, instrumentID, lateBuyOn))
	}

	sellAndBuy(acme.ID)
	// (1_000_000_000_000_000 - 12_345) × 5000
	const onePosition = 4_999_999_999_938_275_000
	if got := accountPositions(t, c, url, acc.ID).RealizedTotal; got.InBase == nil || *got.InBase != onePosition {
		t.Fatalf("one position: in_base = %s, want %d — the fixture is not testing what it means to unless a single term is publishable",
			gapOf(got), int64(onePosition))
	}

	sellAndBuy(beta.ID)
	resp := do(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/positions", "")
	if resp.StatusCode != http.StatusInternalServerError {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("two positions summing past int64: GET positions = %d, want 500 — twice %d is not an int64 of kopecks, and the 200 carries a wrapped total under a header the owner reads as their money: %s",
			resp.StatusCode, int64(onePosition), b)
	}
}
