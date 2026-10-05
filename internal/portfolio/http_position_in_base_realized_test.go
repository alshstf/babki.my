package portfolio_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/testdb"
)

// Dates for the realized fixtures: purchase at the early rate, sale at the mid
// rate, today at the newest, so "own days", "sale day" and "today" give three
// different numbers.
const (
	midRateOn = "2026-05-01"
	sellOn    = "2026-05-10"
)

// A realized result is struck at its own days' rates: proceeds and fee at the
// sale's, basis at the purchase's (НК РФ ст. 210 п. 5).
//
//	USD->RUB 50 (02-01), 80 (05-01), 90 (07-01)
//	buy 10 @ $100 on 03-10 -> basis 100_000 at 50
//	sell 10 @ $120 on 05-10, fee $5 -> 120_000 and 500 at 80
//	realized USD 19_500; base 120_000×80 − 500×80 − 100_000×50 = 4_560_000
//
// Not: 1_755_000 (today), 1_560_000 (sale day), 4_600_000 (fee dropped).
func TestPositionInBaseRealizedUsesTheRatesOfItsOwnDays(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes,
		datedRate{earlyRateOn, "50"}, datedRate{midRateOn, "80"}, datedRate{lateRateOn, "90"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID, earlyBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":%q,"quantity":"10","price":"120",
		"amount_minor":120000,"fee_minor":500,"currency":"USD"}`, acc.ID, share.ID, sellOn))

	p := onlyPosition(t, c, url, acc.ID)

	// Pin the position-currency figures the arithmetic rests on.
	if realizedFigure(t, p.RealizedPnlMinor) != 19500 {
		t.Fatalf("realized_pnl_minor = %d, want 19500 (120000 - 500 - 100000, in USD)", realizedFigure(t, p.RealizedPnlMinor))
	}
	if p.InBase == nil {
		t.Fatalf("in_base = nil, want a converted object")
	}
	if p.InBase.RealizedPnlMinor == nil {
		t.Fatalf("in_base.realized_pnl_minor = nil, want 4560000: every date this sum needs has a rate")
	}
	switch got := *p.InBase.RealizedPnlMinor; got {
	case 1_755_000:
		t.Fatalf("in_base.realized_pnl_minor = 1755000 — that is the USD result times TODAY's rate (19500 * 90). A realized result is settled history; today's rate has nothing to do with it")
	case 1_560_000:
		t.Fatalf("in_base.realized_pnl_minor = 1560000 — that is the USD result times the SALE DAY's rate (19500 * 80). The proceeds belong to that day, but the basis belongs to %s, when the shares were bought", earlyBuyOn)
	case 4_600_000:
		t.Fatalf("in_base.realized_pnl_minor = 4600000 — that is the sale converted without its fee (120000*80 - 100000*50); the fee is paid on the day of the sale and comes off at that day's rate")
	default:
		if got != 4_560_000 {
			t.Errorf("in_base.realized_pnl_minor = %d, want 4560000 (120000*80 - 500*80 - 100000*50)", got)
		}
	}
	// Closed: the basis is a sum over no lots.
	if p.InBase.CostMinor != 0 {
		t.Errorf("in_base.cost_minor = %d, want 0 (everything was sold)", p.InBase.CostMinor)
	}
}

// A deal can profit in dollars and lose in roubles; no positive rate turns
// +10_000 negative.
//
//	USD->RUB 100, then 50
//	buy 10 @ $100 -> 10_000_000; sell 10 @ $110 -> 5_500_000
//	realized +10_000 USD, −4_500_000 RUB
func TestPositionInBaseRealizedProfitInPositionCurrencyLossInBase(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes, datedRate{earlyRateOn, "100"}, datedRate{midRateOn, "50"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID, earlyBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":%q,"quantity":"10","price":"110",
		"amount_minor":110000,"currency":"USD"}`, acc.ID, share.ID, sellOn))

	p := onlyPosition(t, c, url, acc.ID)

	if realizedFigure(t, p.RealizedPnlMinor) != 10000 {
		t.Fatalf("realized_pnl_minor = %d, want +10000 (a profit in USD)", realizedFigure(t, p.RealizedPnlMinor))
	}
	if p.InBase == nil {
		t.Fatalf("in_base = nil, want a converted object")
	}
	if p.InBase.RealizedPnlMinor == nil {
		t.Fatalf("in_base.realized_pnl_minor = nil, want -4500000")
	}
	if *p.InBase.RealizedPnlMinor != -4_500_000 {
		t.Errorf("in_base.realized_pnl_minor = %d, want -4500000 (110000*50 - 100000*100)", *p.InBase.RealizedPnlMinor)
	}
	// The point of the test, as its own assertion.
	if realizedFigure(t, p.RealizedPnlMinor) <= 0 || *p.InBase.RealizedPnlMinor >= 0 {
		t.Errorf("realized_pnl_minor = %d (USD) and in_base.realized_pnl_minor = %d (RUB): want opposite signs — a deal can be a profit in the position's currency and a loss in the base currency, and both answers must be published as they are",
			realizedFigure(t, p.RealizedPnlMinor), *p.InBase.RealizedPnlMinor)
	}
}

