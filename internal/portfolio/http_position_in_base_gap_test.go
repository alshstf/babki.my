package portfolio_test

import (
	"context"
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

// This file is issue #66's server half. Four different terms can stop a
// position's base-currency object, and until now the payload said only that
// one of them had: in_base went null and the screen put a single sentence,
// «Нет курса — показано в исходной валюте», over every figure in the row. That
// sentence is false about a lot nobody recorded a purchase date for (no rate
// is missing; no rate was ever asked for, and none will be), and false again
// over the market valuation of a row whose today-rate is present and whose
// basis failed on a purchase date. in_base_gap names the term instead, and
// market_value_gap answers the one cell that can be unconverted for a reason of
// its own while the rest of the row converts normally.
//
// Every test here asserts the SPECIFIC value, never merely that some gap is
// set: a flag that is present but names the wrong cause is worse than the
// imprecise-but-true sentence it replaces, so "a gap appeared" is not the
// property under test anywhere in this file.

// gapText renders a nullable gap for a failure message. formatText would do,
// but these read better unquoted next to the prose, and "null" has to be
// distinguishable from a value at a glance since null is a legitimate answer
// for both fields.
func gapText(p *string) string {
	if p == nil {
		return "null (no gap published)"
	}
	return *p
}

// TestPositionInBaseGapNamesTheUndatedLot covers the branch that no rate can
// ever close: a transfer recorded before per-lot breakdowns were kept arrives
// as one lot with a basis and no purchase date, so there is no date to ask the
// fx table about. The fixture is TestPositionInBaseNullWhenALotHasNoAcquisition
// Date's — a real transfer whose stored pieces are then deleted, which is
// exactly the state such a legacy row is in.
//
// The rate table here is complete: both USD->RUB rows are seeded and every
// date in the fixture resolves. So a payload saying «no rate» over this row
// would not merely be vague, it would be false — and would promise a figure
// that no backfill is ever going to produce.
func TestPositionInBaseGapNamesTheUndatedLot(t *testing.T) {
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
	createTransfer(t, c, url, fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,
		"quantity":"10","occurred_on":%q}`, from.ID, to.ID, share.ID, transferOn))
	if _, err := pool.Exec(t.Context(), `DELETE FROM operation_transfer_lots`); err != nil {
		t.Fatalf("drop the stored breakdown: %v", err)
	}

	p := onlyPosition(t, c, url, to.ID)
	if p.InBase != nil {
		t.Fatalf("in_base = %+v, want null: the arriving lot has no acquisition date", *p.InBase)
	}
	if p.InBaseGap == nil || *p.InBaseGap != "undated_lot" {
		t.Fatalf("in_base_gap = %s, want undated_lot: every rate this fixture needs is seeded, and the missing thing is a DATE — a «no rate» answer here promises a figure that is never coming",
			gapText(p.InBaseGap))
	}
}

// TestPositionInBaseGapNamesTheLotDateWithNoRate is the neighbouring branch:
// the lot knows perfectly well when it was bought, and it is the fx table that
// has nothing for that day. Same null on screen, opposite news — the backfill
// closes this one on its own.
//
// The rate table starts at lateRateOn, so the earlyBuyOn lot has no rate on or
// before its date while the lateBuyOn lot and today both resolve.
func TestPositionInBaseGapNamesTheLotDateWithNoRate(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes, datedRate{lateRateOn, "90"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID, earlyBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"200",
		"amount_minor":-200000,"currency":"USD"}`, acc.ID, share.ID, lateBuyOn))

	p := onlyPosition(t, c, url, acc.ID)
	if p.InBase != nil {
		t.Fatalf("in_base = %+v, want null: the lot bought on %s has no rate on or before its date", *p.InBase, earlyBuyOn)
	}
	if p.InBaseGap == nil || *p.InBaseGap != "no_rate_lot_date" {
		t.Fatalf("in_base_gap = %s, want no_rate_lot_date: both lots have acquisition dates and one of those dates has no rate",
			gapText(p.InBaseGap))
	}
	if p.HasUndatedLots {
		t.Errorf("has_undated_lots = true, want false: both lots were bought on recorded days — this fixture is not testing what it means to")
	}
}

