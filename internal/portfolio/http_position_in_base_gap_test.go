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
	"babki.my/babki/internal/marketdata/ratetest"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/testdb"
)

// in_base_gap names which term stopped a position's base object, and
// market_value_gap answers for the valuation cell (#66). Every test asserts the
// specific value: a gap naming the wrong cause is worse than none.

// gapText renders a nullable gap for a failure message.
func gapText(p *string) string {
	if p == nil {
		return "null (no gap published)"
	}
	return *p
}

// A pre-breakdown transfer's undated lot is named undated_lot, though every
// rate in the table resolves: «no rate» would be false.
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

// A dated lot whose day has no rate is no_rate_lot_date (the table starts at
// lateRateOn).
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

// A dividend older than the rate table is no_rate_income_date.
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

// Today's rate missing for a quoted position is no_rate_today; the historical
// rates all resolve (oneDateConverter makes the hole reachable).
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
	// A quote, so today's rate is asked for.
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
	// The valuation is already in the position's currency, so market_value_gap is
	// null.
	if p.MarketValueGap != nil {
		t.Errorf("market_value_gap = %s, want null: the quote is already in USD, so no conversion into the position's currency was ever attempted",
			gapText(p.MarketValueGap))
	}
}

// Moving the one hole from the lot's day to the dividend's moves the gap
// with it: the value comes from the term that actually failed.
func TestPositionInBaseGapFollowsWhichRateIsActuallyMissing(t *testing.T) {
	// No quote, so today's rate is never asked for.
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

// With an undated lot and an income date without a rate, the permanent cause,
// undated_lot, is named; the other position is the control.
func TestPositionInBaseGapNamesThePermanentCauseWhenTwoAreTrue(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))
	// The table starts at lateRateOn: the buys and transfer resolve, the
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

// With holes under both the lot and the dividend date, the lot is named first,
// as the contract promises; only this test can tell "checked first" from "the
// only one that failed".
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

	// Buy and dividend on the same day, so one hole stops both.
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

// twoDateConverter is oneDateConverter with holes under a set of dates.
type twoDateConverter struct {
	rate   decimal.Decimal
	rateOn time.Time
	// holes maps YYYY-MM-DD dates without a rate to Rate's error.
	holes map[string]error
}

func (c twoDateConverter) Rate(_ context.Context, _, _ string, on time.Time) (decimal.Decimal, time.Time, error) {
	if err, missing := c.holes[on.Format("2006-01-02")]; missing {
		return decimal.Decimal{}, time.Time{}, err
	}
	return c.rate, c.rateOn, nil
}

// RatesOn answers from this double's Rate.
func (c twoDateConverter) RatesOn(ctx context.Context, queries []marketdata.RateQuery) (marketdata.Rates, error) {
	return ratetest.BatchFrom(ctx, c, queries)
}

// With holes under the income date and today, the income date is named: the
// order income-then-today, invisible to single-hole tests.
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
	// A quote is needed to reach the today check at all.
	quotes.byInstrument[shareID] = marketdata.Quote{
		InstrumentID: shareID, On: mustDate(t, "2026-07-20"),
		Price: decimal.RequireFromString("120.00"), Currency: "USD", Source: "test",
	}
	// The lot resolves; only the dividend and today are holes.
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

// The gap is null when nothing stopped the object: a USD row with every rate,
// and a RUB row in a RUB space.
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

// A EUR-faced bond in a USD position with no EUR rate: in_base stands, and
// market_value_gap names the valuation currency; a separate field because it
// is another cell.
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

// market_value_gap is null only when the valuation converted. An unquoted share
// carries no_quote (#78).
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

	resp := apitest.Do(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/positions", "")
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET positions = %d: %s", resp.StatusCode, b)
	}
	var got positionsResp
	apitest.Decode(t, resp, &got)
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

// Both gaps at once: an undated lot stops the object, and the valuation is
// stuck in EUR. One field could not caption both truthfully.
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

// market_value_gap is published on a base-currency position too: it concerns
// reaching the position's currency, independent of the base.
func TestPositionMarketValueGapPublishedOnABaseCurrencyPosition(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))
	// No EUR rate: the valuation can never reach RUB.

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

// has_undated_lots is reported on a base-currency position, with a null
// in_base_gap: nothing to convert.
func TestPositionHasUndatedLotsOnABaseCurrencyPosition(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))
	// No rates: both accounts are in the base currency.

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