// A covered amortization is neutral in dollars and a real result in roubles,
// so no rate turns the native zero into the answer.
//
//	USD->RUB 50 (02-01), 80 (05-01)
//	buy 1 bond @ $1 000 on 03-10 -> basis 100_000 at 50
//	amortization $300 on 05-10 -> 30_000 at 80, retiring 30_000 bought at 50
//	realized 0 USD; base 900_000; cost 70_000 USD -> 3_500_000
func TestPositionInBaseRealizedIncludesAmortization(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes, datedRate{earlyRateOn, "50"}, datedRate{midRateOn, "80"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	bond := createInstrument(t, c, url, `{"type":"bond","name":"Облигация","ticker":"AMRT","currency":"USD"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"1","price":"1000",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, bond.ID, earlyBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"amortization",
		"occurred_on":%q,"amount_minor":30000,"currency":"USD"}`, acc.ID, bond.ID, sellOn))

	p := onlyPosition(t, c, url, acc.ID)

	// Pin the fixture: in USD this amortization really is a non-event.
	if realizedFigure(t, p.RealizedPnlMinor) != 0 {
		t.Fatalf("realized_pnl_minor = %d, want 0 — a covered amortization returns exactly what it retires, in the position's own currency", realizedFigure(t, p.RealizedPnlMinor))
	}
	if p.CostMinor != 70000 {
		t.Fatalf("cost_minor = %d, want 70000 (100000 retired by 30000, in USD)", p.CostMinor)
	}
	if p.InBase == nil {
		t.Fatalf("in_base = nil, want a converted object")
	}
	if p.InBase.RealizedPnlMinor == nil {
		t.Fatalf("in_base.realized_pnl_minor = nil, want 900000: every date this sum needs has a rate")
	}
	if *p.InBase.RealizedPnlMinor == 0 {
		t.Fatalf("in_base.realized_pnl_minor = 0 — the amortization was not counted as a disposal (or was converted at one rate, which for a covered amortization comes to the same zero). It returned 30000 at the rate of %s while retiring basis bought at the rate of %s, and that difference is a real result",
			sellOn, earlyBuyOn)
	}
	if *p.InBase.RealizedPnlMinor != 900_000 {
		t.Errorf("in_base.realized_pnl_minor = %d, want 900000 (30000*80 - 30000*50)", *p.InBase.RealizedPnlMinor)
	}
	// The retired basis left; the rest keeps its purchase-day rate.
	if p.InBase.CostMinor != 3_500_000 {
		t.Errorf("in_base.cost_minor = %d, want 3500000 (70000 * 50)", p.InBase.CostMinor)
	}
}