// TestPositionInBaseGapNamesTheIncomeDateWithNoRate is the third branch: every
// lot converts, and it is a dividend older than the rate table that stops the
// object. Same «no rate» family as the lot case, different date to look for —
// and a client that says «нет курса на дату покупки» over it names a purchase
// that had nothing to do with it.
func TestPositionInBaseGapNamesTheIncomeDateWithNoRate(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes, datedRate{lateRateOn, "90"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID, lateBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend",
		"occurred_on":%q,"amount_minor":10000,"currency":"USD"}`, acc.ID, share.ID, earlyBuyOn))

	p := onlyPosition(t, c, url, acc.ID)
	if p.InBase != nil {
		t.Fatalf("in_base = %+v, want null: the dividend paid on %s has no rate on or before its date", *p.InBase, earlyBuyOn)
	}
	if p.InBaseGap == nil || *p.InBaseGap != "no_rate_income_date" {
		t.Fatalf("in_base_gap = %s, want no_rate_income_date: the only lot is dated %s and converts, so the term that stopped the object is the dividend's",
			gapText(p.InBaseGap), lateBuyOn)
	}
}

// TestPositionInBaseGapNamesTodaysRate is the last branch, and the one the old
// single sentence was most obviously wrong about in the other direction: here
// the historical rates are all present, the basis and the income are perfectly
// convertible, and what is missing is TODAY's rate — the one the market
// valuation needs. The object goes because the profit below it is measured
// against a basis that would otherwise be published beside a valuation that is
// not.
//
// oneDateConverter is what makes the hole reachable: a real rate table cannot
// hold a rate for March and none for today, since Store.FxRateOn resolves the
// nearest EARLIER row (see TestPositionInBasePublishedWithoutTodaysRateWhenThere
// IsNoQuote, which uses the same double for the mirror-image case).
func TestPositionInBaseGapNamesTodaysRate(t *testing.T) {
	pool := testdb.New(t)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, oneDateConverter{
		rate:   decimal.RequireFromString("90"),
		rateOn: mustDate(t, lateRateOn),
		on:     time.Now().UTC().Format("2006-01-02"),
		err:    fmt.Errorf("%w: USD -> RUB today", marketdata.ErrNoRate),
	})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	shareID, err := uuid.Parse(share.ID)
	if err != nil {
		t.Fatalf("parse share id: %v", err)
	}
	// A quote, unlike the mirror test: today's rate is required by exactly the
	// figures that use it, so this branch is unreachable without a valuation.
	quotes.byInstrument[shareID] = marketdata.Quote{
		InstrumentID: shareID, On: mustDate(t, "2026-07-20"),
		Price: decimal.RequireFromString("120.00"), Currency: "USD", Source: "test",
	}
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID, earlyBuyOn))

	p := onlyPosition(t, c, url, acc.ID)
	if p.InBase != nil {
		t.Fatalf("in_base = %+v, want null: the valuation cannot be struck without today's rate", *p.InBase)
	}
	if p.InBaseGap == nil || *p.InBaseGap != "no_rate_today" {
		t.Fatalf("in_base_gap = %s, want no_rate_today: the lot's own date resolves at 90 and it is the rate for TODAY that is missing",
			gapText(p.InBaseGap))
	}
	// The valuation itself is in the position's own currency here — nothing
	// tried and failed to bring it there, so the valuation's own field has
	// nothing to report and the row's answer is the true one for every cell.
	if p.MarketValueGap != nil {
		t.Errorf("market_value_gap = %s, want null: the quote is already in USD, so no conversion into the position's currency was ever attempted",
			gapText(p.MarketValueGap))
	}
}

// TestPositionInBaseGapFollowsWhichRateIsActuallyMissing is the test that makes
// the other four worth having. One fixture, one hole in the rate table, and the
// hole is MOVED: from the day the lot was bought to the day the dividend was
// paid. Nothing else changes — same account, same amounts, same number of
// missing rates — and the published cause must move with it.
//
// A flag that merely correlates with "in_base is null" passes every other test
// in this file and fails this one, which is the point: the value has to come
// from the term that actually stopped the sum, not from a condition computed
// beside it that happens to be true at the same time.
//
// oneDateConverter answers 90 everywhere except on its one date, so each run
// has exactly one unresolvable term and they are different terms.
func TestPositionInBaseGapFollowsWhichRateIsActuallyMissing(t *testing.T) {
	// No quote in either run: today's rate is then never asked for, so the two
	// runs differ in the historical term alone.
	run := func(t *testing.T, holeOn string) *string {
		t.Helper()
		pool := testdb.New(t)
		quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
		url, c := setupAPI(t, pool, quotes, oneDateConverter{
			rate:   decimal.RequireFromString("90"),
			rateOn: mustDate(t, lateRateOn),
			on:     holeOn,
			err:    fmt.Errorf("%w: USD -> RUB on %s", marketdata.ErrNoRate, holeOn),
		})
		acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
		share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
		createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
			"occurred_on":%q,"quantity":"10","price":"100",
			"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID, earlyBuyOn))
		createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend",
			"occurred_on":%q,"amount_minor":10000,"currency":"USD"}`, acc.ID, share.ID, lateBuyOn))
		p := onlyPosition(t, c, url, acc.ID)
		if p.InBase != nil {
			t.Fatalf("in_base = %+v, want null: the rate for %s is missing", *p.InBase, holeOn)
		}
		return p.InBaseGap
	}

	t.Run("the hole is on the purchase day", func(t *testing.T) {
		got := run(t, earlyBuyOn)
		if got == nil || *got != "no_rate_lot_date" {
			t.Fatalf("in_base_gap = %s, want no_rate_lot_date: the missing rate is the one for the lot bought on %s", gapText(got), earlyBuyOn)
		}
	})
	t.Run("the hole is on the dividend day", func(t *testing.T) {
		got := run(t, lateBuyOn)
		if got == nil || *got != "no_rate_income_date" {
			t.Fatalf("in_base_gap = %s, want no_rate_income_date: the very same position, with the hole moved to the dividend's day — a flag that still says no_rate_lot_date is naming a term that converted",
				gapText(got))
		}
	})
}

