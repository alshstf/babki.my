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

// Dates the realized-profit fixtures below share. They are laid out so that a
// disposal falls strictly BETWEEN the rate that values its basis and the rate
// that values "today": a purchase on earlyBuyOn resolves to earlyRateOn's rate,
// the disposal on sellOn to midRateOn's, and today's lookup to the newest row
// the fixture seeds. Three distinct rates are what make "at the rates of its own
// days", "times the sale day's rate" and "times today's rate" three different
// numbers — with only two, two of the three answers coincide and a test cannot
// tell the implementations apart.
const (
	midRateOn = "2026-05-01"
	sellOn    = "2026-05-10"
)

// TestPositionInBaseRealizedUsesTheRatesOfItsOwnDays is the core of this
// change. A settled result is struck at the rates of the days it actually
// happened on: the proceeds and the fee at the day of the sale, the basis at the
// day the shares were BOUGHT (НК РФ ст. 210 п. 5). It is therefore not the
// position-currency result times any one rate, and the fixture is built so that
// the two rates a wrong implementation would reach for produce two other,
// visibly different numbers.
//
//	fx USD->RUB: 50 from 2026-02-01, 80 from 2026-05-01, 90 from 2026-07-01
//	  (nothing newer is seeded, so today's lookup resolves to 90)
//	buy  10 @ $100.00 on 2026-03-10 -> lot cost 100_000 minor USD, rate 50
//	sell 10 @ $120.00 on 2026-05-10, fee $5.00 -> proceeds 120_000, fee 500, rate 80
//
//	realized_pnl_minor (USD)  = 120_000 - 500 - 100_000            =    19_500
//	in_base.realized_pnl_minor = 120_000*80 - 500*80 - 100_000*50  = 4_560_000
//
// The three numbers this must NOT be, each asserted by name:
//
//	19_500 * 90 = 1_755_000  the USD result at TODAY's rate
//	19_500 * 80 = 1_560_000  the USD result at the SALE DAY's rate — the subtler
//	                         mistake: it dates the whole result correctly for a
//	                         tax authority and still values the basis on the
//	                         wrong day
//	4_600_000                the same computation with the fee dropped
//
// 4_560_000 is 2.9 times the sale-day answer. Almost none of that is the
// instrument: of the 4_560_000 rubles, 1_560_000 came from the shares rising in
// dollars and 3_000_000 from the dollar rising from 50 to 80 between the day
// they were bought and the day they were sold (100_000 * (80-50)).
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
	if p.RealizedPnlMinor != 19500 {
		t.Fatalf("realized_pnl_minor = %d, want 19500 (120000 - 500 - 100000, in USD)", p.RealizedPnlMinor)
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
	// The other figures answer their own questions from their own dates and are
	// untouched by any of this: the position is fully closed, so its basis is a
	// sum over no lots.
	if p.InBase.CostMinor != 0 {
		t.Errorf("in_base.cost_minor = %d, want 0 (everything was sold)", p.InBase.CostMinor)
	}
}

// TestPositionInBaseRealizedProfitInPositionCurrencyLossInBase pins the
// consequence the owner accepted for unrealized profit and which holds just as
// firmly once the deal is done: a sale can be a profit measured in the
// position's own currency and a LOSS measured in rubles. The dollars went up;
// the dollar went down harder. Both answers are honest answers to two different
// questions — "did the instrument gain" and "did the deal grow my rubles" — and
// a version that kept the signs in step would be hiding the currency loss from
// the person who took it.
//
// This is also the one arrangement where the realized figure cannot be mistaken
// for a scaled copy of the position-currency one: no positive rate turns +10_000
// into a negative number.
//
//	fx USD->RUB: 100 from 2026-02-01, 50 from 2026-05-01 (the ruble doubles)
//	buy  10 @ $100.00 on 2026-03-10 -> basis 100_000 minor USD at 100 = 10_000_000
//	sell 10 @ $110.00 on 2026-05-10 -> proceeds 110_000 at 50         =  5_500_000
//
//	realized_pnl_minor (USD)   =    110_000 -    100_000 =    +10_000  (profit)
//	in_base.realized_pnl_minor =  5_500_000 - 10_000_000 = -4_500_000  (loss)
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

	if p.RealizedPnlMinor != 10000 {
		t.Fatalf("realized_pnl_minor = %d, want +10000 (a profit in USD)", p.RealizedPnlMinor)
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
	// The point of the test, stated as its own assertion so the failure message
	// says what broke rather than merely which number moved.
	if p.RealizedPnlMinor <= 0 || *p.InBase.RealizedPnlMinor >= 0 {
		t.Errorf("realized_pnl_minor = %d (USD) and in_base.realized_pnl_minor = %d (RUB): want opposite signs — a deal can be a profit in the position's currency and a loss in the base currency, and both answers must be published as they are",
			p.RealizedPnlMinor, *p.InBase.RealizedPnlMinor)
	}
}