// The realized result is rounded once over every term of every disposal.
//
//	USD->RUB 90.5; two buys of $123.45, two sells of $246.90
//	terms 24_690×90.5 = 2_234_445.0 and −12_345×90.5 = −1_117_222.5, twice
//	once: 2_234_445; per event: 2_234_446; per term: 2_234_444
func TestPositionInBaseRealizedRoundsOnceForTheWholePosition(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes, datedRate{"2026-01-01", "90.5"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)

	for range 2 {
		createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
			"occurred_on":%q,"quantity":"1","price":"123.45",
			"amount_minor":-12345,"currency":"USD"}`, acc.ID, share.ID, lateBuyOn))
	}
	for _, on := range []string{"2026-07-15", "2026-07-16"} {
		createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
			"occurred_on":%q,"quantity":"1","price":"246.90",
			"amount_minor":24690,"currency":"USD"}`, acc.ID, share.ID, on))
	}

	p := onlyPosition(t, c, url, acc.ID)
	if realizedFigure(t, p.RealizedPnlMinor) != 24690 {
		t.Fatalf("realized_pnl_minor = %d, want 24690 (two disposals of 12345, in USD)", realizedFigure(t, p.RealizedPnlMinor))
	}
	if p.InBase == nil {
		t.Fatalf("in_base = nil, want a converted object")
	}
	if p.InBase.RealizedPnlMinor == nil {
		t.Fatalf("in_base.realized_pnl_minor = nil, want 2234445")
	}
	switch got := *p.InBase.RealizedPnlMinor; got {
	case 2_234_446:
		t.Fatalf("in_base.realized_pnl_minor = 2234446 — that is each disposal rounded before summing (1117223 twice); the position's figure is one published number and is rounded once, giving 2234445")
	case 2_234_444:
		t.Fatalf("in_base.realized_pnl_minor = 2234444 — that is each term rounded before summing (2234445 - 1117223, twice); nothing between the rate and the published figure may round")
	default:
		if got != 2_234_445 {
			t.Errorf("in_base.realized_pnl_minor = %d, want 2234445 (round(2*(24690*90.5 - 12345*90.5)))", got)
		}
	}
}

// A sold parcel with no acquisition date nulls only the realized figure; the
// rest of the object stands (unlike an undated lot still held).
//
//	source buys 10 @ $100 (03-10) and 10 @ $200 (07-10), transfers all 20
//	(breakdown dropped -> undated lot, cost 300_000)
//	destination buys 3 @ $40 (07-25) -> dated lot 12_000
//	destination sells 20 @ $200 (07-28), releasing the undated lot
//	realized USD 100_000; base null; cost 12_000 × 90 = 1_080_000
//
// Not 9_000_000 (dated by the sale) nor 36_000_000 (parcel dropped).
func TestPositionInBaseRealizedNullWhenAReleasedParcelHasNoAcquisitionDate(t *testing.T) {
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
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, from.ID, share.ID, earlyBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"200",
		"amount_minor":-200000,"currency":"USD"}`, from.ID, share.ID, lateBuyOn))
	createTransfer(t, c, url, fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,
		"quantity":"20","occurred_on":%q}`, from.ID, to.ID, share.ID, transferOn))

	// Make it a pre-breakdown transfer: basis kept, dates gone.
	if _, err := pool.Exec(t.Context(), `DELETE FROM operation_transfer_lots`); err != nil {
		t.Fatalf("drop the stored breakdown: %v", err)
	}

	// A dated lot of its own, so the surviving basis is real.
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-25","quantity":"3","price":"40",
		"amount_minor":-12000,"currency":"USD"}`, to.ID, share.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":"2026-07-28","quantity":"20","price":"200",
		"amount_minor":400000,"currency":"USD"}`, to.ID, share.ID))

	p := onlyPosition(t, c, url, to.ID)

	// Pin the fixture: the sale took the undated parcel and left the dated one.
	if p.Quantity != "3" || p.CostMinor != 12000 {
		t.Fatalf("destination position after the sale = {qty %q cost %d}, want {\"3\" 12000} — the queue must hand over the undated lot first",
			p.Quantity, p.CostMinor)
	}
	if realizedFigure(t, p.RealizedPnlMinor) != 100000 {
		t.Fatalf("realized_pnl_minor = %d, want 100000 (400000 - 300000, in USD)", realizedFigure(t, p.RealizedPnlMinor))
	}
	if p.HasUndatedLots {
		t.Fatalf("has_undated_lots = true, want false: the undated lot has been sold and is not among the lots still held — which is exactly why it cannot be the thing that explains the null below")
	}
	// has_undated_realizations is raised: no flag about held lots could name the
	// sold parcel.
	if !p.HasUndatedRealizations {
		t.Errorf("has_undated_realizations = false, want true: the sale above retired the undated parcel — a piece of basis whose acquisition date was never recorded — and that is exactly the condition this flag exists to report")
	}

	if p.InBase == nil {
		t.Fatalf("in_base = nil, want the object: only the realized figure is unstrikeable, and the basis, the income and the valuation never depended on a parcel that is already gone")
	}
	if p.InBase.RealizedPnlMinor != nil {
		switch got := *p.InBase.RealizedPnlMinor; got {
		case 9_000_000:
			t.Fatalf("in_base.realized_pnl_minor = 9000000 — the retired parcel was dated on the day of the SALE (100000 * 90). Nobody recorded a purchase date for it, and a figure invented from the wrong day is indistinguishable from a real one")
		case 36_000_000:
			t.Fatalf("in_base.realized_pnl_minor = 36000000 — the undatable parcel was dropped and the proceeds published alone (400000 * 90), as if the shares had cost nothing")
		default:
			t.Fatalf("in_base.realized_pnl_minor = %d, want null: the parcel this sale retired does not know when it was bought", got)
		}
	}
	if p.InBase.CostMinor != 1_080_000 {
		t.Errorf("in_base.cost_minor = %d, want 1080000 (the dated lot, 12000 * 90) — an unstrikeable realized figure must not take the rest of the object down with it", p.InBase.CostMinor)
	}
}