// TestPositionInBaseGapNamesThePermanentCauseWhenTwoAreTrue answers the
// question a single-valued field has to answer: what happens when more than one
// term is unvaluable at once. Both positions below have an income payment older
// than the rate table; one of them ALSO holds a lot with no acquisition date.
// The second is the control — it proves the income gap is really there — and
// the first must still say undated_lot.
//
// The order is the argument, not a coincidence: three of the four causes are
// gaps the fx backfill closes on its own, and one of them never closes at all.
// Naming a closeable cause on a row that also has the permanent one would
// promise a figure that is not coming. RealizedTotal.in_base_gap needs a `both`
// value because it sums many positions and both kinds really are true of that
// one total; a single position stops at its first unvaluable term, and the
// permanent term is the first one it looks at.
func TestPositionInBaseGapNamesThePermanentCauseWhenTwoAreTrue(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))
	// The table starts at lateRateOn: every buy and the transfer resolve, the
	// dividends on earlyBuyOn do not.
	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: mustDate(t, lateRateOn), Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rate: %v", err)
	}

	from := createAccount(t, c, url, `{"name":"Старый брокер","type":"brokerage","currency":"USD"}`)
	to := createAccount(t, c, url, `{"name":"Новый брокер","type":"brokerage","currency":"USD"}`)
	plain := createAccount(t, c, url, `{"name":"Обычный брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, from.ID, share.ID, lateBuyOn))
	createTransfer(t, c, url, fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,
		"quantity":"10","occurred_on":%q}`, from.ID, to.ID, share.ID, transferOn))
	if _, err := pool.Exec(t.Context(), `DELETE FROM operation_transfer_lots`); err != nil {
		t.Fatalf("drop the stored breakdown: %v", err)
	}
	// The same unresolvable dividend on both accounts.
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend",
		"occurred_on":%q,"amount_minor":10000,"currency":"USD"}`, to.ID, share.ID, earlyBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, plain.ID, share.ID, lateBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend",
		"occurred_on":%q,"amount_minor":10000,"currency":"USD"}`, plain.ID, share.ID, earlyBuyOn))

	control := onlyPosition(t, c, url, plain.ID)
	if control.InBaseGap == nil || *control.InBaseGap != "no_rate_income_date" {
		t.Fatalf("control in_base_gap = %s, want no_rate_income_date — without it the test below proves nothing: the second cause has to be genuinely present",
			gapText(control.InBaseGap))
	}

	both := onlyPosition(t, c, url, to.ID)
	if !both.HasUndatedLots {
		t.Fatalf("has_undated_lots = false on the transferred position: the fixture did not produce a dateless lot, so both causes are not present")
	}
	if both.InBaseGap == nil || *both.InBaseGap != "undated_lot" {
		t.Fatalf("in_base_gap = %s, want undated_lot: this row has BOTH gaps, and naming the dividend's — which the backfill will close — would promise a converted figure that the dateless lot makes impossible forever",
			gapText(both.InBaseGap))
	}
}

