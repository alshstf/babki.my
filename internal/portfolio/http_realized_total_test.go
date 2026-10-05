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

// The account's realized total is summed by the server so the figure has one
// definition: what it adds, refuses, and says when it refuses.

// accountPositions fetches the whole positions payload, where the total
// lives.
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

// gapOf renders in_base and its gap as one string for messages.
func gapOf(rt realizedTotalResp) string {
	if rt.InBase != nil {
		return fmt.Sprintf("%d", *rt.InBase)
	}
	if rt.InBaseGap == nil {
		return "null (no gap named)"
	}
	return fmt.Sprintf("null (%s)", *rt.InBaseGap)
}

// One figure per currency, never one integer of two (15_000 USD + 2_000 EUR
// would be 17_000 of nothing). An account without positions gets an empty
// by_currency, not a zero.
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
	// +2_000 EUR in the same account: a position's currency is its operations'.
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
	if realizedFigure(t, got.ByCurrency[0].RealizedPnlMinor) != 2_000 {
		t.Errorf("EUR total = %d, want 2000 (12000 - 10000)", realizedFigure(t, got.ByCurrency[0].RealizedPnlMinor))
	}
	switch usd := realizedFigure(t, got.ByCurrency[1].RealizedPnlMinor); usd {
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

// The base total is the sum of the positions' own base figures.
//
//	USD->RUB 50 (02-01), 80 (05-01), 90 (07-01)
//	ACME buy 10 @ $100 (03-10), sell 10 @ $120 fee $5 (05-10): 4_560_000
//	BETA buy 10 @ $50 (03-10), sell 10 @ $60 (05-10):           2_300_000
//	account in_base = 6_860_000
//
// Scaled answers: 2_655_000 (today), 2_360_000 (sale day).
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

	// The total is the sum of the per-position figures in the same response.
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

// A base-currency position has no in_base block, but its realized result is
// the base figure and must count.
//
//	USD->RUB 90
//	ACME (USD) buy $1 000, sell $1 200: 1_800_000
//	RUBL (RUB) from a transfer without dates, sold for 1 300 ₽: 30_000
//	account in_base = 1_830_000
//
// The rouble position's undated parcel catches a total that converts without
// asking whether it must.
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

	// The rouble position has no in_base (nothing to convert), and its sold parcel
	// really is undated.
	for _, p := range resp.Positions {
		if p.Instrument.Ticker != "RUBL" {
			continue
		}
		if p.InBase != nil {
			t.Fatalf("RUBL.in_base = %+v, want null: it is already in the base currency", *p.InBase)
		}
		if !p.HasUndatedRealizations || realizedFigure(t, p.RealizedPnlMinor) != 30_000 {
			t.Fatalf("RUBL = {has_undated_realizations %v, realized_pnl_minor %d}, want {true, 30000} — the fixture is not testing what it means to",
				p.HasUndatedRealizations, realizedFigure(t, p.RealizedPnlMinor))
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

// A missing rate withholds the total (no_rate: it will come); a missing
// purchase day leaves the position out and counts it (#195, #158).
//
//	USD->RUB 60 (02-01), 90 (07-01)
//	undated — shares in by a transfer without dates, sold
//	norate  — bought 2026-01-05, before the rates, sold
//	both    — one of each
//	mixed   — undated plus an ordinary $100 -> $150 at 90 = 4 500 000
func TestRealizedTotalLeavesOutWhatCanNeverBeValuedAndWaitsForWhatCan(t *testing.T) {
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
	mixed := createAccount(t, c, url, `{"name":"Без дат и обычная","type":"brokerage","currency":"USD"}`)
	acme := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	beta := createInstrument(t, c, url, `{"type":"share","name":"Бета","ticker":"BETA","currency":"USD"}`)
	gamma := createInstrument(t, c, url, `{"type":"share","name":"Гамма","ticker":"GAMMA","currency":"USD"}`)
	delta := createInstrument(t, c, url, `{"type":"share","name":"Дельта","ticker":"DELTA","currency":"USD"}`)
	omega := createInstrument(t, c, url, `{"type":"share","name":"Омега","ticker":"OMEGA","currency":"USD"}`)

	for _, in := range []struct{ instrument, to string }{{acme.ID, undated.ID}, {beta.ID, both.ID}, {delta.ID, mixed.ID}} {
		createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
			"occurred_on":%q,"quantity":"10","price":"100",
			"amount_minor":-100000,"currency":"USD"}`, src.ID, in.instrument, earlyBuyOn))
		createTransfer(t, c, url, fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,
			"quantity":"10","occurred_on":%q}`, src.ID, in.to, in.instrument, transferOn))
	}
	// Make the transfers pre-breakdown: basis kept, dates gone.
	if _, err := pool.Exec(t.Context(), `DELETE FROM operation_transfer_lots`); err != nil {
		t.Fatalf("drop the stored breakdowns: %v", err)
	}
	for _, sale := range []struct{ account, instrument string }{{undated.ID, acme.ID}, {both.ID, beta.ID}, {mixed.ID, delta.ID}} {
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
	// The ordinary pair beside the undated one.
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-10","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, mixed.ID, omega.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":"2026-07-28","quantity":"10","price":"150",
		"amount_minor":150000,"currency":"USD"}`, mixed.ID, omega.ID))

	base := func(v int64) *int64 { return &v }
	gap := func(v string) *string { return &v }
	for _, tc := range []struct {
		name        string
		account     string
		wantInBase  *int64
		wantGap     *string
		wantUndated int
		wantNative  int64
		why         string
	}{
		{
			"undated", undated.ID, base(0), nil, 1, 100_000,
			"the only position is the one nobody can date: it is left out and counted, and what is left comes to nought — said as a figure, not withheld for ever",
		},
		{
			"norate", norate.ID, nil, gap("no_rate"), 0, 10_000,
			"every date is known and the fx table simply does not reach 2026-01-05 yet — the backfill closes that gap on its own, so the figure waits for it",
		},
		{
			"both", both.ID, nil, gap("no_rate"), 1, 110_000,
			"a rate is still on its way, so the figure waits; the position that will never be valued is counted all the same",
		},
		{
			"mixed", mixed.ID, base(4_500_000), nil, 1, 150_000,
			"the ordinary position is valued in full and the undated one is left out and counted — one unrecorded date must not cost the account its whole figure",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := accountPositions(t, c, url, tc.account).RealizedTotal

			switch {
			case tc.wantInBase == nil && got.InBase != nil:
				t.Errorf("in_base = %d, want null: %s", *got.InBase, tc.why)
			case tc.wantInBase != nil && got.InBase == nil:
				t.Errorf("in_base = null (%s), want %d: %s", gapOf(got), *tc.wantInBase, tc.why)
			case tc.wantInBase != nil && *got.InBase != *tc.wantInBase:
				t.Errorf("in_base = %d, want %d: %s", *got.InBase, *tc.wantInBase, tc.why)
			}
			switch {
			case tc.wantGap == nil && got.InBaseGap != nil:
				t.Errorf("in_base_gap = %q, want null: %s", *got.InBaseGap, tc.why)
			case tc.wantGap != nil && (got.InBaseGap == nil || *got.InBaseGap != *tc.wantGap):
				t.Errorf("in_base_gap = %s, want %q: %s", gapOf(got), *tc.wantGap, tc.why)
			}
			if got.UndatedPositions != tc.wantUndated {
				t.Errorf("undated_positions = %d, want %d: %s", got.UndatedPositions, tc.wantUndated, tc.why)
			}
			// Nothing was bought for nothing.
			if got.UnknownCostPositions != 0 {
				t.Errorf("unknown_cost_positions = %d, want 0: every sale here knew its cost", got.UnknownCostPositions)
			}
			// The native currency always has a complete answer.
			if len(got.ByCurrency) != 1 || realizedFigure(t, got.ByCurrency[0].RealizedPnlMinor) != tc.wantNative {
				t.Errorf("by_currency = %+v, want one USD entry of %d — the by-currency form has no gaps and must not be withheld along with the converted one", got.ByCurrency, tc.wantNative)
			}
		})
	}
}

// The total stands when a position's in_base object is absent for reasons
// unrelated to disposals (here an undated held lot).
//
//	USD->RUB 60 (02-01), 90 (07-01)
//	ACME — undated held lot, never sold: in_base null, realized 0
//	BETA — $100 -> $150 after 07-01: 4_500_000
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

	// ACME's whole block is gone and it is held, not sold.
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

// The total is the exact sum of the rounded per-position figures, so the
// header matches its rows (and, like НК РФ ст. 210 п. 5, sums per-disposal
// bases).
//
//	USD->RUB 90.5; two instruments bought $123.45, sold $246.90
//	each 1_117_222.5 -> 1_117_223; sum 2_234_446 (once for the account: 2_234_445)
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

// Two positions each with a publishable ~5×10^18 figure sum past int64; the
// request fails rather than publishing a wrapped header. The 5000 ₽/$ rate is
// fictional: amounts are capped, rates are not.
func TestRealizedTotalRefusesToPublishASumThatWouldWrap(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes, datedRate{earlyRateOn, "5000"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	acme := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	beta := createInstrument(t, c, url, `{"type":"share","name":"Бета","ticker":"BETA","currency":"USD"}`)

	// No price: only amount_minor matters here.
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

// «Зафиксировано» is realized result plus income.
func TestSettledIsRealizedPlusIncome(t *testing.T) {
	url, c := newAPI(t)
	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	bond := createInstrument(t, c, url,
		`{"type":"bond","name":"Селектел","ticker":"SEL1","currency":"RUB","face_value_minor":100000,"face_currency":"RUB"}`)

	// The owner's own bond: 200 for 197 980,70 ₽ with 78,88 ₽ commission, one
	// coupon of 13 264,00 ₽, redeemed at par.
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-02-20","quantity":"200","amount_minor":-19798070,"fee_minor":7888,"currency":"RUB"}`, acc.ID, bond.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"coupon",
		"occurred_on":"2026-08-14","amount_minor":1326400,"currency":"RUB"}`, acc.ID, bond.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"redemption",
		"occurred_on":"2026-08-14","quantity":"200","amount_minor":20000000,"currency":"RUB"}`, acc.ID, bond.ID))

	p := onlyPosition(t, c, url, acc.ID)

	// 200 000,00 − 197 980,70 − 78,88 = 1 940,42 ₽
	if got := realizedFigure(t, p.RealizedPnlMinor); got != 194042 {
		t.Fatalf("realized_pnl_minor = %d, want 194042", got)
	}
	if p.IncomeMinor != 1326400 {
		t.Fatalf("income_minor = %d, want 1326400", p.IncomeMinor)
	}
	// 1 940,42 + 13 264,00 = 15 204,42 ₽, as a literal.
	if p.SettledMinor == nil || *p.SettledMinor != 1520442 {
		t.Errorf("settled_minor = %v, want 1520442 (1940,42 ₽ realized plus 13 264,00 ₽ of coupon)", p.SettledMinor)
	}
}