// oneDateConverter answers one flat rate except on one date, where it returns
// err — a hole under one disposal, which a real converter (nearest earlier
// date) cannot produce. The date is compared as YYYY-MM-DD.
type oneDateConverter struct {
	rate   decimal.Decimal
	rateOn time.Time
	on     string
	err    error
}

func (c oneDateConverter) Rate(_ context.Context, _, _ string, on time.Time) (decimal.Decimal, time.Time, error) {
	if on.Format("2006-01-02") == c.on {
		return decimal.Decimal{}, time.Time{}, c.err
	}
	return c.rate, c.rateOn, nil
}

// RatesOn answers from this double's Rate, so the hole is in the prewarm too.
func (c oneDateConverter) RatesOn(ctx context.Context, queries []marketdata.RateQuery) (marketdata.Rates, error) {
	return ratesFromRate(ctx, c, queries)
}

// realizedRateHoleAPI: a USD position bought 20 and sold 10, against a
// converter answering 90 except on the sale day.
//
//	buy 20 @ $100 (03-10) -> 200_000; sell 10 @ $120 (05-10) releases 100_000
//	held: 10 units, 100_000, dated 03-10
//
// Only the realized figure needs the sale day's rate.
func realizedRateHoleAPI(t *testing.T, err error) (url string, c *http.Client, accountID string) {
	t.Helper()
	pool := testdb.New(t)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c = setupAPI(t, pool, quotes, oneDateConverter{
		rate:   decimal.RequireFromString("90"),
		rateOn: mustDate(t, lateRateOn),
		on:     sellOn,
		err:    err,
	})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"20","price":"100",
		"amount_minor":-200000,"currency":"USD"}`, acc.ID, share.ID, earlyBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":%q,"quantity":"10","price":"120",
		"amount_minor":120000,"currency":"USD"}`, acc.ID, share.ID, sellOn))
	return url, c, acc.ID
}