// TestPositionInBaseGapNamesTheLotDateOverTheIncomeDateWhenBothAreMissing is
// what the contract's ordering promise actually rests on. The lot sum and the
// income sum are two adjacent, independent calls (see positionInBase): nothing
// makes the lot's check run before the income's except the order they are
// written in. This fixture puts a hole under BOTH the lot's date and the
// dividend's — the same day, so one missing rate takes down both terms at
// once — and the contract (InBaseGap's description, api/openapi.yaml) says the
// server "stops at the first term it cannot value, in the order listed above,
// so each value also says that everything before it succeeded". For a row
// missing both, that promise is `no_rate_lot_date`: if the two calls were ever
// reordered, this is the one test that would catch it, because every other
// gap test in this file leaves only ONE term unresolvable and cannot tell
// "checked first" from "the only one that failed".
func TestPositionInBaseGapNamesTheLotDateOverTheIncomeDateWhenBothAreMissing(t *testing.T) {
	pool := testdb.New(t)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, oneDateConverter{
		rate:   decimal.RequireFromString("90"),
		rateOn: mustDate(t, lateRateOn),
		on:     earlyBuyOn,
		err:    fmt.Errorf("%w: USD -> RUB on %s", marketdata.ErrNoRate, earlyBuyOn),
	})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)

	// Buy and dividend dated the SAME day, so the one hole in the rate table
	// stops both the lot sum and the income sum at once — neither term is
	// merely "the only one that failed".
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID, earlyBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend",
		"occurred_on":%q,"amount_minor":10000,"currency":"USD"}`, acc.ID, share.ID, earlyBuyOn))

	p := onlyPosition(t, c, url, acc.ID)
	if p.InBase != nil {
		t.Fatalf("in_base = %+v, want null: both the lot and the dividend are dated %s, which has no fx rate", *p.InBase, earlyBuyOn)
	}
	if p.InBaseGap == nil || *p.InBaseGap != "no_rate_lot_date" {
		t.Fatalf("in_base_gap = %s, want no_rate_lot_date: the contract promises the server stops at the FIRST term it cannot value, in the declared order, and the lot sum is checked before the income sum — a row missing a rate for both must report the lot's term, never the income's, or the contract's ordering promise is false",
			gapText(p.InBaseGap))
	}
}

// twoDateConverter is oneDateConverter's twin for a fixture that needs a hole
// under TWO specific dates at once — here, an income operation's date and
// today's — rather than the one oneDateConverter can put a hole under. Same
// flat-rate/err shape, checked against a small set instead of a single
// string.
type twoDateConverter struct {
	rate   decimal.Decimal
	rateOn time.Time
	// holes maps each YYYY-MM-DD string that has no rate to the error Rate
	// answers with for it.
	holes map[string]error
}

func (c twoDateConverter) Rate(_ context.Context, _, _ string, on time.Time) (decimal.Decimal, time.Time, error) {
	if err, missing := c.holes[on.Format("2006-01-02")]; missing {
		return decimal.Decimal{}, time.Time{}, err
	}
	return c.rate, c.rateOn, nil
}

// RatesOn answers the batch from this double's own Rate, for the same reason
// oneDateConverter's does.
func (c twoDateConverter) RatesOn(ctx context.Context, queries []marketdata.RateQuery) (marketdata.Rates, error) {
	return ratesFromRate(ctx, c, queries)
}

// TestPositionInBaseGapNamesTheIncomeDateOverTodaysRateWhenBothAreMissing is
// TestPositionInBaseGapNamesTheLotDateOverTheIncomeDateWhenBothAreMissing's
// twin for the OTHER adjacent pair the contract's ordering promise covers:
// the income sum (positionInBase's second check) against today's rate for the
// market valuation (its third and last). Before this test, no fixture put a
// hole under both the income date and today at once, so a build that checked
// them in the wrong order — today before income — would still report
// no_rate_income_date whenever only the income date lacked a rate (today's
// still present, the common case every other test in this file exercises)
// and still report no_rate_today whenever only today lacked one
// (TestPositionInBaseGapNamesTodaysRate) — the swap would be invisible until
// a row hit both holes simultaneously, which is exactly what this fixture
// does. The lot itself is dated and rated: only the dividend's day and today
// have no rate, so the lot sum succeeds and the object stops at the income
// sum, never reaching the today check at all.
func TestPositionInBaseGapNamesTheIncomeDateOverTodaysRateWhenBothAreMissing(t *testing.T) {
	pool := testdb.New(t)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	today := time.Now().UTC().Format("2006-01-02")
	url, c := setupAPI(t, pool, quotes, twoDateConverter{
		rate:   decimal.RequireFromString("90"),
		rateOn: mustDate(t, lateRateOn),
		holes: map[string]error{
			earlyBuyOn: fmt.Errorf("%w: USD -> RUB on %s", marketdata.ErrNoRate, earlyBuyOn),
			today:      fmt.Errorf("%w: USD -> RUB today", marketdata.ErrNoRate),
		},
	})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	shareID, err := uuid.Parse(share.ID)
	if err != nil {
		t.Fatalf("parse share id: %v", err)
	}
	// A quote is required to reach the today check at all (see
	// TestPositionInBaseGapNamesTodaysRate) — without one, positionInBase
	// returns before it ever asks for today's rate, and the fixture would
	// prove nothing about which of the two remaining checks runs first.
	quotes.byInstrument[shareID] = marketdata.Quote{
		InstrumentID: shareID, On: mustDate(t, "2026-07-20"),
		Price: decimal.RequireFromString("120.00"), Currency: "USD", Source: "test",
	}
	// The lot is dated lateBuyOn, which resolves at the flat rate — only the
	// dividend and today are holes.
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID, lateBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend",
		"occurred_on":%q,"amount_minor":10000,"currency":"USD"}`, acc.ID, share.ID, earlyBuyOn))

	p := onlyPosition(t, c, url, acc.ID)
	if p.InBase != nil {
		t.Fatalf("in_base = %+v, want null: both the dividend's date (%s) and today have no fx rate", *p.InBase, earlyBuyOn)
	}
	if p.InBaseGap == nil || *p.InBaseGap != "no_rate_income_date" {
		t.Fatalf("in_base_gap = %s, want no_rate_income_date: the contract promises the server stops at the FIRST term it cannot value, in the declared order, and the income sum is checked before today's rate for the valuation — a row missing both must report the income term, never today's, or the contract's ordering promise is false",
			gapText(p.InBaseGap))
	}
}

