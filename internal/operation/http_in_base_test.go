package operation_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/marketdata/ratetest"
)

// operationInBase mirrors apitypes.OperationInBase for decoding in tests.
type operationInBase struct {
	AmountMinor int64  `json:"amount_minor"`
	FeeMinor    int64  `json:"fee_minor"`
	Currency    string `json:"currency"`
	RateOn      string `json:"rate_on"`
	// DatedOn is the date the headline rate was asked for; RateOn equals it
	// only when that day had a rate (#80).
	DatedOn string `json:"dated_on"`
}

// journalItem is the part of apitypes.Operation these tests read. A nil
// *operationInBase is both an omitted key and an explicit null.
type journalItem struct {
	ID          string           `json:"id"`
	OccurredOn  string           `json:"occurred_on"`
	AmountMinor int64            `json:"amount_minor"`
	FeeMinor    int64            `json:"fee_minor"`
	Currency    string           `json:"currency"`
	InBase      *operationInBase `json:"in_base"`
	// HasUndatedLots says in_base is null for an unrecorded purchase date, not
	// a missing rate.
	HasUndatedLots bool `json:"has_undated_lots"`
	// AssembledFromLots is published whether or not in_base exists (#67).
	AssembledFromLots bool `json:"assembled_from_lots"`
	// InBaseGap is decoded as a plain string so a test sees the wire value
	// (#79).
	InBaseGap string `json:"in_base_gap"`
}

// listJournal fetches GET .../operations and returns the page's rows.
// Envelope tests use getJournalPage (http_pagination_test.go).
func listJournal(t *testing.T, url string, c *http.Client, accountID string) []journalItem {
	t.Helper()
	resp := do(t, c, "GET", url+"/api/v1/accounts/"+accountID+"/operations", "")
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("list operations = %d: %s", resp.StatusCode, b)
	}
	var page journalPage
	decodeJSON(t, resp, &page)
	return page.Operations
}

// mkAccount creates an account in currency and returns its id.
func mkAccount(t *testing.T, url string, c *http.Client, name, currency string) string {
	t.Helper()
	resp := do(t, c, "POST", url+"/api/v1/accounts",
		fmt.Sprintf(`{"name":%q,"type":"brokerage","currency":%q}`, name, currency))
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create %s account = %d: %s", currency, resp.StatusCode, b)
	}
	var a idResp
	decodeJSON(t, resp, &a)
	return a.ID
}

// mkInstrument creates an instrument and returns its id.
func mkInstrument(t *testing.T, url string, c *http.Client, body string) string {
	t.Helper()
	resp := do(t, c, "POST", url+"/api/v1/instruments", body)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create instrument = %d: %s", resp.StatusCode, b)
	}
	var i idResp
	decodeJSON(t, resp, &i)
	return i.ID
}

// mkOperation creates one operation and returns its id.
func mkOperation(t *testing.T, url string, c *http.Client, body string) string {
	t.Helper()
	resp := do(t, c, "POST", url+"/api/v1/operations", body)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create operation = %d: %s", resp.StatusCode, b)
	}
	var o idResp
	decodeJSON(t, resp, &o)
	return o.ID
}

// findOperation picks an operation by id, so tests do not depend on order.
func findOperation(t *testing.T, list []journalItem, id string) journalItem {
	t.Helper()
	for _, o := range list {
		if o.ID == id {
			return o
		}
	}
	t.Fatalf("operation %s not found in journal of %d items", id, len(list))
	return journalItem{}
}

// mustDate parses a YYYY-MM-DD fixture date.
func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse date %q: %v", s, err)
	}
	return d
}

// seedFxRate stores one USD->RUB rate on the given date.
func seedFxRate(t *testing.T, mdStore *marketdata.Store, on, rate string) {
	t.Helper()
	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: mustDate(t, on), Rate: decimal.RequireFromString(rate), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rate %s@%s: %v", rate, on, err)
	}
}