// A missing rate on the sale day nulls the realized figure only.
//
//	realized USD 20_000; base null; cost 100_000 × 90 = 9_000_000
func TestPositionInBaseRealizedNullWhenTheDisposalDateHasNoRate(t *testing.T) {
	url, c, accountID := realizedRateHoleAPI(t, fmt.Errorf("%w: USD -> RUB on %s", marketdata.ErrNoRate, sellOn))

	p := onlyPosition(t, c, url, accountID)

	if realizedFigure(t, p.RealizedPnlMinor) != 20000 {
		t.Fatalf("realized_pnl_minor = %d, want 20000 (120000 - 100000, in USD)", realizedFigure(t, p.RealizedPnlMinor))
	}
	// Only a rate is missing, so has_undated_realizations stays false.
	if p.HasUndatedRealizations {
		t.Errorf("has_undated_realizations = true, want false: every parcel this position ever held or retired has a recorded acquisition date — only the fx rate for the day of the sale is missing")
	}
	if p.InBase == nil {
		t.Fatalf("in_base = nil, want the object: only the sale's own day lacks a rate, and the basis still held was bought on a day that has one")
	}
	if p.InBase.RealizedPnlMinor != nil {
		if *p.InBase.RealizedPnlMinor == 1_800_000 {
			t.Fatalf("in_base.realized_pnl_minor = 1800000 — that is the USD result times the rate that DID resolve (20000 * 90); the day the sale happened has no rate at all and the figure is not strikeable")
		}
		t.Fatalf("in_base.realized_pnl_minor = %d, want null: the day of the sale has no fx rate", *p.InBase.RealizedPnlMinor)
	}
	if p.InBase.CostMinor != 9_000_000 {
		t.Errorf("in_base.cost_minor = %d, want 9000000 (100000 * 90) — the basis is computed from its own dates and must survive a hole under the sale", p.InBase.CostMinor)
	}
}

// A missing rate under the purchase day of a sold parcel also nulls the
// realized figure; a real converter produces this hole when the buy predates
// every rate.
//
//	USD->RUB 80 from 05-01
//	buy 10 @ $100 on 01-05 (no rate); sell 10 @ $200 on 05-10
//	buy 3 @ $40 on 07-25 (held)
//	realized USD 100_000; base null; cost 12_000 × 80 = 960_000
func TestPositionInBaseRealizedNullWhenThePurchaseDateHasNoRate(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes, datedRate{midRateOn, "80"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-01-05","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":%q,"quantity":"10","price":"200",
		"amount_minor":200000,"currency":"USD"}`, acc.ID, share.ID, sellOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-25","quantity":"3","price":"40",
		"amount_minor":-12000,"currency":"USD"}`, acc.ID, share.ID))

	p := onlyPosition(t, c, url, acc.ID)

	if realizedFigure(t, p.RealizedPnlMinor) != 100000 {
		t.Fatalf("realized_pnl_minor = %d, want 100000 (200000 - 100000, in USD)", realizedFigure(t, p.RealizedPnlMinor))
	}
	// The date is on record; only the rate is missing, so the flag stays false.
	if p.HasUndatedRealizations {
		t.Errorf("has_undated_realizations = true, want false: the retired parcel's purchase date is recorded (2026-01-05) — only its fx rate is missing, which is not the condition this flag reports")
	}
	if p.InBase == nil {
		t.Fatalf("in_base = nil, want the object: only the retired parcel's own day lacks a rate, and the lot still held was bought on a day that has one")
	}
	if p.InBase.RealizedPnlMinor != nil {
		if *p.InBase.RealizedPnlMinor == 8_000_000 {
			t.Fatalf("in_base.realized_pnl_minor = 8000000 — that is the USD result times the one rate that DID resolve (100000 * 80); the day the shares were bought has no rate at all and the figure is not strikeable")
		}
		t.Fatalf("in_base.realized_pnl_minor = %d, want null: the day the retired parcel was bought has no fx rate", *p.InBase.RealizedPnlMinor)
	}
	if p.InBase.CostMinor != 960_000 {
		t.Errorf("in_base.cost_minor = %d, want 960000 (12000 * 80) — the basis still held is computed from its own date and must survive a hole under a parcel already sold", p.InBase.CostMinor)
	}
}

// A real failure on the sale day's rate fails the request instead of
// publishing a null.
func TestPositionInBaseRealizedRateErrorFailsRequest(t *testing.T) {
	url, c, accountID := realizedRateHoleAPI(t, errors.New("connection reset by peer"))

	resp := do(t, c, "GET", url+"/api/v1/accounts/"+accountID+"/positions", "")
	if resp.StatusCode != http.StatusInternalServerError {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET positions with a failing rate lookup on the day of a sale = %d, want 500 — a real outage must not be served as a 200 with in_base.realized_pnl_minor: null: %s",
			resp.StatusCode, b)
	}
}