// TestPositionInBaseGapNullWhenNothingStoppedTheObject pins the two ways the
// field is legitimately absent, which a client tells apart by `currency` alone:
// the object was struck (USD row, every rate present), and there was never
// anything to convert (RUB row in an RUB space, where the position's own
// figures ARE the base-currency ones and no «not converted» caption belongs
// over them at all).
//
// Without this, a flag that fired on every row would still pass every test
// above.
func TestPositionInBaseGapNullWhenNothingStoppedTheObject(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes, datedRate{earlyRateOn, "90"})

	usd := createAccount(t, c, url, `{"name":"Валютный","type":"brokerage","currency":"USD"}`)
	rub := createAccount(t, c, url, `{"name":"Рублёвый","type":"brokerage","currency":"RUB"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	ruShare := createInstrument(t, c, url, `{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, usd.ID, share.ID, lateBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"RUB"}`, rub.ID, ruShare.ID, lateBuyOn))

	converted := onlyPosition(t, c, url, usd.ID)
	if converted.InBase == nil {
		t.Fatalf("in_base = nil on the USD row, want the object: every rate it needs is seeded")
	}
	if converted.InBaseGap != nil {
		t.Fatalf("in_base_gap = %s, want null: the object was struck, so nothing stopped it", gapText(converted.InBaseGap))
	}

	native := onlyPosition(t, c, url, rub.ID)
	if native.InBase != nil {
		t.Fatalf("in_base = %+v on the RUB row, want null: it is already in the base currency", *native.InBase)
	}
	if native.InBaseGap != nil {
		t.Fatalf("in_base_gap = %s, want null: nothing was missing — there was nothing to convert, and a caption over these figures would be explaining an absence that is not one",
			gapText(native.InBaseGap))
	}
}

// TestPositionMarketValueGapNamesTheValuationCurrency is the case the brief
// asks about explicitly: in_base is present — the basis and the income convert
// normally — and one figure inside it is null. A bond priced off a EUR face
// value in a USD position of an RUB space, with no EUR rate anywhere, is that
// case: the valuation never reached the position's own currency, so it is not
// carried into the base one either.
//
// It is a second field rather than a fifth value of in_base_gap because it
// answers for a different cell of the same row, and the row-level field is null
// here: cost, income and their captions are about a conversion that worked.
func TestPositionMarketValueGapNamesTheValuationCurrency(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))
	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: fxSeedOn(t), Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rate: %v", err)
	}

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	bond := createInstrument(t, c, url,
		`{"type":"bond","name":"Еврооблигация","ticker":"EUB","currency":"USD","face_value_minor":100000,"face_currency":"EUR"}`)
	bondID, err := uuid.Parse(bond.ID)
	if err != nil {
		t.Fatalf("parse bond id: %v", err)
	}
	quotes.byInstrument[bondID] = marketdata.Quote{
		InstrumentID: bondID, On: mustDate(t, "2026-07-20"),
		Price: decimal.RequireFromString("100.00"), Currency: "USD", Source: "test",
	}
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"1","price":"1000",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, bond.ID))

	p := onlyPosition(t, c, url, acc.ID)
	if p.MarketValueCurrency == nil || *p.MarketValueCurrency != "EUR" {
		t.Fatalf("market_value_currency = %s, want EUR — the fixture is not producing a valuation stuck in a third currency", formatText(p.MarketValueCurrency))
	}
	if p.MarketValueGap == nil || *p.MarketValueGap != "no_rate_valuation_currency" {
		t.Fatalf("market_value_gap = %s, want no_rate_valuation_currency: the valuation could not be converted out of EUR",
			gapText(p.MarketValueGap))
	}
	if p.InBase == nil {
		t.Fatalf("in_base = nil, want the object: the basis and the income are in USD and convert normally")
	}
	if p.InBase.MarketValueMinor != nil {
		t.Errorf("in_base.market_value_minor = %s, want null — the gap says it was not struck, and a figure beside that flag would be one of the two lying",
			formatMinor(p.InBase.MarketValueMinor))
	}
	if p.InBaseGap != nil {
		t.Errorf("in_base_gap = %s, want null: the object stands, and the row's other cells are converted — a row-level cause here would be captioning cost and income with a failure that is not theirs",
			gapText(p.InBaseGap))
	}
}

// TestPositionMarketValueGapNullOnlyWhenTheValuationIsThere is the negative
// half, and half of it stopped being negative with #78. The field is null on a
// row whose valuation converted — it was already in the position's currency, so
// nothing about that cell went wrong and there is nothing to explain.
//
// THE ROW WITH NO VALUATION IS NOT THAT CASE, and this test used to assert that
// it was, on the ground that "nothing was kept back, and there is no cell for a
// caption to sit on". Both halves of that ground were false: the client renders
// a dash in that cell and captions it, and for want of a published cause the
// caption it chose was «Нет котировки» over every empty valuation — including
// the two kinds of row where a quote is present and is not what is missing. So
// the unquoted row here now carries `no_quote`, which on THIS fixture — a share,
// with a complete catalog row — is exactly what is true of it.
func TestPositionMarketValueGapNullOnlyWhenTheValuationIsThere(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := fxRateAPI(t, quotes, datedRate{earlyRateOn, "90"})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	quoted := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	unquoted := createInstrument(t, c, url, `{"type":"share","name":"Без Котировки","ticker":"NOQ","currency":"USD"}`)
	quotedID, err := uuid.Parse(quoted.ID)
	if err != nil {
		t.Fatalf("parse instrument id: %v", err)
	}
	quotes.byInstrument[quotedID] = marketdata.Quote{
		InstrumentID: quotedID, On: mustDate(t, "2026-07-20"),
		Price: decimal.RequireFromString("120.00"), Currency: "USD", Source: "test",
	}
	for _, inst := range []string{quoted.ID, unquoted.ID} {
		createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
			"occurred_on":%q,"quantity":"10","price":"100",
			"amount_minor":-100000,"currency":"USD"}`, acc.ID, inst, lateBuyOn))
	}

	resp := do(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/positions", "")
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET positions = %d: %s", resp.StatusCode, b)
	}
	var got positionsResp
	decodeJSON(t, resp, &got)
	byID := make(map[string]positionResp, len(got.Positions))
	for _, p := range got.Positions {
		byID[p.Instrument.Id] = p
	}

	withQuote, ok := byID[quoted.ID]
	if !ok {
		t.Fatalf("no position for the quoted instrument: %+v", got.Positions)
	}
	if withQuote.MarketValueGap != nil {
		t.Errorf("market_value_gap = %s on a row whose valuation is in its own currency, want null: nothing was withheld",
			gapText(withQuote.MarketValueGap))
	}
	if withQuote.InBase == nil || withQuote.InBase.MarketValueMinor == nil {
		t.Errorf("in_base.market_value_minor = %v, want a figure: this row's valuation converts", withQuote.InBase)
	}

	withoutQuote, ok := byID[unquoted.ID]
	if !ok {
		t.Fatalf("no position for the unquoted instrument: %+v", got.Positions)
	}
	if withoutQuote.MarketValueMinor != nil {
		t.Fatalf("market_value_minor = %s, want null — the fixture seeds no quote for this instrument", formatMinor(withoutQuote.MarketValueMinor))
	}
	if withoutQuote.MarketValueGap == nil || *withoutQuote.MarketValueGap != "no_quote" {
		t.Errorf("market_value_gap = %s, want no_quote: the dash this row renders needs the reason it is there, and for a priced type with a complete catalog row the reason is the price",
			gapText(withoutQuote.MarketValueGap))
	}
}