// TestPositionInBaseRealizedIncludesAmortization covers the second kind of
// disposal, and it is the case that makes the whole design visible: a covered
// return of principal is EXACTLY neutral in the position's own currency, and is
// nonetheless a real result in rubles. The principal comes back at the rate of
// the day it was paid while the basis it retires was struck at the rate of the
// day the bond was bought, and the gap between those two rates is money.
//
// Because the position-currency result is zero, no rate whatsoever turns it into
// the right answer: an implementation that folds only sales into this figure
// prints 0 here, and 0 is exactly "realized_pnl_minor times any rate you like".
//
//	fx USD->RUB: 50 from 2026-02-01, 80 from 2026-05-01
//	buy 1 bond @ $1000.00 on 2026-03-10 -> basis 100_000 minor USD, rate 50
//	amortization $300.00 on 2026-05-10  -> returns 30_000 minor USD, rate 80,
//	  retiring 30_000 of basis bought on 2026-03-10
//
//	realized_pnl_minor (USD)   = 30_000 - 30_000        =         0
//	in_base.realized_pnl_minor = 30_000*80 - 30_000*50  =   900_000
//	cost_minor (USD)           = 100_000 - 30_000       =    70_000
//	in_base.cost_minor         = 70_000 * 50            = 3_500_000
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
	if p.RealizedPnlMinor != 0 {
		t.Fatalf("realized_pnl_minor = %d, want 0 — a covered amortization returns exactly what it retires, in the position's own currency", p.RealizedPnlMinor)
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
	// The retired basis really left, and what remains is still valued at the
	// rate of the day it was bought.
	if p.InBase.CostMinor != 3_500_000 {
		t.Errorf("in_base.cost_minor = %d, want 3500000 (70000 * 50)", p.InBase.CostMinor)
	}
}