// A dollar bond redeemed for roubles has no native result (null) but an exact
// rouble one: the roubles need no rate, the basis takes its purchase day's.
//
//	USD->RUB 50 (02-01), 80 (05-01), 90 (07-01)
//	buy 10 @ $100 on 03-10 -> 100_000 at 50
//	sell 10 for 60 000 ₽ on 05-10
//	base 6_000_000 − 100_000×50 = 1_000_000
//
// Not −2_000_000 (sale-day basis) nor −3_000_000 (today's).
func TestRealizedInBaseSurvivesASaleSettledInAnotherCurrency(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes,
		datedRate{earlyRateOn, "50"}, datedRate{midRateOn, "80"}, datedRate{lateRateOn, "90"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	bond := createInstrument(t, c, url,
		`{"type":"bond","name":"Облигация","ticker":"BOND1","currency":"USD","face_value_minor":100000,"face_currency":"USD"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, bond.ID, earlyBuyOn))
	// The redemption settles in roubles.
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":%q,"quantity":"10","amount_minor":6000000,"currency":"RUB"}`,
		acc.ID, bond.ID, sellOn))

	p := onlyPosition(t, c, url, acc.ID)

	if p.Currency != "USD" {
		t.Fatalf("currency = %q, want USD — the purchase settles it and the ruble redemption does not", p.Currency)
	}
	if p.RealizedPnlMinor != nil {
		t.Errorf("realized_pnl_minor = %d, want null: 6 000 000 kopecks against a basis of 100 000 cents is a result in neither",
			*p.RealizedPnlMinor)
	}
	if p.InBase == nil || p.InBase.RealizedPnlMinor == nil {
		t.Fatalf("in_base.realized_pnl_minor is absent, want 1000000: every term is present and dated, so the ruble answer exists (in_base = %+v, gap = %v)",
			p.InBase, p.InBaseGap)
	}
	if got := *p.InBase.RealizedPnlMinor; got != 1_000_000 {
		t.Errorf("in_base.realized_pnl_minor = %d, want 1000000 (6000000 - 100000*50)", got)
	}

	// The USD bucket of the account total is not a fake zero.
	total := realizedTotalOf(t, c, url, acc.ID)
	if len(total.ByCurrency) != 1 || total.ByCurrency[0].Currency != "USD" {
		t.Fatalf("by_currency = %+v, want one USD bucket", total.ByCurrency)
	}
	if total.ByCurrency[0].RealizedPnlMinor != nil {
		t.Errorf("by_currency[USD].realized_pnl_minor = %d, want null — its only position has no figure in one currency",
			*total.ByCurrency[0].RealizedPnlMinor)
	}
	// The base total is unaffected.
	if total.InBase == nil || *total.InBase != 1_000_000 {
		t.Errorf("in_base = %v, want 1000000 — the ruble answer survives what the dollar answer cannot", total.InBase)
	}
}

// A rouble bond sold for dollars publishes its realized result in in_base,
// though the position is in the base currency.
//
//	buy 10 for 100 000 ₽ (03-10); sell 10 for $2 000 (05-10, rate 80)
//	base 200_000×80 − 10_000_000 = 6_000_000 (today's rate would give 8_000_000)
func TestARubleBondSoldForDollarsStillPublishesItsResult(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes,
		datedRate{earlyRateOn, "50"}, datedRate{midRateOn, "80"}, datedRate{lateRateOn, "90"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME2","currency":"RUB"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","amount_minor":-10000000,"currency":"RUB"}`,
		acc.ID, share.ID, earlyBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":%q,"quantity":"10","amount_minor":200000,"currency":"USD"}`,
		acc.ID, share.ID, sellOn))

	p := onlyPosition(t, c, url, acc.ID)

	if p.Currency != "RUB" {
		t.Fatalf("currency = %q, want RUB", p.Currency)
	}
	if p.RealizedPnlMinor != nil {
		t.Errorf("realized_pnl_minor = %d, want null — $2000 against a basis in kopecks is a result in neither",
			*p.RealizedPnlMinor)
	}
	if p.InBase == nil || p.InBase.RealizedPnlMinor == nil {
		t.Fatalf("in_base.realized_pnl_minor is absent, want 6000000: without it this position's realized result is published nowhere at all (in_base = %+v, gap = %v)",
			p.InBase, p.InBaseGap)
	}
	if got := *p.InBase.RealizedPnlMinor; got != 6_000_000 {
		t.Errorf("in_base.realized_pnl_minor = %d, want 6000000 (200000*80 - 10000000)", got)
	}
	// The rest is the native figures under the same sign.
	if p.InBase.CostMinor != 0 {
		t.Errorf("in_base.cost_minor = %d, want 0 — everything was sold", p.InBase.CostMinor)
	}
}