// TestPositionGapsAnswerForDifferentCellsAtOnce is why there are two fields
// instead of one. This row has both: a lot with no purchase date (which stops
// the whole object) and a valuation stuck in EUR (which is the valuation's own
// impediment and has nothing to do with the missing date). A single field would
// have to pick one, and either pick would put a false sentence over half the
// row — «нет даты покупки» over a valuation whose problem is a currency, or
// «оценка в чужой валюте» over a basis and an income that are in USD.
//
// The fixture stacks the two: the bond is bought in one account, transferred to
// another, and the stored breakdown is dropped, so the destination holds one
// dateless lot of a EUR-faced bond with no EUR rate anywhere.
func TestPositionGapsAnswerForDifferentCellsAtOnce(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))
	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: fxSeedOn(t), Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rate: %v", err)
	}

	from := createAccount(t, c, url, `{"name":"Старый брокер","type":"brokerage","currency":"USD"}`)
	to := createAccount(t, c, url, `{"name":"Новый брокер","type":"brokerage","currency":"USD"}`)
	bond := createInstrument(t, c, url,
		`{"type":"bond","name":"Еврооблигация","ticker":"EUB","currency":"USD","face_value_minor":100000,"face_currency":"EUR"}`)
	bondID, err := uuid.Parse(bond.ID)
	if err != nil {
		t.Fatalf("parse bond id: %v", err)
	}
	quotes.byInstrument[bondID] = marketdata.Quote{
		InstrumentID: bondID, On: mustDate(t, "2026-07-20"),
		Price: decimal.RequireFromString("100.00"), Currency: "USD", Source: "test",
	}

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"1","price":"1000",
		"amount_minor":-100000,"currency":"USD"}`, from.ID, bond.ID, lateBuyOn))
	createTransfer(t, c, url, fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,
		"quantity":"1","occurred_on":%q}`, from.ID, to.ID, bond.ID, transferOn))
	if _, err := pool.Exec(t.Context(), `DELETE FROM operation_transfer_lots`); err != nil {
		t.Fatalf("drop the stored breakdown: %v", err)
	}

	p := onlyPosition(t, c, url, to.ID)
	if p.InBase != nil {
		t.Fatalf("in_base = %+v, want null: the arriving lot has no acquisition date", *p.InBase)
	}
	if p.InBaseGap == nil || *p.InBaseGap != "undated_lot" {
		t.Fatalf("in_base_gap = %s, want undated_lot", gapText(p.InBaseGap))
	}
	if p.MarketValueGap == nil || *p.MarketValueGap != "no_rate_valuation_currency" {
		t.Fatalf("market_value_gap = %s, want no_rate_valuation_currency: the valuation is stuck in EUR for its own reason, which the row's missing purchase date neither caused nor describes — and it is published whether or not in_base survived",
			gapText(p.MarketValueGap))
	}
}