// TestPositionInBaseRealizedRoundsOnceForTheWholePosition pins WHERE the
// rounding happens. The published quantity is one number per position, so it is
// rounded once, at the end, over every term of every disposal — the same
// contract cost_minor and income_minor already keep (see the handler's
// sumInBase, and TestPositionInBaseCostRoundsOnceForTheWholeBasis).
//
// Two other shapes are tempting and both drift. Rounding each EVENT's result
// before adding reads like "convert each deal"; rounding each TERM reads like
// "convert each amount". Each is off by a minor unit here, and the error grows
// with the number of disposals.
//
//	fx USD->RUB = 90.5, one rate for every date, so this test says nothing about
//	  WHICH date's rate is used (that is pinned above) — only about rounding
//	two buys of $123.45  -> two lots of 12_345 minor USD
//	two sells of $246.90 -> two disposals, proceeds 24_690 each, basis 12_345 each
//
//	terms, as decimals:  24_690 * 90.5 =  2_234_445.0
//	                    -12_345 * 90.5 = -1_117_222.5   (twice each)
//
//	rounding once, at the end: 2 * 1_117_222.5 = 2_234_445.0 -> 2_234_445
//	rounding per event:        2 * 1_117_223                 =  2_234_446
//	rounding per term:         2 * (2_234_445 - 1_117_223)   =  2_234_444
//
// All three are distinct, and both wrong ones are asserted by name.
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
	if p.RealizedPnlMinor != 24690 {
		t.Fatalf("realized_pnl_minor = %d, want 24690 (two disposals of 12345, in USD)", p.RealizedPnlMinor)
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

// TestPositionInBaseRealizedNullWhenAReleasedParcelHasNoAcquisitionDate is the
// honesty rule on the realized side: a parcel that does not know when it was
// bought (it arrived by a transfer whose per-lot breakdown was never recorded —
// see portfolio.Lot.AcquiredOn) has no date to ask the fx table about, so no
// ruble expense can be struck for it and the realized figure is not publishable
// at all.
//
// What must NOT happen is the rest of the object going with it. cost_minor,
// income_minor and the valuation are answers to their own questions, computed
// from their own dates, and none of them depends on a parcel that has already
// been sold. This is the difference from an undated lot STILL HELD, which does
// null the whole object (TestPositionInBaseNullWhenALotHasNoAcquisitionDate):
// there the missing date sits inside cost_minor's own sum.
//
//	fx USD->RUB: 60 from 2026-02-01, 90 from 2026-07-01
//	source: buy 10 @ $100.00 on 2026-03-10, buy 10 @ $200.00 on 2026-07-10
//	transfer all 20 to the destination on 2026-07-20, then its breakdown is
//	  dropped -> destination lot 1: 20 units, cost 300_000, NO date
//	destination: buy 3 @ $40.00 on 2026-07-25 -> lot 2: 3 units, cost 12_000, dated
//	destination: sell 20 @ $200.00 on 2026-07-28 -> the queue hands over the
//	  undated lot first (undated lots sort ahead of every dated one), so the
//	  disposal releases exactly the 300_000 nobody has a purchase date for
//
//	realized_pnl_minor (USD)   = 400_000 - 300_000 = 100_000
//	in_base.realized_pnl_minor = null
//	in_base.cost_minor         = 12_000 * 90       = 1_080_000  (the dated lot,
//	                                                  still perfectly convertible)
//
// The two numbers a wrong implementation prints are asserted by name:
// 100_000 * 90 = 9_000_000 dates the retired parcel on the day of the SALE — a
// figure nothing downstream could tell from a real one — and 400_000 * 90 =
// 36_000_000 drops the parcel from the sum and publishes the proceeds as if they
// had cost nothing.
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

	// Turn the transfer into one recorded before breakdowns were kept: the basis
	// survives on the operation, the dates behind it do not.
	if _, err := pool.Exec(t.Context(), `DELETE FROM operation_transfer_lots`); err != nil {
		t.Fatalf("drop the stored breakdown: %v", err)
	}

	// A dated lot of the destination's own, so that what survives the sale below
	// is a real, convertible basis rather than an empty one — a zero would prove
	// nothing about the rest of the object staying up.
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
	if p.RealizedPnlMinor != 100000 {
		t.Fatalf("realized_pnl_minor = %d, want 100000 (400000 - 300000, in USD)", p.RealizedPnlMinor)
	}
	if p.HasUndatedLots {
		t.Fatalf("has_undated_lots = true, want false: the undated lot has been sold and is not among the lots still held — which is exactly why it cannot be the thing that explains the null below")
	}
	// has_undated_lots being false while realized_pnl_minor is null is exactly
	// the gap has_undated_realizations exists to close: the parcel that
	// stopped the sum has already been sold, so no flag about HELD lots can
	// name it, and a reader is left to guess "no rate" about a date that will
	// never arrive. This is the "raised" half of that flag's coverage.
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

// oneDateConverter answers every fx lookup with one flat rate except on a
// single date, where it answers with err instead. A real *marketdata.Converter
// cannot produce that: Store.FxRateOn resolves to the nearest date on or before
// the one asked for, so a date that has no rate is always OLDER than every date
// that does — and a sale is by construction later than the purchase whose basis
// it retires. A hole under exactly one disposal is therefore only reachable
// through a double, and it is the hole worth testing: it separates the two
// figures that get their rates from different days.
//
// The one date is matched as its YYYY-MM-DD string so it compares the calendar
// date rather than two time.Time values that merely mean the same day (the same
// reason the handler's rateKey holds a string).
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

// RatesOn answers the batch from this double's own Rate, so the hole under the
// one date is in the prewarm too and not only in the fallback (see
// ratesFromRate).
func (c oneDateConverter) RatesOn(ctx context.Context, queries []marketdata.RateQuery) (marketdata.Rates, error) {
	return ratesFromRate(ctx, c, queries)
}

// realizedRateHoleAPI wires the fixture the two tests below share: a USD
// position that bought 20 shares and sold 10 of them, against a converter that
// answers everything at 90 except on the day of the sale, where it answers with
// err.
//
//	buy  20 @ $100.00 on 2026-03-10 -> lot 20 units, cost 200_000 minor USD
//	sell 10 @ $120.00 on 2026-05-10 -> releases half the lot, 100_000 of basis
//	  remaining lot: 10 units, cost 100_000, dated 2026-03-10
//
// Only the realized figure needs the sale day's rate. The basis needs 2026-03-10
// and rate_on needs today, both of which resolve — so whatever the handler does
// with err shows up in exactly one field, and the rest of the object stands there
// as the control.
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

// TestPositionInBaseRealizedNullWhenTheDisposalDateHasNoRate is the second half
// of the honesty rule: a missing RATE on a date the realized sum needs nulls
// that figure, exactly as a missing purchase DATE does, and leaves the rest of
// the object standing. The two causes differ only in whether the date is unknown
// or the rate for it is; both make one term of the sum unstrikeable, and a sum
// missing a term is an invented number.
//
//	every rate resolves at 90 except on the day of the sale, which has none
//	realized_pnl_minor (USD)   = 120_000 - 100_000 = 20_000
//	in_base.realized_pnl_minor = null
//	in_base.cost_minor         = 100_000 * 90      = 9_000_000 (the lot that is
//	                                                  still held, unaffected)
func TestPositionInBaseRealizedNullWhenTheDisposalDateHasNoRate(t *testing.T) {
	url, c, accountID := realizedRateHoleAPI(t, fmt.Errorf("%w: USD -> RUB on %s", marketdata.ErrNoRate, sellOn))

	p := onlyPosition(t, c, url, accountID)

	if p.RealizedPnlMinor != 20000 {
		t.Fatalf("realized_pnl_minor = %d, want 20000 (120000 - 100000, in USD)", p.RealizedPnlMinor)
	}
	// Every parcel here — the one retired and the one still held — has a
	// recorded purchase date; only a fx rate is missing, and that is a gap
	// the backfill job closes on its own. has_undated_realizations must stay
	// false here, or a reader would be told a permanent, unrecoverable gap
	// where the true story is "the number will appear later" — the "lowered"
	// half of this flag's coverage, the mirror of the raised case above.
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

// TestPositionInBaseRealizedNullWhenThePurchaseDateHasNoRate is the mirror of
// the test above: the missing rate sits under the day a retired parcel was
// BOUGHT instead of the day it was SOLD. The honesty rule treats the two
// identically — a term the sum needs has no rate, so the sum is not
// strikeable — and it is the same code path either way (sumInBase walks every
// term the same way regardless of which date it carries). What differs is how
// the hole is reached: a hole under the SALE day needs oneDateConverter,
// because a real Converter's Store.FxRateOn falls back to the nearest EARLIER
// date and a sale is by construction later than the purchase whose basis it
// retires (see oneDateConverter's doc comment) — but a hole under the
// PURCHASE day needs no double at all. Buying before every rate the fixture
// seeds leaves Store.FxRateOn nothing earlier to fall back to, so the same
// real converter used everywhere else in this file already produces exactly
// this gap.
//
//	fx USD->RUB: one rate, 80 from 2026-05-01
//	buy  10 @ $100.00 on 2026-01-05 (before the only seeded rate) -> lot cost
//	  100_000 minor USD; no rate resolves for this date at all
//	sell 10 @ $200.00 on 2026-05-10 (after the seeded rate) -> proceeds
//	  200_000, releasing the whole lot above; the sale's OWN day resolves fine
//	buy   3 @ $40.00  on 2026-07-25 (after the sale, untouched by it) -> lot
//	  12_000 minor USD, held, dated and rated same as the sibling fixture in
//	  TestPositionInBaseRealizedNullWhenAReleasedParcelHasNoAcquisitionDate
//
//	realized_pnl_minor (USD)   = 200_000 - 100_000 = 100_000
//	in_base.realized_pnl_minor = null (the retired parcel's OWN day has no rate)
//	in_base.cost_minor         = 12_000 * 80       = 960_000 (the held lot,
//	                                                  unaffected — its own day
//	                                                  resolves fine)
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

	if p.RealizedPnlMinor != 100000 {
		t.Fatalf("realized_pnl_minor = %d, want 100000 (200000 - 100000, in USD)", p.RealizedPnlMinor)
	}
	// The retired parcel's purchase date IS on record (2026-01-05); only its
	// fx rate is missing. has_undated_realizations reports an unrecorded
	// DATE, not a missing rate, and must stay false here — a missing rate is
	// a gap the backfill job closes on its own.
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

// TestPositionInBaseRealizedRateErrorFailsRequest draws, for the realized
// figure, the distinction the rest of this handler already draws: a genuine
// failure — a dropped connection, a canceled context — must fail the request,
// never be served as a 200 with a null in it. The two look identical at the call
// site and mean opposite things: "no rate for this day" is a fact about the
// world that the fx backfill may never change, while an outage is a fact about
// this server that will be gone in a minute, and rendering the second as the
// first tells the owner their sale is unconvertible when it is not.
//
// The fixture is the one above with a different error, so the ONLY difference
// between a null field and a failed request is which error the lookup returned —
// which is precisely the distinction under test. Without it, wrapping the
// realized sum in a "treat any error as no rate" shortcut would pass every other
// test in this file.
func TestPositionInBaseRealizedRateErrorFailsRequest(t *testing.T) {
	url, c, accountID := realizedRateHoleAPI(t, errors.New("connection reset by peer"))

	resp := do(t, c, "GET", url+"/api/v1/accounts/"+accountID+"/positions", "")
	if resp.StatusCode != http.StatusInternalServerError {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET positions with a failing rate lookup on the day of a sale = %d, want 500 — a real outage must not be served as a 200 with in_base.realized_pnl_minor: null: %s",
			resp.StatusCode, b)
	}
}