// Decision Р-3 (НК РФ ст. 210 п. 5): the settlement day's rate when known.
//
//	buy 04-30, settled 05-01 -> basis at 80, not 50
//	sell 06-30, settled 07-01 -> proceeds and fee at 90, not 80
//	base 120_000×90 − 500×90 − 100_000×80 = 2_755_000 (trade days: 4_560_000)
func TestPositionInBaseRealizedUsesTheSettlementDays(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes,
		datedRate{earlyRateOn, "50"}, datedRate{midRateOn, "80"}, datedRate{lateRateOn, "90"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-04-30","settled_on":"2026-05-01","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":"2026-06-30","settled_on":"2026-07-01","quantity":"10","price":"120",
		"amount_minor":120000,"fee_minor":500,"currency":"USD"}`, acc.ID, share.ID))

	p := onlyPosition(t, c, url, acc.ID)
	if p.InBase == nil || p.InBase.RealizedPnlMinor == nil {
		t.Fatalf("in_base.realized_pnl_minor = nil, want 2755000")
	}
	switch got := *p.InBase.RealizedPnlMinor; got {
	case 4_560_000:
		t.Errorf("in_base.realized_pnl_minor = 4560000 — the trade days' rates; the settlement days were given")
	case 2_755_000:
	default:
		t.Errorf("in_base.realized_pnl_minor = %d, want 2755000 (120000*90 - 500*90 - 100000*80)", got)
	}
}

// Held shares are valued at their purchase's settlement-day rate, kept when
// they move between accounts.
//
//	buy 04-30 (50), settled 05-01 (80): cost 100_000 × 80 = 8_000_000
func TestPositionInBaseCostUsesTheSettlementDayAndKeepsItOnAMove(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes,
		datedRate{earlyRateOn, "50"}, datedRate{midRateOn, "80"}, datedRate{lateRateOn, "90"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	other := createAccount(t, c, url, `{"name":"Другой","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-04-30","settled_on":"2026-05-01","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID))

	if p := onlyPosition(t, c, url, acc.ID); p.InBase == nil || p.InBase.CostMinor != 8_000_000 {
		t.Fatalf("in_base = %+v, want cost 8000000 — the settlement day's rate, not the trade day's 5000000", p.InBase)
	}
	createTransfer(t, c, url, fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,
		"quantity":"10","occurred_on":"2026-06-01"}`, acc.ID, other.ID, share.ID))
	if p := onlyPosition(t, c, url, other.ID); p.InBase == nil || p.InBase.CostMinor != 8_000_000 {
		t.Errorf("after the move in_base = %+v, want cost 8000000 — the parcel keeps its settlement day", p.InBase)
	}
}

// Dollars that arrived at 50 and left for a purchase settled at 80 banked the
// dollar's move up to the settlement day.
func TestCashResultUsesTheSettlementDayOfWhatSpentIt(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes,
		datedRate{earlyRateOn, "50"}, datedRate{midRateOn, "80"}, datedRate{lateRateOn, "90"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"deposit",
		"occurred_on":"2026-03-10","amount_minor":100000,"currency":"USD"}`, acc.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-04-30","settled_on":"2026-05-01","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID))

	usd := cashOf(t, accountPositions(t, c, url, acc.ID), "USD")
	if usd.InBase.RealizedPnlMinor == nil || *usd.InBase.RealizedPnlMinor != 3_000_000 {
		t.Errorf("the dollars' banked result is %v, want 3000000 — they left at the settlement day's 80, having arrived at 50", usd.InBase.RealizedPnlMinor)
	}
}
