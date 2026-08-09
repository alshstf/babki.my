package tinvest

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
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

// This file is package tinvest so it can reach the worker's own factory and the
// map table's writer. Everything it asserts is a stored row or a returned
// error, both of which are public facts.

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
}

// recordingQuotes stands in for marketdata.Store. A fake rather than the real
// store because what is under test is WHICH rows the worker builds — the
// currency it stamps them with above all — and a fake makes that the assertion
// instead of a second query.
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
		now: time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC),
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

// mapTo puts one broker listing on the map, with the currency THAT LISTING is
// denominated in — which is the whole subject of these tests and is deliberately
// allowed to differ from the catalog row's.
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

// TestQuotesWorkerStampsThePriceWithTheListingsCurrency is the reason the
// listing's currency is recorded at all.
//
// The broker's price answer carries NO currency. The catalog row does carry
// one, and reaching for it is the mistake this guards: Apple's catalog row here
// says rubles — a row that could have been created from any listing of the
// paper — while the СПБ line the price came from is in dollars. Stamping the
// price with the row's currency would file 313,25 $ as 313,25 ₽, which is a
// figure of the right shape and the wrong magnitude by a factor of eighty.
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

// TestQuotesWorkerKeepsADealersPriceApartFromAnExchangesPins the one thing
// that distinguishes a delisted fund's only price from a market one. Both are
// stored — the dealer's is what those units could actually be sold at, and for
// the owner's eight FinEx funds it is the only price anywhere — and the row
// says which it is.
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

// TestQuotesWorkerStoresNothingForAnInstrumentWithNoPrice. The broker really
// does answer with an entry carrying a uid and nothing else — no price, no
// figi, no ticker — and a zero is not what that means. A stored zero would
// value the whole holding at nought and look exactly like a real collapse.
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

// TestQuotesWorkerKeepsAPriceThatStoppedMoving. A paper that stopped trading
// still answers, with the day its price was last struck — Tesla's old ruble
// line answers with 2022-02-25 to this day. The date is the whole of how a
// stale price announces itself, so it is stored as the broker gave it and not
// as today.
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

// TestQuotesWorkerRefusesAPriceDatedInTheFuture. The latest quote is chosen by
// ORDER BY on_date DESC, so one row dated ahead outranks every genuine refresh
// after it until that day arrives — silently, on every position the instrument
// appears in.
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

// TestQuotesWorkerLeavesAListingItCannotDenominateUnpriced. A mapping written
// before the currency was kept (migration 0017) has none, and the passport that
// would supply it is unreachable here. Nothing is guessed: pricing it under the
// catalog row's currency is the one answer that would look right and be wrong.
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

// TestQuotesWorkerParksAConnectionWhoseTokenTheBrokerRefuses. Retrying cannot
// un-revoke a token, so the job must not fail over one — the owner is told
// through the connection's status, the same way the sync worker tells them.
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

