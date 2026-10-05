package tinvest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/secretbox"
)

// Package tinvest, for the worker's factory and the map writer.

const rpcLastPrices = "MarketDataService/GetLastPrices"

// quotesFixture drives the price worker against the same broker stub the sync
// tests use, and remembers everything it wrote.
type quotesFixture struct {
	fixture
	broker *brokerStub
	logs   *logCapture
	quotes *recordingQuotes
	worker river.Worker[RefreshQuotesArgs]
	now    time.Time
	sealer *secretbox.Box
}

// recordingQuotes stands in for marketdata.Store, so the rows the worker builds
// (their currency above all) are the assertion.
type recordingQuotes struct {
	stored []marketdata.Quote
	err    error
}

func (r *recordingQuotes) UpsertQuotes(_ context.Context, quotes []marketdata.Quote) error {
	if r.err != nil {
		return r.err
	}
	r.stored = append(r.stored, quotes...)
	return nil
}

func newQuotesFixture(t *testing.T) *quotesFixture {
	t.Helper()
	f := newFixture(t)

	box, err := secretbox.New(bytes.Repeat([]byte{7}, secretbox.KeySize))
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	if err := f.store.UpdateConnectionToken(f.ctx, f.spaceID, f.conn.ID,
		box.Seal([]byte(testToken)), "oken"); err != nil {
		t.Fatalf("UpdateConnectionToken: %v", err)
	}
	conn, err := f.store.ConnectionByID(f.ctx, f.spaceID, f.conn.ID)
	if err != nil {
		t.Fatalf("ConnectionByID: %v", err)
	}
	f.conn = conn

	qf := &quotesFixture{
		fixture: f,
		broker:  newBrokerStub(t),
		logs:    &logCapture{},
		quotes:  &recordingQuotes{},
		// A fixed "today", so the future-price guard is asserted against a
		// literal rather than against whatever day the suite runs on.
		now:    time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC),
		sealer: box,
	}
	log := slog.New(qf.logs)
	newClient := func(token string) (*Client, error) {
		return NewClient(qf.broker.srv.Client(), qf.broker.srv.URL, token, log), nil
	}
	qf.worker = NewQuotesWorker(f.store, qf.quotes, box, newClient, log, func() time.Time { return qf.now })
	return qf
}

func (f *quotesFixture) work(t *testing.T) error {
	t.Helper()
	return f.worker.Work(f.ctx, &river.Job[RefreshQuotesArgs]{
		JobRow: &rivertype.JobRow{ID: 1},
		Args:   RefreshQuotesArgs{},
	})
}

// mapTo maps a listing with its own currency, which may differ from the
// catalog row's.
func (f *quotesFixture) mapTo(t *testing.T, uid string, instrumentID uuid.UUID, listingCurrency string) {
	t.Helper()
	if err := f.store.saveMap(f.ctx, f.conn.ID, instrumentID,
		InstrumentRef{InstrumentUID: uid}, "", "", listingCurrency); err != nil {
		t.Fatalf("saveMap(%s): %v", uid, err)
	}
}

func (f *quotesFixture) instrument(t *testing.T, ticker, currency string) instrument.Instrument {
	t.Helper()
	inst, err := instrument.NewStore(f.pool).Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: ticker, Ticker: ticker, Currency: currency,
	})
	if err != nil {
		t.Fatalf("create instrument %s: %v", ticker, err)
	}
	return inst
}

// lastPrice is one entry of the broker's answer, written by hand so the
// worker's path runs through the client's real parsing.
func lastPrice(uid, units string, nano int32, at, kind string) string {
	return fmt.Sprintf(`{"instrumentUid":%q,"price":{"units":%q,"nano":%d},"time":%q,"lastPriceType":%q}`,
		uid, units, nano, at, kind)
}

func lastPricesBody(entries ...string) string {
	return `{"lastPrices":[` + strings.Join(entries, ",") + `]}`
}