// Settled is null when income arrived in another currency: a sum named "all of
// it" would miss a term.
func TestSettledIsWithheldWhenIncomeArrivedInAnotherCurrency(t *testing.T) {
	url, c := newAPI(t)
	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	bond := createInstrument(t, c, url,
		`{"type":"bond","name":"Юаневая","ticker":"CNY1","currency":"USD","face_value_minor":100000,"face_currency":"USD"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-02-20","quantity":"10","amount_minor":-100000,"currency":"USD"}`, acc.ID, bond.ID))
	// The coupon arrives in rubles, as a yuan bond's does on a Russian broker.
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"coupon",
		"occurred_on":"2026-08-14","amount_minor":500000,"currency":"RUB"}`, acc.ID, bond.ID))

	p := onlyPosition(t, c, url, acc.ID)

	if p.IncomeMinor != 0 {
		t.Fatalf("income_minor = %d, want 0 — nothing was paid in the position's own currency", p.IncomeMinor)
	}
	if p.SettledMinor != nil {
		t.Errorf("settled_minor = %d, want null: the ruble coupon is income this sum cannot see", *p.SettledMinor)
	}
}

// The account tax figure carries only tax charged to the account; tax tied to
// a paper is already in that position's income.
func TestAccountTaxIsTheWithholdingNoPositionSees(t *testing.T) {
	url, c := newAPI(t)
	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-02-20","quantity":"10","amount_minor":-100000,"currency":"RUB"}`, acc.ID, share.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend",
		"occurred_on":"2026-03-01","amount_minor":10000,"currency":"RUB"}`, acc.ID, share.ID))
	// Withheld from that dividend: already inside the position's income.
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"tax",
		"occurred_on":"2026-03-01","amount_minor":-1300,"currency":"RUB"}`, acc.ID, share.ID))
	// Withheld from the account when money was taken out: nothing sees this.
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"tax",
		"occurred_on":"2026-08-14","amount_minor":-1058800,"currency":"RUB"}`, acc.ID))

	total := realizedTotalOf(t, c, url, acc.ID)
	if len(total.TaxWithheldByCurrency) != 1 {
		t.Fatalf("tax_withheld_by_currency = %+v, want one entry", total.TaxWithheldByCurrency)
	}
	got := total.TaxWithheldByCurrency[0]
	if got.Currency != "RUB" || got.AmountMinor != -1058800 {
		t.Errorf("tax = %s %d, want RUB -1058800 — the account's own withholding, and not the 1300 already inside the position's income",
			got.Currency, got.AmountMinor)
	}

	p := onlyPosition(t, c, url, acc.ID)
	// 100,00 ₽ of dividend less 13,00 ₽ of tax.
	if p.IncomeMinor != 8700 {
		t.Errorf("income_minor = %d, want 8700 — net of the tax attributed to this paper", p.IncomeMinor)
	}
}

// Shares passed on without their cost count as bought for nothing (#195), in
// roubles too, and say so on the paper and both totals. Nought needs no date,
// so they are not left out as undated.
func TestAPaperThatArrivedWithNoPriceCountsAsBoughtForNothingInEveryCurrency(t *testing.T) {
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
	dst := createAccount(t, c, url, `{"name":"Новый брокер","type":"brokerage","currency":"USD"}`)
	acme := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	acmeID, err := uuid.Parse(acme.ID)
	if err != nil {
		t.Fatalf("parse instrument id: %v", err)
	}
	quotes.byInstrument[acmeID] = marketdata.Quote{
		InstrumentID: acmeID, On: mustDate(t, "2026-07-28"),
		Price: decimal.RequireFromString("250"), Currency: "USD",
	}

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, src.ID, acme.ID, earlyBuyOn))
	// The new broker was told nothing about the price: a basis of nought.
	createTransfer(t, c, url, fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,
		"quantity":"10","occurred_on":%q,"cost_minor":0}`, src.ID, dst.ID, acme.ID, transferOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":"2026-07-28","quantity":"4","price":"200",
		"amount_minor":80000,"currency":"USD"}`, dst.ID, acme.ID))

	got := accountPositions(t, c, url, dst.ID)

	// The realized result: all 800 $ of the sale, at 90 ₽ on the day of it.
	rt := got.RealizedTotal
	if len(rt.ByCurrency) != 1 || realizedFigure(t, rt.ByCurrency[0].RealizedPnlMinor) != 80_000 {
		t.Errorf("by_currency = %+v, want one USD entry of 80000 — the whole proceeds, the cost being nought", rt.ByCurrency)
	}
	if rt.InBase == nil || *rt.InBase != 7_200_000 {
		t.Errorf("in_base = %s, want 7200000 (80000 × 90): nought needs no date, so the sale is not left out", gapOf(rt))
	}
	if rt.UndatedPositions != 0 {
		t.Errorf("undated_positions = %d, want 0: a parcel with no price is not one that needs a date", rt.UndatedPositions)
	}
	if rt.UnknownCostPositions != 1 {
		t.Errorf("unknown_cost_positions = %d, want 1: the result is higher than the truth by what was really paid, and only this says so", rt.UnknownCostPositions)
	}

	// The sale plus six cost-free shares at $250, valued at today's 90 ₽.
	at := got.AccountTotal
	if at.InBase == nil || *at.InBase != 7_200_000+13_500_000 {
		t.Errorf("account in_base = %v, want 20700000 (7200000 realized + 150000 × 90 held): the holding with no price is in the figure, not left out", at.InBase)
	}
	if at.UndatedPositions != 0 || at.UnknownCostPositions != 1 {
		t.Errorf("account undated/unknown_cost = %d/%d, want 0/1", at.UndatedPositions, at.UnknownCostPositions)
	}

	// And the paper itself says it.
	if len(got.Positions) != 1 {
		t.Fatalf("positions = %d, want 1", len(got.Positions))
	}
	p := got.Positions[0]
	if !p.HasUnknownCost {
		t.Error("has_unknown_cost = false on the paper that arrived with no price")
	}
	if p.HasUndatedLots || p.HasUndatedRealizations {
		t.Errorf("has_undated_lots/realizations = %v/%v, want false/false: nothing here needs a date", p.HasUndatedLots, p.HasUndatedRealizations)
	}
	if p.InBaseGap != nil {
		t.Errorf("in_base_gap = %q, want null: the position converts in full", *p.InBaseGap)
	}

	// The account it came from paid real money and sold nothing.
	if from := accountPositions(t, c, url, src.ID); from.AccountTotal.UnknownCostPositions != 0 {
		t.Errorf("the source account counts %d positions with no price, want 0", from.AccountTotal.UnknownCostPositions)
	}
}