// A USD purchase in a RUB space with a rate on its own date is converted at
// that rate.
//
// Rate 65.4567 RUB per USD:
//
//	amount_minor -123_456 * 65.4567 = -8_081_022.3552 -> -8_081_022
//	fee_minor        799 * 65.4567 =     52_299.9033 ->     52_300
//
// The fee rounds up while the amount rounds down in magnitude, which needs the
// two converted separately. rate_on is 2019-03-12. A much later rate of 100 is
// seeded too, so converting at today's rate would fail every assertion.
func TestListOperationInBaseConvertsAtOperationDate(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	seedFxRate(t, mdStore, "2019-03-12", "65.4567")
	seedFxRate(t, mdStore, "2025-01-09", "100")

	acc := mkAccount(t, url, c, "US брокер", "USD")
	share := mkInstrument(t, url, c,
		`{"type":"share","name":"Apple","ticker":"AAPL","currency":"USD"}`)
	buy := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2019-03-12","quantity":"10","price":"123.456",
		"amount_minor":-123456,"currency":"USD","fee_minor":799}`, acc, share))

	op := findOperation(t, listJournal(t, url, c, acc), buy)
	if op.InBase == nil {
		t.Fatalf("in_base = null, want a conversion at the 2019-03-12 rate")
	}
	if op.InBase.AmountMinor != -8081022 {
		t.Errorf("in_base.amount_minor = %d, want -8081022 (-123456 * 65.4567, sign preserved)", op.InBase.AmountMinor)
	}
	if op.InBase.FeeMinor != 52300 {
		t.Errorf("in_base.fee_minor = %d, want 52300 (799 * 65.4567, rounded on its own)", op.InBase.FeeMinor)
	}
	if op.InBase.Currency != "RUB" {
		t.Errorf("in_base.currency = %q, want RUB (the space's base currency)", op.InBase.Currency)
	}
	if op.InBase.RateOn != "2019-03-12" {
		t.Errorf("in_base.rate_on = %q, want 2019-03-12 (the operation's own date, where the rate lives)", op.InBase.RateOn)
	}
}

// The fee is converted on its own, not derived from the total. At rate 65.50
// both products land on exactly half a minor unit:
//
//	amount_minor 12_345 * 65.50 = 808_597.50 -> 808_598
//	fee_minor       799 * 65.50 =  52_334.50 ->  52_335
//	combined     13_144 * 65.50 = 860_932.00 -> 860_932
//
// Derived from the total the fee would be 52_334. A dividend keeps both figures
// positive so the sign does not mask the difference.
func TestListOperationInBaseFeeIsNotDerivedFromTheCombinedTotal(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	seedFxRate(t, mdStore, "2019-03-12", "65.50")

	acc := mkAccount(t, url, c, "US брокер", "USD")
	share := mkInstrument(t, url, c,
		`{"type":"share","name":"Apple","ticker":"AAPL","currency":"USD"}`)
	div := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend",
		"occurred_on":"2019-03-12","amount_minor":12345,"currency":"USD","fee_minor":799}`, acc, share))

	op := findOperation(t, listJournal(t, url, c, acc), div)
	if op.InBase == nil {
		t.Fatalf("in_base = null, want a conversion at the 2019-03-12 rate")
	}
	if op.InBase.AmountMinor != 808598 {
		t.Errorf("in_base.amount_minor = %d, want 808598 (12345 * 65.50)", op.InBase.AmountMinor)
	}
	if op.InBase.FeeMinor != 52335 {
		t.Errorf("in_base.fee_minor = %d, want 52335 (799 * 65.50 rounded on its own; "+
			"52334 means the fee was derived from the combined total)", op.InBase.FeeMinor)
	}
}