// TestSavingAMappingWithoutACurrencyKeepsTheOneItHas. A resolution that hit the
// map has no passport in hand and passes the empty string; the row it refreshes
// has a currency already, learned by the call that created it. Blanking it
// would cost that listing its price until a passport happened to be fetched
// again — silently, since an unpriced listing looks exactly like one the broker
// has no price for.
func TestSavingAMappingWithoutACurrencyKeepsTheOneItHas(t *testing.T) {
	f := newQuotesFixture(t)
	inst := f.instrument(t, "KEEP", "RUB")
	f.mapTo(t, "uid-keep", inst.ID, "USD")

	// The same listing resolved again, this time from the map: no currency to
	// offer, and something else about the row genuinely changed so the upsert
	// does write.
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

// TestCurrencyTradesAreCountedPerLinkAndByReason. The count that explains a
// cash difference has to be about ONE broker account and about ONE reason:
// captioning an account's money gap with another account's unimported trades,
// or with rows unparsed for some unrelated cause, would name a reason that is
// not the true one.
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

// markUnparsed writes one mirror row carrying a given reason. Straight to the
// table: what is under test is the COUNT's grouping, and driving a real
// projection to produce each reason would make the fixture about something
// else.
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

// -------------------------------------------------------------------------
// pricing a holding nobody imported (#137)
// -------------------------------------------------------------------------

func listing(uid, isin, ticker, class, currency, kind string) Listing {
	return Listing{UID: uid, ISIN: isin, Ticker: ticker, ClassCode: class, Currency: currency, Kind: kind}
}

// TestCandidateListingsRefuseEverythingButTheSamePaper. The broker answers a
// search with whatever matches, and three of the four filters below are the
// difference between a price and a stranger's price.
func TestCandidateListingsRefuseEverythingButTheSamePaper(t *testing.T) {
	want := UnmappedHeldInstrument{ISIN: "US0378331005", Ticker: "AAPL", Type: "share", Currency: "USD"}
	found := []Listing{
		listing("uid-spb", "US0378331005", "AAPL", "SPBXM", "USD", "INSTRUMENT_TYPE_SHARE"),
		listing("uid-a25", "US0378331005", "AAPL", "A25", "usd", "INSTRUMENT_TYPE_SHARE"),
		// Same paper, the old ruble line: a price in the wrong currency filed
		// under this row would be wrong by a factor of eighty.
		listing("uid-rm", "US0378331005", "AAPL-RM", "FQBR", "RUB", "INSTRUMENT_TYPE_SHARE"),
		// SOMEBODY ELSE'S PAPER UNDER THE SAME TICKER, and identical to the
		// wanted one in every other respect — same kind, same currency. Only
		// the ISIN tells them apart, which is what makes this row the one that
		// says the match is on the ISIN and not on the ticker. The broker
		// really does answer "T" with two issuers.
		listing("uid-other", "RU000A107UL4", "AAPL", "TQBR", "USD", "INSTRUMENT_TYPE_SHARE"),
		// THE SAME PAPER UNDER ANOTHER TICKER, in the wanted currency: kept,
		// which ticker matching would not do. The frozen foreign lines carry a
		// "-RM" name of their own, and a rule that dropped them would leave
		// exactly the holdings this pass exists for unpriced.
		listing("uid-rm-usd", "US0378331005", "AAPL-RM", "MTQR", "USD", "INSTRUMENT_TYPE_SHARE"),
		// The right ISIN and currency, the wrong kind of asset: a bond's quote
		// is a percent of par and would be read here as money per share.
		listing("uid-bond", "US0378331005", "AAPL", "TQCB", "USD", "INSTRUMENT_TYPE_BOND"),
	}

	got := candidateListings(want, found)
	kept := map[string]bool{}
	for _, l := range got {
		kept[l.UID] = true
	}
	want3 := []string{"uid-spb", "uid-a25", "uid-rm-usd"}
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

// TestPickListingTakesTheOneStillBeingQuoted. Apple answers with lines quoted
// this week and lines frozen since trading in them stopped in 2022. The freshest
// price is the rule, and it needs no hand-maintained list of venue names.
func TestPickListingTakesTheOneStillBeingQuoted(t *testing.T) {
	live := listing("uid-live", "US0378331005", "AAPL", "SPBXM", "USD", "INSTRUMENT_TYPE_SHARE")
	frozen := listing("uid-frozen", "US0378331005", "AAPL-RM", "FQBR", "USD", "INSTRUMENT_TYPE_SHARE")
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

// TestPickListingRefusesTwoEqallyFreshPricesThatDisagree is the trap this
// whole selection exists around: with nothing to choose between two venues,
// picking one puts its price on the holding and nothing on any screen says
// which venue it came from.
func TestPickListingRefusesTwoEqallyFreshPricesThatDisagree(t *testing.T) {
	at := time.Date(2026, 8, 7, 23, 59, 0, 0, time.UTC)
	a := listing("uid-a", "US0378331005", "AAPL", "SPBXM", "USD", "INSTRUMENT_TYPE_SHARE")
	b := listing("uid-b", "US0378331005", "AAPL", "A25", "USD", "INSTRUMENT_TYPE_SHARE")

	disagree := map[string]LastPrice{
		"uid-a": {InstrumentUID: "uid-a", Price: decimal.RequireFromString("313.25"), At: at},
		"uid-b": {InstrumentUID: "uid-b", Price: decimal.RequireFromString("311.00"), At: at},
	}
	if _, _, ok := pickListing([]Listing{a, b}, disagree); ok {
		t.Error("picked one of two same-day prices that disagree, want a refusal")
	}

	// The same tie with the same price is not a choice at all — it is one fact
	// reported twice, and refusing it would leave a holding unpriced for no
	// reason.
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
	a := listing("uid-a", "US0378331005", "AAPL", "SPBXM", "USD", "INSTRUMENT_TYPE_SHARE")
	if _, _, ok := pickListing([]Listing{a}, map[string]LastPrice{}); ok {
		t.Error("picked a listing the broker quoted no price for")
	}
	zero := map[string]LastPrice{"uid-a": {InstrumentUID: "uid-a", At: time.Now()}}
	if _, _, ok := pickListing([]Listing{a}, zero); ok {
		t.Error("picked a listing whose price is nought, which is no price")
	}
}