// TestPositionMarketValueGapPublishedOnABaseCurrencyPosition pins a contract
// claim (api/openapi.yaml, Position.market_value_gap) that nothing exercised
// before this task: the field answers for the valuation cell reaching the
// POSITION's own currency, a question entirely independent of whether that
// currency also happens to be the space's BASE one. A position already in the
// base currency skips in_base and in_base_gap entirely (positionInBase returns
// early on p.Currency == baseCurrency, before it ever looks at the valuation),
// but toAPI's own conversion — the one market_value_gap reports — runs
// regardless, and this fixture's bond still cannot reach RUB from EUR no
// matter which currency is "base".
func TestPositionMarketValueGapPublishedOnABaseCurrencyPosition(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))
	// No EUR rate anywhere: the bond's face-currency valuation can never reach
	// RUB, whether or not RUB is also the space's base currency.

	acc := createAccount(t, c, url, `{"name":"Рублёвый","type":"brokerage","currency":"RUB"}`)
	bond := createInstrument(t, c, url,
		`{"type":"bond","name":"Еврооблигация","ticker":"EUB","currency":"RUB","face_value_minor":100000,"face_currency":"EUR"}`)
	bondID, err := uuid.Parse(bond.ID)
	if err != nil {
		t.Fatalf("parse bond id: %v", err)
	}
	quotes.byInstrument[bondID] = marketdata.Quote{
		InstrumentID: bondID, On: mustDate(t, "2026-07-20"),
		Price: decimal.RequireFromString("100.00"), Currency: "RUB", Source: "test",
	}
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"1","price":"1000",
		"amount_minor":-100000,"currency":"RUB"}`, acc.ID, bond.ID))

	p := onlyPosition(t, c, url, acc.ID)
	if p.Currency != "RUB" {
		t.Fatalf("currency = %s, want RUB: this fixture is not testing a base-currency position", p.Currency)
	}
	if p.InBase != nil {
		t.Fatalf("in_base = %+v, want null: currency already equals the base currency, so there was nothing to convert", *p.InBase)
	}
	if p.InBaseGap != nil {
		t.Fatalf("in_base_gap = %s, want null: nothing was missing — there was nothing to convert in the first place", gapText(p.InBaseGap))
	}
	if p.MarketValueGap == nil || *p.MarketValueGap != "no_rate_valuation_currency" {
		t.Fatalf("market_value_gap = %s, want no_rate_valuation_currency: the valuation is stuck in EUR for a reason of its own, and that reason does not go away just because RUB happens to be both this position's currency and the space's base one",
			gapText(p.MarketValueGap))
	}
}

// TestPositionHasUndatedLotsOnABaseCurrencyPosition pins the other contract
// claim finding-6 asks about: has_undated_lots is a standing fact about the
// position's own lots (see anyUndatedLot), published whether or not there was
// ever anything to convert. A base-currency position with a dateless lot must
// still report it, alongside an in_base_gap that stays null — currency already
// equals the base currency, so there is no conversion for the missing date to
// have stopped (positionInBase never reaches lotTerms/anyUndatedLot on this
// path at all).
func TestPositionHasUndatedLotsOnABaseCurrencyPosition(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))
	// No fx rates seeded at all: both accounts are RUB, the space's base
	// currency, so nothing here ever needs one.

	from := createAccount(t, c, url, `{"name":"Старый брокер","type":"brokerage","currency":"RUB"}`)
	to := createAccount(t, c, url, `{"name":"Новый брокер","type":"brokerage","currency":"RUB"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"RUB"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"RUB"}`, from.ID, share.ID, earlyBuyOn))
	createTransfer(t, c, url, fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,
		"quantity":"10","occurred_on":%q}`, from.ID, to.ID, share.ID, transferOn))
	if _, err := pool.Exec(t.Context(), `DELETE FROM operation_transfer_lots`); err != nil {
		t.Fatalf("drop the stored breakdown: %v", err)
	}

	p := onlyPosition(t, c, url, to.ID)
	if p.Currency != "RUB" {
		t.Fatalf("currency = %s, want RUB: this fixture is not testing a base-currency position", p.Currency)
	}
	if !p.HasUndatedLots {
		t.Fatalf("has_undated_lots = false, want true: its only lot arrived by a transfer with no stored breakdown, so nothing knows when those shares were bought")
	}
	if p.InBaseGap != nil {
		t.Fatalf("in_base_gap = %s, want null: currency already equals the base currency, so there is no conversion for the missing date to have stopped", gapText(p.InBaseGap))
	}
	if p.InBase != nil {
		t.Fatalf("in_base = %+v, want null: nothing to convert", *p.InBase)
	}
}