// An operation on Sunday 2019-03-17 has no rate of its own; FxRateOn falls
// back to Friday 2019-03-15 and rate_on must say so.
//
//	-10_000 * 65 = -650_000
func TestListOperationInBaseUsesNearestEarlierRateDate(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	// Friday's rate only, plus a much later one, so "nearest before the
	// operation" and "nearest before today" differ.
	seedFxRate(t, mdStore, "2019-03-15", "65")
	seedFxRate(t, mdStore, "2025-01-09", "100")

	acc := mkAccount(t, url, c, "US брокер", "USD")
	wd := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2019-03-17","amount_minor":-10000,"currency":"USD"}`, acc))

	op := findOperation(t, listJournal(t, url, c, acc), wd)
	if op.InBase == nil {
		t.Fatalf("in_base = null, want the nearest earlier (2019-03-15) rate to be used")
	}
	if op.InBase.RateOn != "2019-03-15" {
		t.Errorf("in_base.rate_on = %q, want 2019-03-15 — the date of the rate ACTUALLY used, not occurred_on (2019-03-17)", op.InBase.RateOn)
	}
	if op.InBase.AmountMinor != -650000 {
		t.Errorf("in_base.amount_minor = %d, want -650000 (-10000 * 65)", op.InBase.AmountMinor)
	}
}

// An operation already in the base currency has nothing to convert:
// in_base is null, not a copy at rate 1.
func TestListOperationInBaseNullWhenBaseCurrency(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	// A rate exists, so null can only come from the base-currency case.
	seedFxRate(t, mdStore, "2019-03-12", "65")

	acc := mkAccount(t, url, c, "Рублёвый брокер", "RUB")
	wd := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2019-03-12","amount_minor":-10000,"currency":"RUB","fee_minor":100}`, acc))

	op := findOperation(t, listJournal(t, url, c, acc), wd)
	if op.InBase != nil {
		t.Fatalf("in_base = %+v, want null (operation already in the base currency)", op.InBase)
	}
}

// The only rate is later than the operation: in_base is null as a whole,
// never filled from a future rate, and the request still answers 200.
func TestListOperationInBaseNullWhenNoRateOnOrBeforeDate(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	// Seeded AFTER the operation's date: nothing on or before 2019-03-12.
	seedFxRate(t, mdStore, "2020-01-10", "65")

	acc := mkAccount(t, url, c, "US брокер", "USD")
	wd := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2019-03-12","amount_minor":-10000,"currency":"USD"}`, acc))

	list := listJournal(t, url, c, acc)
	op := findOperation(t, list, wd)
	if op.InBase != nil {
		t.Fatalf("in_base = %+v, want null (no USD->RUB rate on or before 2019-03-12)", op.InBase)
	}
}

// converterLike mirrors the handler's unexported converter interface so this
// package can name the double's type.
type converterLike interface {
	Rate(ctx context.Context, from, to string, on time.Time) (decimal.Decimal, time.Time, error)
	RatesOn(ctx context.Context, queries []marketdata.RateQuery) (marketdata.Rates, error)
}

// failingConverter fails every lookup with err, standing in for an outage
// rather than marketdata.ErrNoRate, which a real converter cannot be made to
// produce on demand.
type failingConverter struct{ err error }

func (c failingConverter) Rate(_ context.Context, _, _ string, _ time.Time) (decimal.Decimal, time.Time, error) {
	return decimal.Decimal{}, time.Time{}, c.err
}

func (c failingConverter) RatesOn(ctx context.Context, queries []marketdata.RateQuery) (marketdata.Rates, error) {
	return ratetest.BatchFrom(ctx, c, queries)
}

// A genuine failure resolving a rate fails the request rather than becoming
// in_base: null, or an outage would look like an ordinary missing rate.
func TestListOperationInBaseRealRateErrorFailsRequest(t *testing.T) {
	url, c := newAPIWithConverterDouble(t, failingConverter{err: errors.New("connection reset by peer")})

	// USD in a RUB space, so the lookup really happens.
	acc := mkAccount(t, url, c, "US брокер", "USD")
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2019-03-12","amount_minor":-10000,"currency":"USD"}`, acc))

	resp := do(t, c, "GET", url+"/api/v1/accounts/"+acc+"/operations", "")
	if resp.StatusCode != http.StatusInternalServerError {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET operations with a failing rate lookup = %d, want 500 — a real outage must not be served as a 200 with in_base: null: %s",
			resp.StatusCode, b)
	}
}