// The broker's price has no currency, and the catalog row's is not the
// listing's: Apple's row says roubles, the СПБ line is in dollars, and 313,25 $
// stamped as roubles is wrong eightyfold.
func TestQuotesWorkerStampsThePriceWithTheListingsCurrency(t *testing.T) {
	f := newQuotesFixture(t)
	inst := f.instrument(t, "AAPL", "RUB")
	f.mapTo(t, "uid-aapl-spb", inst.ID, "USD")
	f.broker.answer(rpcLastPrices, 200, lastPricesBody(
		lastPrice("uid-aapl-spb", "313", 250000000, "2026-08-07T23:28:00Z", "LAST_PRICE_EXCHANGE")))

	if err := f.work(t); err != nil {
		t.Fatalf("Work: %v", err)
	}

	stored := f.quotes.stored
	if len(stored) != 1 {
		t.Fatalf("stored %d quotes, want 1: %+v", len(stored), stored)
	}
	q := stored[0]
	if q.Currency != "USD" {
		t.Errorf("currency = %q, want USD — the listing's, not the catalog row's %q", q.Currency, inst.Currency)
	}
	if q.InstrumentID != inst.ID {
		t.Errorf("instrument_id = %s, want %s", q.InstrumentID, inst.ID)
	}
	if got := q.Price.String(); got != "313.25" {
		t.Errorf("price = %s, want 313.25", got)
	}
	if q.Source != SourceExchange {
		t.Errorf("source = %q, want %q", q.Source, SourceExchange)
	}
	// The broker's instant is 23:28 UTC on the 7th, which is already the 8th in
	// Moscow — the same day rule the journal keeps.
	if want := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC); !q.On.Equal(want) {
		t.Errorf("on = %s, want %s", q.On.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// A dealer's price is stored and marked as such: for the owner's eight FinEx
// funds it is the only price.
func TestQuotesWorkerKeepsADealersPriceApartFromAnExchanges(t *testing.T) {
	f := newQuotesFixture(t)
	fund := f.instrument(t, "FXGD", "RUB")
	share := f.instrument(t, "SBER", "RUB")
	f.mapTo(t, "uid-fxgd-otc", fund.ID, "RUB")
	f.mapTo(t, "uid-sber", share.ID, "RUB")
	f.broker.answer(rpcLastPrices, 200, lastPricesBody(
		lastPrice("uid-fxgd-otc", "23", 820000000, "2026-08-07T20:34:27Z", "LAST_PRICE_DEALER"),
		lastPrice("uid-sber", "300", 0, "2026-08-07T15:00:00Z", "LAST_PRICE_EXCHANGE")))

	if err := f.work(t); err != nil {
		t.Fatalf("Work: %v", err)
	}

	bySource := map[uuid.UUID]string{}
	for _, q := range f.quotes.stored {
		bySource[q.InstrumentID] = q.Source
	}
	if bySource[fund.ID] != SourceDealer {
		t.Errorf("the fund's source = %q, want %q", bySource[fund.ID], SourceDealer)
	}
	if bySource[share.ID] != SourceExchange {
		t.Errorf("the share's source = %q, want %q", bySource[share.ID], SourceExchange)
	}
}

// An entry with a uid and nothing else stores nothing: a zero would look
// like a real collapse.
func TestQuotesWorkerStoresNothingForAnInstrumentWithNoPrice(t *testing.T) {
	f := newQuotesFixture(t)
	inst := f.instrument(t, "FXIT", "RUB")
	f.mapTo(t, "uid-nothing", inst.ID, "RUB")
	f.broker.answer(rpcLastPrices, 200,
		`{"lastPrices":[{"instrumentUid":"uid-nothing","lastPriceType":"LAST_PRICE_UNSPECIFIED"}]}`)

	if err := f.work(t); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(f.quotes.stored) != 0 {
		t.Errorf("stored %+v, want nothing at all", f.quotes.stored)
	}
}

// A stopped paper's price keeps the day it was struck (Tesla's old rouble
// line: 2022-02-25); the date is how staleness shows.
func TestQuotesWorkerKeepsAPriceThatStoppedMoving(t *testing.T) {
	f := newQuotesFixture(t)
	inst := f.instrument(t, "TSLARM", "RUB")
	f.mapTo(t, "uid-frozen", inst.ID, "RUB")
	f.broker.answer(rpcLastPrices, 200, lastPricesBody(
		lastPrice("uid-frozen", "58424", 0, "2022-02-25T10:10:00Z", "LAST_PRICE_EXCHANGE")))

	if err := f.work(t); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(f.quotes.stored) != 1 {
		t.Fatalf("stored %d quotes, want 1", len(f.quotes.stored))
	}
	if want := time.Date(2022, 2, 25, 0, 0, 0, 0, time.UTC); !f.quotes.stored[0].On.Equal(want) {
		t.Errorf("on = %s, want %s — the day the price was struck, not today",
			f.quotes.stored[0].On.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// A future-dated price would outrank every real one until its day.
func TestQuotesWorkerRefusesAPriceDatedInTheFuture(t *testing.T) {
	f := newQuotesFixture(t)
	inst := f.instrument(t, "GLITCH", "RUB")
	f.mapTo(t, "uid-future", inst.ID, "RUB")
	f.broker.answer(rpcLastPrices, 200, lastPricesBody(
		lastPrice("uid-future", "100", 0, "2027-01-01T10:00:00Z", "LAST_PRICE_EXCHANGE")))

	if err := f.work(t); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(f.quotes.stored) != 0 {
		t.Errorf("stored %+v, want nothing", f.quotes.stored)
	}
}

// A listing with no currency (pre-0017) and an unreachable passport stays
// unpriced; nothing is guessed.
func TestQuotesWorkerLeavesAListingItCannotDenominateUnpriced(t *testing.T) {
	f := newQuotesFixture(t)
	inst := f.instrument(t, "OLDMAP", "RUB")
	f.mapTo(t, "uid-currencyless", inst.ID, "")
	f.broker.answer(rpcInstrumentB, 503, `{"code":13,"message":"unavailable"}`)
	f.broker.answer(rpcLastPrices, 200, lastPricesBody(
		lastPrice("uid-currencyless", "100", 0, "2026-08-07T10:00:00Z", "LAST_PRICE_EXCHANGE")))

	if err := f.work(t); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(f.quotes.stored) != 0 {
		t.Errorf("stored %+v, want nothing — the listing's currency is unknown", f.quotes.stored)
	}
	// And the broker was not asked for a price it could not have filed.
	if n := f.broker.callCount(rpcLastPrices); n != 0 {
		t.Errorf("asked for prices %d times, want 0: there was nothing askable", n)
	}
}

// TestQuotesWorkerLearnsAListingsCurrencyOnce is the other half: the passport
// answers, the currency is remembered, and a second run does not ask again.
func TestQuotesWorkerLearnsAListingsCurrencyOnce(t *testing.T) {
	f := newQuotesFixture(t)
	inst := f.instrument(t, "LEARN", "RUB")
	f.mapTo(t, "uid-learn", inst.ID, "")
	f.broker.answer(rpcInstrumentB, 200,
		`{"instrument":{"uid":"uid-learn","ticker":"LEARN","name":"Learn","currency":"usd","instrumentType":"share"}}`)
	f.broker.answer(rpcLastPrices, 200, lastPricesBody(
		lastPrice("uid-learn", "10", 0, "2026-08-07T10:00:00Z", "LAST_PRICE_EXCHANGE")))

	if err := f.work(t); err != nil {
		t.Fatalf("first Work: %v", err)
	}
	if len(f.quotes.stored) != 1 || f.quotes.stored[0].Currency != "USD" {
		t.Fatalf("stored %+v, want one quote in USD", f.quotes.stored)
	}

	f.quotes.stored = nil
	if err := f.work(t); err != nil {
		t.Fatalf("second Work: %v", err)
	}
	if n := f.broker.callCount(rpcInstrumentB); n != 1 {
		t.Errorf("asked the passport %d times over two runs, want 1 — the answer is remembered", n)
	}
	if len(f.quotes.stored) != 1 || f.quotes.stored[0].Currency != "USD" {
		t.Errorf("second run stored %+v, want one quote in USD from the remembered currency", f.quotes.stored)
	}
}

// A revoked token parks the connection and does not fail the job.
func TestQuotesWorkerParksAConnectionWhoseTokenTheBrokerRefuses(t *testing.T) {
	f := newQuotesFixture(t)
	inst := f.instrument(t, "ANY", "RUB")
	f.mapTo(t, "uid-any", inst.ID, "RUB")
	f.broker.answer(rpcLastPrices, 401, `{"code":16,"message":"unauthenticated","description":"40003"}`)

	if err := f.work(t); err != nil {
		t.Fatalf("Work returned %v, want nil: a retry cannot un-revoke a token", err)
	}
	conn, err := f.store.ConnectionByID(f.ctx, f.spaceID, f.conn.ID)
	if err != nil {
		t.Fatalf("ConnectionByID: %v", err)
	}
	if conn.Status != StatusTokenRevoked {
		t.Errorf("status = %q, want %q", conn.Status, StatusTokenRevoked)
	}
}

// A map-hit resolution passes "" for the currency and must not blank the one
// the row has, which would unprice the listing silently.
func TestSavingAMappingWithoutACurrencyKeepsTheOneItHas(t *testing.T) {
	f := newQuotesFixture(t)
	inst := f.instrument(t, "KEEP", "RUB")
	f.mapTo(t, "uid-keep", inst.ID, "USD")

	// Resolved again from the map, with something else changed so the upsert
	// writes.
	if err := f.store.saveMap(f.ctx, f.conn.ID, inst.ID,
		InstrumentRef{InstrumentUID: "uid-keep", FIGI: "BBG000B9XRY4"}, "US0378331005", "KEEP", ""); err != nil {
		t.Fatalf("saveMap: %v", err)
	}

	listings, err := f.store.QuotableByConnection(f.ctx, f.conn.ID)
	if err != nil {
		t.Fatalf("QuotableByConnection: %v", err)
	}
	if len(listings) != 1 {
		t.Fatalf("listings = %+v, want 1", listings)
	}
	if listings[0].Currency != "USD" {
		t.Errorf("currency = %q, want USD — a call with nothing to say must not erase what is known", listings[0].Currency)
	}
	// And the fact that DID change went in, so this is not passing because the
	// upsert wrote nothing at all.
	if listings[0].InstrumentUID != "uid-keep" {
		t.Fatalf("listing = %+v", listings[0])
	}
	var figi string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT figi FROM tinvest_instrument_map WHERE connection_id = $1 AND instrument_uid = 'uid-keep'`,
		f.conn.ID).Scan(&figi); err != nil {
		t.Fatalf("read figi: %v", err)
	}
	if figi != "BBG000B9XRY4" {
		t.Errorf("figi = %q, want the identifier the second call carried", figi)
	}
}

// The count explaining a cash gap is per link and per reason.
func TestCurrencyTradesAreCountedPerLinkAndByReason(t *testing.T) {
	f := newQuotesFixture(t)
	other := f.secondLink(t)

	f.markUnparsed(t, f.link.ID, "cur-1", string(ReasonCurrencyTrade))
	f.markUnparsed(t, f.link.ID, "cur-2", string(ReasonCurrencyTrade))
	f.markUnparsed(t, f.link.ID, "other-reason", string(ReasonUnsupportedType))
	f.markUnparsed(t, f.link.ID, "read-fine", "")
	f.markUnparsed(t, other.ID, "cur-3", string(ReasonCurrencyTrade))

	got, err := f.store.CurrencyTradesUnparsedByLink(f.ctx, f.conn.ID)
	if err != nil {
		t.Fatalf("CurrencyTradesUnparsedByLink: %v", err)
	}
	if got[f.link.ID] != 2 {
		t.Errorf("first account = %d, want 2 — its own currency trades and nothing else", got[f.link.ID])
	}
	if got[other.ID] != 1 {
		t.Errorf("second account = %d, want 1", got[other.ID])
	}
	if len(got) != 2 {
		t.Errorf("map holds %d links, want 2: %+v", len(got), got)
	}
}

// markUnparsed writes a mirror row with a given reason straight to the
// table; the grouping is under test, not the projection.
func (f *quotesFixture) markUnparsed(t *testing.T, linkID uuid.UUID, key, reason string) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO tinvest_operations_mirror (
			connection_id, link_id, broker_operation_id, op_type, state,
			occurred_at, currency, payment, raw, content_key,
			last_confirmed_at, unparsed_reason)
		VALUES ($1, $2, $3, 'OPERATION_TYPE_BUY', 'OPERATION_STATE_EXECUTED',
			now(), 'RUB', 0, '{}'::jsonb, $3, now(), $4)`,
		f.conn.ID, linkID, key, reason); err != nil {
		t.Fatalf("seed mirror row %s: %v", key, err)
	}
}

// Pricing a holding nobody imported (#137).

// listing mirrors FindInstrument, with no currency: giving one here once let
// these tests pass against a filter that matched nothing in production.
func listing(uid, isin, ticker, class, kind string) Listing {
	return Listing{UID: uid, ISIN: isin, Ticker: ticker, ClassCode: class, Kind: kind}
}

// Only the same paper is a candidate.
func TestCandidateListingsRefuseEverythingButTheSamePaper(t *testing.T) {
	want := UnmappedHeldInstrument{ISIN: "US0378331005", Ticker: "AAPL", Type: "share", Currency: "USD"}
	found := []Listing{
		listing("uid-spb", "US0378331005", "AAPL", "SPBXM", "INSTRUMENT_TYPE_SHARE"),
		listing("uid-a25", "US0378331005", "AAPL", "A25", "INSTRUMENT_TYPE_SHARE"),
		// The old rouble line: a candidate (the search has no currency), stopped
		// later by its passport and in practice by its frozen price.
		listing("uid-rm", "US0378331005", "AAPL-RM", "FQBR", "INSTRUMENT_TYPE_SHARE"),
		// Another issuer's paper under the same ticker: only the ISIN tells them
		// apart.
		listing("uid-other", "RU000A107UL4", "AAPL", "TQBR", "INSTRUMENT_TYPE_SHARE"),
		// The same paper under another ticker ("-RM" lines): kept, which ticker
		// matching would not do.
		listing("uid-rm-usd", "US0378331005", "AAPL-RM", "MTQR", "INSTRUMENT_TYPE_SHARE"),
		// The right ISIN, the wrong kind of asset: a bond's quote is a percent
		// of par and would be read here as money per share.
		listing("uid-bond", "US0378331005", "AAPL", "TQCB", "INSTRUMENT_TYPE_BOND"),
	}

	got := candidateListings(want, found)
	kept := map[string]bool{}
	for _, l := range got {
		kept[l.UID] = true
	}
	want3 := []string{"uid-spb", "uid-a25", "uid-rm-usd", "uid-rm"}
	for _, uid := range want3 {
		if !kept[uid] {
			t.Errorf("dropped %q, want it kept — it is this paper, in this currency, of this kind", uid)
		}
	}
	if kept["uid-other"] {
		t.Error("kept uid-other: another issuer's share under the same ticker, told apart only by its ISIN")
	}
	if len(got) != len(want3) {
		t.Errorf("kept %d listings, want %d: %+v", len(got), len(want3), got)
	}
}

// The freshest price wins: Apple's live lines over those frozen since
// 2022, with no list of venue names.
func TestPickListingTakesTheOneStillBeingQuoted(t *testing.T) {
	live := listing("uid-live", "US0378331005", "AAPL", "SPBXM", "INSTRUMENT_TYPE_SHARE")
	frozen := listing("uid-frozen", "US0378331005", "AAPL-RM", "FQBR", "INSTRUMENT_TYPE_SHARE")
	prices := map[string]LastPrice{
		"uid-live":   {InstrumentUID: "uid-live", Price: decimal.RequireFromString("313.25"), At: time.Date(2026, 8, 7, 23, 28, 0, 0, time.UTC)},
		"uid-frozen": {InstrumentUID: "uid-frozen", Price: decimal.RequireFromString("58424"), At: time.Date(2022, 2, 25, 10, 10, 0, 0, time.UTC)},
	}

	// Offered in the order that would trip a "first one wins" implementation.
	got, price, ok := pickListing([]Listing{frozen, live}, prices)
	if !ok {
		t.Fatal("refused to pick, want the listing still being quoted")
	}
	if got.UID != "uid-live" {
		t.Errorf("picked %q, want uid-live", got.UID)
	}
	if price.Price.String() != "313.25" {
		t.Errorf("price = %s, want 313.25", price.Price)
	}
}

// Two equally fresh prices that disagree: refused rather than an unnamed
// venue's price on the holding.
func TestPickListingRefusesTwoEqallyFreshPricesThatDisagree(t *testing.T) {
	at := time.Date(2026, 8, 7, 23, 59, 0, 0, time.UTC)
	a := listing("uid-a", "US0378331005", "AAPL", "SPBXM", "INSTRUMENT_TYPE_SHARE")
	b := listing("uid-b", "US0378331005", "AAPL", "A25", "INSTRUMENT_TYPE_SHARE")

	disagree := map[string]LastPrice{
		"uid-a": {InstrumentUID: "uid-a", Price: decimal.RequireFromString("313.25"), At: at},
		"uid-b": {InstrumentUID: "uid-b", Price: decimal.RequireFromString("311.00"), At: at},
	}
	if _, _, ok := pickListing([]Listing{a, b}, disagree); ok {
		t.Error("picked one of two same-day prices that disagree, want a refusal")
	}

	// The same tie at the same price is one fact twice.
	agree := map[string]LastPrice{
		"uid-a": {InstrumentUID: "uid-a", Price: decimal.RequireFromString("313.25"), At: at},
		"uid-b": {InstrumentUID: "uid-b", Price: decimal.RequireFromString("313.25"), At: at},
	}
	if _, price, ok := pickListing([]Listing{a, b}, agree); !ok || price.Price.String() != "313.25" {
		t.Errorf("refused two identical prices, want 313.25 (ok=%v)", ok)
	}
}

// TestPickListingRefusesWhenNothingIsQuoted. A candidate with no price is not
// a candidate; with none of them priced there is nothing to pick.
func TestPickListingRefusesWhenNothingIsQuoted(t *testing.T) {
	a := listing("uid-a", "US0378331005", "AAPL", "SPBXM", "INSTRUMENT_TYPE_SHARE")
	if _, _, ok := pickListing([]Listing{a}, map[string]LastPrice{}); ok {
		t.Error("picked a listing the broker quoted no price for")
	}
	zero := map[string]LastPrice{"uid-a": {InstrumentUID: "uid-a", At: time.Now()}}
	if _, _, ok := pickListing([]Listing{a}, zero); ok {
		t.Error("picked a listing whose price is nought, which is no price")
	}
}

// A rouble line of Coca-Cola traded last and a dollar line before it: the
// dollar holding gets the dollar line (#261).
func TestResolveListingPassesOverAListingInAnotherCurrency(t *testing.T) {
	passports := map[string]string{"uid-ko-rub": "rub", "uid-ko-usd": "usd"}
	asked := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rpc := strings.TrimPrefix(r.URL.Path, rpcPathPrefix)
		switch rpc {
		case "InstrumentsService/FindInstrument":
			_, _ = w.Write([]byte(`{"instruments":[
				{"uid":"uid-ko-rub","isin":"US1912161007","ticker":"KO-RM","classCode":"FQBR","instrumentKind":"INSTRUMENT_TYPE_SHARE"},
				{"uid":"uid-ko-usd","isin":"US1912161007","ticker":"KO","classCode":"SPBXM","instrumentKind":"INSTRUMENT_TYPE_SHARE"}]}`))
		case rpcLastPrices:
			_, _ = w.Write([]byte(lastPricesBody(
				lastPrice("uid-ko-rub", "5000", 0, "2026-08-07T20:00:00Z", "LAST_PRICE_EXCHANGE"),
				lastPrice("uid-ko-usd", "61", 0, "2026-08-06T20:00:00Z", "LAST_PRICE_EXCHANGE"))))
		case "InstrumentsService/GetInstrumentBy":
			var req struct {
				ID string `json:"id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			asked = append(asked, req.ID)
			_, _ = fmt.Fprintf(w, `{"instrument":{"uid":%q,"currency":%q}}`, req.ID, passports[req.ID])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	log := slog.New(&logCapture{})
	client := NewClient(srv.Client(), srv.URL, testToken, log)

	got, price, currency, ok, err := resolveListing(context.Background(), client, log,
		UnmappedHeldInstrument{ISIN: "US1912161007", Ticker: "KO", Type: "share", Currency: "USD"})
	if err != nil || !ok {
		t.Fatalf("resolveListing: ok=%v err=%v, want the dollar line", ok, err)
	}
	if got.UID != "uid-ko-usd" || currency != "USD" || price.Price.String() != "61" {
		t.Errorf("chose %s in %s at %s, want uid-ko-usd in USD at 61", got.UID, currency, price.Price)
	}
	if strings.Join(asked, ",") != "uid-ko-rub,uid-ko-usd" {
		t.Errorf("asked passports %v, want the freshest first and then the next", asked)
	}

	passports["uid-ko-usd"] = "eur"
	asked = nil
	if _, _, _, ok, err := resolveListing(context.Background(), client, log,
		UnmappedHeldInstrument{ISIN: "US1912161007", Ticker: "KO", Type: "share", Currency: "USD"}); ok || err != nil {
		t.Errorf("no listing in dollars: ok=%v err=%v, want it left unpriced", ok, err)
	}
}