// The memo key includes the date: two USD operations on different dates get
// different rates, and two on the same date share one lookup.
//
//	2019-03-12 @ 60: -10_000 * 60 = -600_000 ; -20_000 * 60 = -1_200_000
//	2019-04-12 @ 70: -10_000 * 70 =  -700_000
func TestListOperationInBaseMemoizesRatePerCurrencyAndDate(t *testing.T) {
	pool, mdStore := newTestPool(t)
	conv := &ratetest.Counting{Inner: marketdata.NewConverter(mdStore)}
	url, c := newAPIOn(t, pool, conv)

	seedFxRate(t, mdStore, "2019-03-12", "60")
	seedFxRate(t, mdStore, "2019-04-12", "70")

	acc := mkAccount(t, url, c, "US брокер", "USD")
	marchA := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2019-03-12","amount_minor":-10000,"currency":"USD"}`, acc))
	marchB := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2019-03-12","amount_minor":-20000,"currency":"USD"}`, acc))
	april := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal",
		"occurred_on":"2019-04-12","amount_minor":-10000,"currency":"USD"}`, acc))

	// Only the listing below is under measurement.
	conv.Reset()
	list := listJournal(t, url, c, acc)

	for _, tc := range []struct {
		name       string
		id         string
		wantMinor  int64
		wantRateOn string
	}{
		{"march A", marchA, -600000, "2019-03-12"},
		{"march B", marchB, -1200000, "2019-03-12"},
		{"april", april, -700000, "2019-04-12"},
	} {
		op := findOperation(t, list, tc.id)
		if op.InBase == nil {
			t.Fatalf("%s in_base = null, want a conversion", tc.name)
		}
		if op.InBase.AmountMinor != tc.wantMinor {
			t.Errorf("%s in_base.amount_minor = %d, want %d — each date must use its own rate",
				tc.name, op.InBase.AmountMinor, tc.wantMinor)
		}
		if op.InBase.RateOn != tc.wantRateOn {
			t.Errorf("%s in_base.rate_on = %q, want %q", tc.name, op.InBase.RateOn, tc.wantRateOn)
		}
	}

	// One rate per distinct (currency, date), in one batch, nothing left for
	// the fallback. Round-trip cost is pinned by
	// TestJournalRoundTripsDoNotGrowWithTheData.
	if got := conv.Queries.Load(); got != 2 {
		t.Errorf("rates asked for = %d, want 2 — one per distinct (currency, date), so the two 2019-03-12 operations share a single one", got)
	}
	if got := conv.Batches.Load(); got != 1 {
		t.Errorf("batched resolutions = %d, want 1 — the whole page's rates are resolved in one go", got)
	}
	if got := conv.Singles.Load(); got != 0 {
		t.Errorf("one-pair fallback lookups = %d, want 0 — every date this page needs was enumerated and prewarmed", got)
	}
}

// Р-3: a purchase with a settlement day is converted at that day's rate.
func TestListOperationInBaseConvertsATradeAtItsSettlementDay(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	seedFxRate(t, mdStore, "2019-03-12", "65")
	seedFxRate(t, mdStore, "2019-03-14", "70")

	acc := mkAccount(t, url, c, "US брокер", "USD")
	share := mkInstrument(t, url, c, `{"type":"share","name":"Apple","ticker":"AAPL","currency":"USD"}`)
	buy := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2019-03-12","settled_on":"2019-03-14","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc, share))

	op := findOperation(t, listJournal(t, url, c, acc), buy)
	if op.InBase == nil || op.InBase.AmountMinor != -7000000 || op.InBase.RateOn != "2019-03-14" {
		t.Errorf("in_base = %+v, want -7000000 at the 2019-03-14 rate — the settlement day", op.InBase)
	}
}
