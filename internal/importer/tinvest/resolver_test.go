package tinvest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata/moex"
	"babki.my/babki/internal/platform/money"
)

// countingCatalog wraps the real *instrument.Store with call counters. Not an
// in-memory fake: tinvest_instrument_map has a foreign key to instruments, so the
// map writes need real rows. failByISIN and failUpdate replace one call's answer
// with an error, to reach a catalog that is down.
type countingCatalog struct {
	*instrument.Store
	createCalls, updateCalls int
	failByISIN, failUpdate   error
}

func (c *countingCatalog) ByISIN(ctx context.Context, isin string) (instrument.Instrument, error) {
	if c.failByISIN != nil {
		return instrument.Instrument{}, c.failByISIN
	}
	return c.Store.ByISIN(ctx, isin)
}

func (c *countingCatalog) Create(ctx context.Context, inst instrument.Instrument) (instrument.Instrument, error) {
	c.createCalls++
	return c.Store.Create(ctx, inst)
}

func (c *countingCatalog) Update(ctx context.Context, id uuid.UUID, upd instrument.Update) (instrument.Instrument, error) {
	c.updateCalls++
	if c.failUpdate != nil {
		return instrument.Instrument{}, c.failUpdate
	}
	return c.Store.Update(ctx, id, upd)
}

// secondConnection adds another connection to the same space (a second
// T-Invest login), for what the Resolver carries across connections.
func (f fixture) secondConnection(t *testing.T) Connection {
	t.Helper()
	conn, err := f.store.CreateConnection(f.ctx, f.spaceID, []byte("nonce||ciphertext-2"), "7b1e", StatusActive)
	if err != nil {
		t.Fatalf("CreateConnection (second): %v", err)
	}
	return conn
}

// raceCatalog simulates createInstrument losing a race: the first Create for
// raceOnTicker inserts racedWinner through the real store, so the resolver's own
// insert hits a real unique violation. A winner with an ISIN collides on the isin
// index (ErrISINTaken, the same paper); one without, on the ticker index
// (ErrTickerTaken, maybe anybody).
type raceCatalog struct {
	*countingCatalog
	raceOnTicker string
	racedWinner  instrument.Instrument
}

func (c *raceCatalog) Create(ctx context.Context, inst instrument.Instrument) (instrument.Instrument, error) {
	if c.raceOnTicker != "" && inst.Ticker == c.raceOnTicker {
		c.raceOnTicker = ""
		// Straight to the embedded store, so the setup is not counted as the
		// resolver's call.
		if _, err := c.Store.Create(ctx, c.racedWinner); err != nil {
			return instrument.Instrument{}, fmt.Errorf("raceCatalog: seed the winning row: %w", err)
		}
	}
	return c.countingCatalog.Create(ctx, inst)
}

// fakePassportSource is an in-memory *Client stand-in counting calls per
// method. instrumentErrs makes one uid fail with a chosen kind of failure ("no
// such instrument" and "unreachable" are acted on differently).
type fakePassportSource struct {
	instruments      map[string]InstrumentBrief
	nominals         map[string]MoneyValue
	instrumentErrs   map[string]error
	instrumentCalls  map[string]int
	bondNominalCalls map[string]int

	currencyNominals     map[string]MoneyValue
	currencyNominalCalls map[string]int
}

func newFakePassportSource() *fakePassportSource {
	return &fakePassportSource{
		instruments:      map[string]InstrumentBrief{},
		nominals:         map[string]MoneyValue{},
		instrumentErrs:   map[string]error{},
		instrumentCalls:  map[string]int{},
		bondNominalCalls: map[string]int{},

		currencyNominals:     map[string]MoneyValue{},
		currencyNominalCalls: map[string]int{},
	}
}

// Nothing registered by default, so a test gets "no such instrument",
// never a silent zero nominal.
func (s *fakePassportSource) CurrencyNominalByUID(_ context.Context, uid string) (MoneyValue, error) {
	s.currencyNominalCalls[uid]++
	nominal, ok := s.currencyNominals[uid]
	if !ok {
		return MoneyValue{}, fmt.Errorf("%w: %s", ErrInstrumentNotFound, uid)
	}
	return nominal, nil
}

func (s *fakePassportSource) InstrumentByUID(_ context.Context, uid string) (InstrumentBrief, error) {
	s.instrumentCalls[uid]++
	if err, ok := s.instrumentErrs[uid]; ok {
		return InstrumentBrief{}, err
	}
	brief, ok := s.instruments[uid]
	if !ok {
		return InstrumentBrief{}, fmt.Errorf("fakePassportSource: no instrument for uid %q", uid)
	}
	return brief, nil
}

func (s *fakePassportSource) BondNominalByUID(_ context.Context, uid string) (MoneyValue, error) {
	s.bondNominalCalls[uid]++
	nominal, ok := s.nominals[uid]
	if !ok {
		return MoneyValue{}, fmt.Errorf("fakePassportSource: no nominal for uid %q", uid)
	}
	return nominal, nil
}

// Map hits: no catalog write, no broker.

// A connection that already resolved this instrument_uid answers from the
// map alone.
func TestResolve_MapHitByInstrumentUID(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	inst, err := catalog.Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER",
		ISIN: "RU0009029540", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
	if err := f.store.saveMap(f.ctx, f.conn.ID, inst.ID,
		InstrumentRef{InstrumentUID: "uid-sber", FIGI: "BBG004730N88"}, inst.ISIN, inst.Ticker, "RUB"); err != nil {
		t.Fatalf("seed map: %v", err)
	}

	src := newFakePassportSource() // deliberately empty: any lookup fails the test
	r := NewResolver(f.store, catalog, nil)
	got, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: "uid-sber", FIGI: "BBG004730N88"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.InstrumentID != inst.ID || got.Type != instrument.TypeShare {
		t.Errorf("Resolve = %+v, want {%v %v}", got, inst.ID, instrument.TypeShare)
	}
	if len(src.instrumentCalls) != 0 {
		t.Errorf("InstrumentByUID called %v, want zero calls on a map hit", src.instrumentCalls)
	}
	if catalog.createCalls != 1 { // only the seeding Create above
		t.Errorf("catalog.Create called %d times, want 1 (the seed only)", catalog.createCalls)
	}
}

// The figi fallback for an unmapped instrument_uid, and the hit is then
// recorded under the new uid too: resolving it again with the broker failing
// succeeds only through a uid hit.
func TestResolve_MapHitByFIGI(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	inst, err := catalog.Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeBond, Name: "ОФЗ 26238", Ticker: "SU26238RMFS4",
		ISIN: "RU000A1038V6", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
	if err := f.store.saveMap(f.ctx, f.conn.ID, inst.ID,
		InstrumentRef{InstrumentUID: "uid-old", FIGI: "FIGI-STABLE"}, inst.ISIN, inst.Ticker, "RUB"); err != nil {
		t.Fatalf("seed map: %v", err)
	}

	src := newFakePassportSource()
	r := NewResolver(f.store, catalog, nil)
	ref := InstrumentRef{InstrumentUID: "uid-new", FIGI: "FIGI-STABLE"}
	got, err := r.Resolve(f.ctx, f.conn.ID, src, ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.InstrumentID != inst.ID || got.Type != instrument.TypeBond {
		t.Errorf("Resolve = %+v, want {%v %v}", got, inst.ID, instrument.TypeBond)
	}
	if len(src.instrumentCalls) != 0 {
		t.Errorf("InstrumentByUID called %v, want zero calls on a figi hit", src.instrumentCalls)
	}

	// The drifted uid must now be its own hit — a second Resolve for it
	// alone (figi withheld this time) can only succeed via mapByInstrumentUID.
	got2, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: "uid-new"})
	if err != nil {
		t.Fatalf("Resolve(uid-new alone): %v", err)
	}
	if got2.InstrumentID != inst.ID {
		t.Errorf("Resolve(uid-new alone) = %+v, want instrument %v — the figi hit must have saved uid-new to the map", got2, inst.ID)
	}
}

// When uid and figi would match different rows, the uid wins.
func TestResolve_MapLookupPrefersInstrumentUIDOverFIGI(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	instA, err := catalog.Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "A", Ticker: "AAAA", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("seed instrument A: %v", err)
	}
	instB, err := catalog.Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeBond, Name: "B", Ticker: "BBBB", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("seed instrument B: %v", err)
	}
	if err := f.store.saveMap(f.ctx, f.conn.ID, instA.ID,
		InstrumentRef{InstrumentUID: "uid-a", FIGI: "FIGI-A"}, "", "AAAA", "RUB"); err != nil {
		t.Fatalf("seed map A: %v", err)
	}
	if err := f.store.saveMap(f.ctx, f.conn.ID, instB.ID,
		InstrumentRef{InstrumentUID: "uid-target", FIGI: "FIGI-B"}, "", "BBBB", "RUB"); err != nil {
		t.Fatalf("seed map B: %v", err)
	}

	// This ref's instrument_uid names B's row; its figi names A's. If figi
	// were consulted first (or instead), this would resolve to A.
	src := newFakePassportSource()
	r := NewResolver(f.store, catalog, nil)
	got, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: "uid-target", FIGI: "FIGI-A"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.InstrumentID != instB.ID {
		t.Errorf("Resolve = %+v, want instrument B (%v) — instrument_uid must be tried before figi", got, instB.ID)
	}
}

// A hit rewrites the row with what this call sees, so a figi that drifted
// under a stable uid is captured.
func TestResolve_MapHitRefreshesDriftedAttributes(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	inst, err := catalog.Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Газпром", Ticker: "GAZP", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
	if err := f.store.saveMap(f.ctx, f.conn.ID, inst.ID,
		InstrumentRef{InstrumentUID: "uid-gazp", FIGI: "FIGI-OLD"}, "", "GAZP", "RUB"); err != nil {
		t.Fatalf("seed map: %v", err)
	}

	src := newFakePassportSource()
	r := NewResolver(f.store, catalog, nil)
	if _, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: "uid-gazp", FIGI: "FIGI-NEW"}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	var figi string
	err = f.pool.QueryRow(f.ctx,
		`SELECT figi FROM tinvest_instrument_map WHERE connection_id = $1 AND instrument_uid = $2`,
		f.conn.ID, "uid-gazp").Scan(&figi)
	if err != nil {
		t.Fatalf("read map row: %v", err)
	}
	if figi != "FIGI-NEW" {
		t.Errorf("map row figi = %q, want %q — a hit must refresh it, not freeze the first observation", figi, "FIGI-NEW")
	}
}

// Broker and catalog paths.

// A forgotten paper (passport 404 for good) is found by the ISIN the broker
// puts in the operation's ticker field ("RU000A101X68", a FinEx fund on the
// owner's account).
func TestResolve_ForgottenPaperIsFoundByTheIsinTheOperationCarries(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	src := newFakePassportSource()
	src.instrumentErrs["uid-gone"] = fmt.Errorf("%w: uid-gone", ErrInstrumentNotFound)

	existing, err := catalog.Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeETF, Name: "Технологии Америки",
		Ticker: "TECH", ISIN: "RU000A101X68", FIGI: "TCS20A101X68", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("seed the catalog: %v", err)
	}

	r := NewResolver(f.store, catalog, nil)
	got, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{
		InstrumentUID: "uid-gone",
		// The operation's figi differs from the catalog's (reissued per listing),
		// so a figi match cannot be what found it.
		FIGI:   "TCS33A101X68",
		Ticker: "RU000A101X68",
	})
	if err != nil {
		t.Fatalf("Resolve(a paper the broker forgot) = %v, want the catalog row it plainly is", err)
	}
	if got.InstrumentID != existing.ID {
		t.Errorf("resolved to %s, want the existing row %s", got.InstrumentID, existing.ID)
	}
	if catalog.createCalls != 1 {
		t.Errorf("catalog.Create called %d times, want only the seeding one — nothing about such a paper may be invented, its currency least of all", catalog.createCalls)
	}
}

// A forgotten paper the catalog does not know stays unresolved: nothing
// about it, its currency least of all, can be invented from an operation.
func TestResolve_ForgottenPaperTheCatalogDoesNotKnowStaysUnresolved(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	src := newFakePassportSource()
	src.instrumentErrs["uid-gone"] = fmt.Errorf("%w: uid-gone", ErrInstrumentNotFound)

	r := NewResolver(f.store, catalog, nil)
	_, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{
		InstrumentUID: "uid-gone", Ticker: "US87238U2033",
	})
	if !errors.Is(err, ErrInstrumentNotFound) {
		t.Fatalf("Resolve = %v, want the broker's own ErrInstrumentNotFound", err)
	}
	if catalog.createCalls != 0 {
		t.Errorf("catalog.Create called %d times, want 0", catalog.createCalls)
	}
}

// fakeExchange is the exchange's reference: what it remembers, by ISIN, and
// how often it was asked.
type fakeExchange struct {
	papers map[string]moex.Security
	err    error
	asked  int
}

func (e *fakeExchange) RememberedByISIN(_ context.Context, isin string) (moex.Security, bool, error) {
	e.asked++
	if e.err != nil {
		return moex.Security{}, false, e.err
	}
	sec, ok := e.papers[isin]
	return sec, ok, nil
}

// A forgotten paper the catalog does not know is created from what the
// exchange remembers (decision Р-19): its full name, kind and ISIN, in the
// currency the operation was paid in. The TCS receipt on the owner's history,
// replaced by Т-Технологии shares in 2024.
func TestResolve_ForgottenPaperIsCreatedFromTheExchangesReference(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	src := newFakePassportSource()
	src.instrumentErrs["uid-gone"] = fmt.Errorf("%w: uid-gone", ErrInstrumentNotFound)
	exchange := &fakeExchange{papers: map[string]moex.Security{
		"US87238U2033": {SecID: "TCS-ME", ISIN: "US87238U2033", Name: "ГДР TCS Group Holding ORD SHS", Kind: "share"},
	}}

	r := NewResolver(f.store, catalog, nil).WithExchange(exchange)
	ref := InstrumentRef{InstrumentUID: "uid-gone", Ticker: "US87238U2033", Currency: "rub"}
	got, err := r.Resolve(f.ctx, f.conn.ID, src, ref)
	if err != nil {
		t.Fatalf("Resolve(a forgotten receipt the exchange remembers) = %v", err)
	}
	inst, err := catalog.ByISIN(f.ctx, "US87238U2033")
	if err != nil {
		t.Fatalf("no catalog row for the receipt: %v", err)
	}
	if got.InstrumentID != inst.ID || inst.Type != instrument.TypeShare || inst.Name != "ГДР TCS Group Holding ORD SHS" ||
		inst.Ticker != "TCS-ME" || inst.Currency != "RUB" {
		t.Errorf("created %+v (resolved %+v), want the exchange's receipt in roubles", inst, got)
	}

	// The next operation on it finds the row; the exchange is not asked again.
	if _, err := NewResolver(f.store, catalog, nil).WithExchange(exchange).Resolve(f.ctx, f.conn.ID, src, ref); err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if exchange.asked != 1 || catalog.createCalls != 1 {
		t.Errorf("exchange asked %d times, catalog created %d rows; want 1 and 1", exchange.asked, catalog.createCalls)
	}
}

// What the exchange cannot describe stays unresolved, as before Р-19: a paper
// it does not know, an operation with no currency, an exchange that did not
// answer (asked once a run, not once per operation).
func TestResolve_ForgottenPaperTheExchangeCannotDescribeStaysUnresolved(t *testing.T) {
	for name, c := range map[string]struct {
		exchange *fakeExchange
		currency string
	}{
		"unknown to the exchange": {exchange: &fakeExchange{}, currency: "rub"},
		"no currency":             {exchange: &fakeExchange{papers: map[string]moex.Security{"US87238U2033": {ISIN: "US87238U2033", Kind: "share"}}}},
		"exchange did not answer": {exchange: &fakeExchange{err: errors.New("moex: unexpected status 502")}, currency: "rub"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
			src := newFakePassportSource()
			src.instrumentErrs["uid-gone"] = fmt.Errorf("%w: uid-gone", ErrInstrumentNotFound)
			r := NewResolver(f.store, catalog, nil).WithExchange(c.exchange)

			for range 2 {
				_, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{
					InstrumentUID: "uid-gone", Ticker: "US87238U2033", Currency: c.currency,
				})
				if !errors.Is(err, ErrInstrumentNotFound) {
					t.Fatalf("Resolve = %v, want the broker's own ErrInstrumentNotFound", err)
				}
			}
			if catalog.createCalls != 0 || c.exchange.asked > 1 {
				t.Errorf("catalog created %d rows, exchange asked %d times; want 0 and at most 1", catalog.createCalls, c.exchange.asked)
			}
		})
	}
}

// A forgotten paper with a plain ticker finds nothing and does not fall
// through to a ticker search ("T" is AT&T in one catalog, Т-Технологии in
// another).
func TestResolve_AnOrdinaryTickerIsNotTreatedAsAnIsin(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	src := newFakePassportSource()
	src.instrumentErrs["uid-gone"] = fmt.Errorf("%w: uid-gone", ErrInstrumentNotFound)

	if _, err := catalog.Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "AT&T", Ticker: "T",
		ISIN: "US00206R1023", Currency: "USD",
	}); err != nil {
		t.Fatalf("seed the catalog: %v", err)
	}

	r := NewResolver(f.store, catalog, nil)
	_, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{
		InstrumentUID: "uid-gone", Ticker: "T",
	})
	if !errors.Is(err, ErrInstrumentNotFound) {
		t.Fatalf("Resolve = %v, want ErrInstrumentNotFound — «T» is a ticker, and the paper behind it here is a different company entirely", err)
	}
}

// A futures instrument refuses with ErrUnsupportedInstrumentType before any
// catalog call. "futures" is the API's spelling; any unlisted string refuses the
// same way, but the case should run as it arrives.
func TestResolve_UnsupportedInstrumentType(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	src := newFakePassportSource()
	src.instruments["uid-futures"] = InstrumentBrief{
		UID: "uid-futures", Ticker: "SiZ6", Name: "Фьючерс на USD/RUB",
		Currency: "RUB", InstrumentType: "futures",
	}

	r := NewResolver(f.store, catalog, nil)
	_, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: "uid-futures"})
	if !errors.Is(err, ErrUnsupportedInstrumentType) {
		t.Fatalf("Resolve(futures) err = %v, want ErrUnsupportedInstrumentType", err)
	}
	if catalog.createCalls != 0 {
		t.Errorf("catalog.Create called %d times, want 0 — an unsupported type must never reach the catalog", catalog.createCalls)
	}
}

// "currency" is refused, not created. A currency operation becomes a
// conversion that names no instrument; if the resolver is called anyway,
// creation is the only outcome and the unique ticker index does not cover
// currencies (migration 0011): two connections left two rows when this was
// tried.
func TestResolve_CurrencyIsRefusedRatherThanCreated(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	src := newFakePassportSource()
	src.instruments["uid-usd"] = InstrumentBrief{
		UID: "uid-usd", FIGI: "BBG0013HGFT4", Ticker: "USD000UTSTOM",
		Name: "Доллар США", Currency: "RUB", InstrumentType: "currency",
	}

	r := NewResolver(f.store, catalog, nil)
	_, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: "uid-usd", FIGI: "BBG0013HGFT4"})
	if !errors.Is(err, ErrUnsupportedInstrumentType) {
		t.Fatalf("Resolve(currency) err = %v, want ErrUnsupportedInstrumentType", err)
	}
	if catalog.createCalls != 0 {
		t.Errorf("catalog.Create called %d times, want 0 — a currency must never become a catalog row", catalog.createCalls)
	}
}

// A catalog row found by ISIN (entered by hand, or by another connection).
// The seeded row has no ticker, so only the ISIN step can find it; with a ticker
// the ticker step would mask a missing ISIN lookup.
func TestResolve_CatalogHitByISIN(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	inst, err := catalog.Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Сбербанк",
		ISIN: "RU0009029540", FIGI: "BBG004730N88", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("seed catalog: %v", err)
	}

	src := newFakePassportSource()
	src.instruments["uid-sber"] = InstrumentBrief{
		UID: "uid-sber", FIGI: "BBG004730N88", ISIN: "RU0009029540",
		Ticker: "SBER", Name: "Сбер Банк", Currency: "RUB", InstrumentType: "share",
	}

	r := NewResolver(f.store, catalog, nil)
	got, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: "uid-sber", FIGI: "BBG004730N88"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.InstrumentID != inst.ID || got.Type != instrument.TypeShare {
		t.Errorf("Resolve = %+v, want {%v %v}", got, inst.ID, instrument.TypeShare)
	}
	if catalog.createCalls != 1 { // the seed only
		t.Errorf("catalog.Create called %d times, want 1 (the seed only) — an ISIN hit must not create a second row", catalog.createCalls)
	}
}

// A row found only by ticker gets its ISIN and FIGI filled from the
// passport, so the next exact lookup hits.
func TestResolve_CatalogHitByTicker_BackfillsISINAndFIGI(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	inst, err := catalog.Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Лукойл", Ticker: "LKOH", Currency: "RUB",
		// ISIN and FIGI deliberately blank: entered by hand before either was known.
	})
	if err != nil {
		t.Fatalf("seed catalog: %v", err)
	}

	src := newFakePassportSource()
	src.instruments["uid-lkoh"] = InstrumentBrief{
		UID: "uid-lkoh", FIGI: "BBG004731032", ISIN: "RU0009024277",
		Ticker: "LKOH", Name: "Лукойл", Currency: "RUB", InstrumentType: "share",
	}

	r := NewResolver(f.store, catalog, nil)
	got, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: "uid-lkoh", FIGI: "BBG004731032"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.InstrumentID != inst.ID {
		t.Fatalf("Resolve = %+v, want instrument %v", got, inst.ID)
	}
	backfilled, err := catalog.ByID(f.ctx, inst.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if backfilled.ISIN != "RU0009024277" || backfilled.FIGI != "BBG004731032" {
		t.Errorf("catalog row after Resolve = %+v, want isin=RU0009024277 figi=BBG004731032", backfilled)
	}
	if catalog.updateCalls != 1 {
		t.Errorf("catalog.Update called %d times, want 1", catalog.updateCalls)
	}

	// A row with both identifiers is not touched again. The second ref has a
	// new figi to skip the map, and its passport has no ISIN: a different ISIN
	// is refused as another security, the same one is caught by the ISIN step.
	src.instruments["uid-lkoh-2"] = InstrumentBrief{
		UID: "uid-lkoh-2", FIGI: "BBG-UNRELATED",
		Ticker: "LKOH", Name: "Лукойл", Currency: "RUB", InstrumentType: "share",
	}
	if _, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: "uid-lkoh-2", FIGI: "BBG-UNRELATED"}); err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if catalog.updateCalls != 1 {
		t.Errorf("catalog.Update called %d times after a row already whole was found again, want still 1", catalog.updateCalls)
	}
}

// The ticker lookup reaches bonds and funds as well as shares. If it
// covered shares only, existing bonds and funds would not duplicate (the unique
// ticker index covers them) but would fail to resolve with a misleading "lost
// the ticker race" error.
func TestResolve_CatalogHitByTicker_CoversBondsAndFunds(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	// Both seeded WITHOUT isin/figi, so the ISIN step cannot be what finds
	// them and the ticker step is the only route to either row.
	bond, err := catalog.Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeBond, Name: "ОФЗ 26238", Ticker: "SU26238RMFS4", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("seed bond: %v", err)
	}
	fund, err := catalog.Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeETF, Name: "Т-Капитал Индекс МосБиржи", Ticker: "TMOS", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("seed fund: %v", err)
	}

	src := newFakePassportSource()
	src.instruments["uid-ofz"] = InstrumentBrief{
		UID: "uid-ofz", FIGI: "BBG012XT1M09", ISIN: "RU000A1038V6",
		Ticker: "SU26238RMFS4", Name: "ОФЗ 26238", Currency: "RUB", InstrumentType: "bond",
	}
	src.instruments["uid-tmos"] = InstrumentBrief{
		UID: "uid-tmos", FIGI: "BBG333333333", ISIN: "RU000A101X76",
		Ticker: "TMOS", Name: "Т-Капитал Индекс МосБиржи", Currency: "RUB", InstrumentType: "etf",
	}
	// No bond nominal is registered, so reaching creation would also fail.

	r := NewResolver(f.store, catalog, nil)
	gotBond, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: "uid-ofz", FIGI: "BBG012XT1M09"})
	if err != nil {
		t.Fatalf("Resolve(bond): %v", err)
	}
	if gotBond.InstrumentID != bond.ID || gotBond.Type != instrument.TypeBond {
		t.Errorf("Resolve(bond) = %+v, want the seeded bond %v", gotBond, bond.ID)
	}
	gotFund, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: "uid-tmos", FIGI: "BBG333333333"})
	if err != nil {
		t.Fatalf("Resolve(etf): %v", err)
	}
	if gotFund.InstrumentID != fund.ID || gotFund.Type != instrument.TypeETF {
		t.Errorf("Resolve(etf) = %+v, want the seeded fund %v", gotFund, fund.ID)
	}
	if catalog.createCalls != 2 { // the two seeds only
		t.Errorf("catalog.Create called %d times, want 2 (the seeds only) — a bond and a fund already in the catalog must be found, not duplicated", catalog.createCalls)
	}
}

// Plain creation of a share, never frozen: the broker has no sanctions
// field.
func TestResolve_CreatesShare(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	src := newFakePassportSource()
	src.instruments["uid-novatek"] = InstrumentBrief{
		UID: "uid-novatek", FIGI: "BBG00475KKY8", ISIN: "RU000A0DKVS5",
		Ticker: "NVTK", Name: "Новатэк", Currency: "RUB", InstrumentType: "share",
	}

	r := NewResolver(f.store, catalog, nil)
	got, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: "uid-novatek", FIGI: "BBG00475KKY8"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	created, err := catalog.ByID(f.ctx, got.InstrumentID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if created.Type != instrument.TypeShare || created.Name != "Новатэк" || created.Ticker != "NVTK" ||
		created.ISIN != "RU000A0DKVS5" || created.FIGI != "BBG00475KKY8" || created.Currency != "RUB" {
		t.Errorf("created = %+v, want the passport's own fields", created)
	}
	if created.Frozen {
		t.Error("created.Frozen = true, want false: the broker's API has no field for this, so it must never be inferred")
	}
	if created.FaceValueMinor != nil || created.FaceCurrency != nil {
		t.Errorf("created share carries a face value pair = %v/%v, want both nil", created.FaceValueMinor, created.FaceCurrency)
	}
}

// A bond's face value comes from a second call, BondNominalByUID: 1000 RUB
// -> 100000, the value checked live on a sandbox bond.
func TestResolve_CreatesBond_CarriesNominalAndCurrency(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	src := newFakePassportSource()
	src.instruments["uid-bond"] = InstrumentBrief{
		UID: "uid-bond", FIGI: "TCS00A106YF0", ISIN: "RU000A106YF0",
		Ticker: "RU000BOND1", Name: "Пример облигации", Currency: "RUB", InstrumentType: "bond",
	}
	src.nominals["uid-bond"] = MoneyValue{Currency: "RUB", Units: 1000, Nano: 0}

	r := NewResolver(f.store, catalog, nil)
	got, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: "uid-bond", FIGI: "TCS00A106YF0"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	created, err := catalog.ByID(f.ctx, got.InstrumentID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if created.Type != instrument.TypeBond {
		t.Fatalf("created.Type = %v, want bond", created.Type)
	}
	if created.FaceValueMinor == nil || *created.FaceValueMinor != 100000 {
		t.Errorf("created.FaceValueMinor = %v, want 100000 (1000 RUB)", created.FaceValueMinor)
	}
	if created.FaceCurrency == nil || *created.FaceCurrency != "RUB" {
		t.Errorf("created.FaceCurrency = %v, want RUB", created.FaceCurrency)
	}
	if src.bondNominalCalls["uid-bond"] != 1 {
		t.Errorf("BondNominalByUID called %d times, want exactly 1", src.bondNominalCalls["uid-bond"])
	}
}

// A ticker is not an identity.

// The owner's case: AT&T is in the catalog as "T"; Т-Технологии trade on
// MOEX as "T" (RU000A107UL4). It once stamped Т-Технологии's identifiers onto
// AT&T, then refused and left Т-Технологии unimportable. Since migration 0020
// it gets a row of its own, and AT&T's row is unchanged.
func TestResolve_TickerHitWithAContradictingISINGetsARowOfItsOwn(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	att, err := catalog.Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "AT&T", Ticker: "T",
		ISIN: "US00206R1023", Currency: "USD",
	})
	if err != nil {
		t.Fatalf("seed catalog: %v", err)
	}

	src := newFakePassportSource()
	src.instruments["uid-t-tech"] = InstrumentBrief{
		UID: "uid-t-tech", FIGI: "TCS10A107UL4", ISIN: "RU000A107UL4",
		Ticker: "T", Name: "Т-Технологии", Currency: "RUB", InstrumentType: "share",
	}

	r := NewResolver(f.store, catalog, nil)
	got, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: "uid-t-tech", FIGI: "TCS10A107UL4"})
	if err != nil {
		t.Fatalf("Resolve err = %v — the two are different companies and the catalog holds both", err)
	}
	if got.InstrumentID == att.ID {
		t.Fatal("Т-Технологии resolved to AT&T's row: the same ticker is not the same security")
	}
	created, err := catalog.ByID(f.ctx, got.InstrumentID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if created.ISIN != "RU000A107UL4" || created.Ticker != "T" || created.Name != "Т-Технологии" {
		t.Errorf("the new row = %+v, want Т-Технологии under ticker T with its own ISIN", created)
	}

	after, err := catalog.ByID(f.ctx, att.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if after.ISIN != "US00206R1023" || after.FIGI != "" || after.Name != "AT&T" {
		t.Errorf("AT&T's row afterwards = %+v, want it untouched (isin US00206R1023, no figi)", after)
	}
	if catalog.updateCalls != 0 {
		t.Errorf("catalog.Update called %d times, want 0 — a stranger's row is not written to, whatever is done instead", catalog.updateCalls)
	}
}

// The type half of the rule, on a row with no ISIN: valuation branches on
// type, so a different type is a different paper even under the same ticker.
func TestResolve_TickerHitOfAnotherTypeGetsARowOfItsOwn(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	fund, err := catalog.Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeETF, Name: "Фонд с тем же тикером", Ticker: "SAME1", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("seed catalog: %v", err)
	}

	src := newFakePassportSource()
	src.instruments["uid-bond-same-ticker"] = InstrumentBrief{
		UID: "uid-bond-same-ticker", FIGI: "BBG00SAME001", ISIN: "RU000A10SAME",
		Ticker: "SAME1", Name: "Облигация с тем же тикером", Currency: "RUB", InstrumentType: "bond",
	}
	src.nominals["uid-bond-same-ticker"] = MoneyValue{Currency: "RUB", Units: 1000}

	r := NewResolver(f.store, catalog, nil)
	got, err := r.Resolve(f.ctx, f.conn.ID, src,
		InstrumentRef{InstrumentUID: "uid-bond-same-ticker", FIGI: "BBG00SAME001"})
	if err != nil {
		t.Fatalf("Resolve err = %v — a bond and a fund under one ticker are two papers, and the catalog holds both", err)
	}
	if got.InstrumentID == fund.ID {
		t.Fatal("the bond resolved to the FUND's row: every valuation branches on the type, so its trades would be priced as a fund's")
	}
	if got.Type != instrument.TypeBond {
		t.Errorf("resolved type = %s, want bond", got.Type)
	}
	after, err := catalog.ByID(f.ctx, fund.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if after.ISIN != "" || after.FIGI != "" {
		t.Errorf("the fund's row afterwards = %+v, want no identifiers written onto it — they belong to the bond", after)
	}
	if catalog.updateCalls != 0 {
		t.Errorf("catalog.Update called %d times, want 0", catalog.updateCalls)
	}
}

// The re-lookup after losing a ticker race still refuses a stranger: the
// ticker index only holds ISIN-less rows now, so both sides lack an ISIN (a bond
// without one, and a fund someone just entered), and no second row is possible.
// The refusal names both.
func TestResolve_TickerRaceWinnerOfAnotherTypeIsRefused(t *testing.T) {
	f := newFixture(t)
	catalog := &raceCatalog{
		countingCatalog: &countingCatalog{Store: instrument.NewStore(f.pool)},
		raceOnTicker:    "SAME1",
		racedWinner: instrument.Instrument{
			Type: instrument.TypeETF, Name: "Фонд, созданный конкурентно",
			Ticker: "SAME1", Currency: "RUB",
		},
	}

	src := newFakePassportSource()
	src.instruments["uid-bond-no-isin"] = InstrumentBrief{
		UID: "uid-bond-no-isin", FIGI: "BBG00SAME002",
		Ticker: "SAME1", Name: "Облигация без ISIN", Currency: "RUB", InstrumentType: "bond",
	}
	src.nominals["uid-bond-no-isin"] = MoneyValue{Currency: "RUB", Units: 1000}

	r := NewResolver(f.store, catalog, nil)
	_, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: "uid-bond-no-isin", FIGI: "BBG00SAME002"})
	if !errors.Is(err, ErrDifferentSecurity) {
		t.Fatalf("Resolve err = %v, want ErrDifferentSecurity", err)
	}
	if catalog.updateCalls != 0 {
		t.Errorf("catalog.Update called %d times, want 0 — the row that won the race is a different security and must not be written to", catalog.updateCalls)
	}
}

// Losing a race on the ISIN: the winner is the same paper, so its row is
// taken.
func TestResolve_ISINRaceTakesTheWinnersRow(t *testing.T) {
	f := newFixture(t)
	catalog := &raceCatalog{
		countingCatalog: &countingCatalog{Store: instrument.NewStore(f.pool)},
		raceOnTicker:    "ROSN",
		racedWinner: instrument.Instrument{
			Type: instrument.TypeShare, Name: "Роснефть (создана конкурентно)",
			Ticker: "ROSN", ISIN: "RU000A0J2Q06", Currency: "RUB",
		},
	}

	src := newFakePassportSource()
	src.instruments["uid-rosn"] = InstrumentBrief{
		UID: "uid-rosn", FIGI: "BBG004731354", ISIN: "RU000A0J2Q06",
		Ticker: "ROSN", Name: "Роснефть", Currency: "RUB", InstrumentType: "share",
	}

	r := NewResolver(f.store, catalog, nil)
	got, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: "uid-rosn", FIGI: "BBG004731354"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	winner, err := catalog.ByID(f.ctx, got.InstrumentID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if winner.Name != "Роснефть (создана конкурентно)" {
		t.Fatalf("Resolve returned %+v, want the row the race left behind", winner)
	}
	if catalog.createCalls != 1 {
		t.Errorf("catalog.Create called %d times, want exactly 1 (the losing attempt) — a retry must not call Create again", catalog.createCalls)
	}
}

// Resolving one instrument twice for one connection costs one passport
// call; the map is what makes it pass (see the next test for the cache).
func TestResolve_RepeatedResolveMakesOnePassportCallTotal(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	src := newFakePassportSource()
	src.instruments["uid-sber"] = InstrumentBrief{
		UID: "uid-sber", FIGI: "BBG004730N88", ISIN: "RU0009029540",
		Ticker: "SBER", Name: "Сбер Банк", Currency: "RUB", InstrumentType: "share",
	}

	r := NewResolver(f.store, catalog, nil)
	ref := InstrumentRef{InstrumentUID: "uid-sber", FIGI: "BBG004730N88"}
	first, err := r.Resolve(f.ctx, f.conn.ID, src, ref)
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	second, err := r.Resolve(f.ctx, f.conn.ID, src, ref)
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if first != second {
		t.Errorf("first Resolve = %+v, second = %+v, want identical", first, second)
	}
	if src.instrumentCalls["uid-sber"] != 1 {
		t.Errorf("InstrumentByUID called %d times across two Resolve calls for the same ref, want exactly 1", src.instrumentCalls["uid-sber"])
	}
	if catalog.createCalls != 1 {
		t.Errorf("catalog.Create called %d times, want exactly 1 — the second Resolve must not create a duplicate row", catalog.createCalls)
	}
}

// The passport cache, reachable only through a second connection, whose
// map knows nothing yet. The cache is keyed by instrument, since a passport
// does not depend on whose token asked.
func TestResolve_PassportCacheServesASecondConnection(t *testing.T) {
	f := newFixture(t)
	second := f.secondConnection(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	src := newFakePassportSource()
	src.instruments["uid-sber"] = InstrumentBrief{
		UID: "uid-sber", FIGI: "BBG004730N88", ISIN: "RU0009029540",
		Ticker: "SBER", Name: "Сбер Банк", Currency: "RUB", InstrumentType: "share",
	}

	r := NewResolver(f.store, catalog, nil)
	ref := InstrumentRef{InstrumentUID: "uid-sber", FIGI: "BBG004730N88"}
	first, err := r.Resolve(f.ctx, f.conn.ID, src, ref)
	if err != nil {
		t.Fatalf("Resolve (first connection): %v", err)
	}
	fromSecond, err := r.Resolve(f.ctx, second.ID, src, ref)
	if err != nil {
		t.Fatalf("Resolve (second connection): %v", err)
	}
	if fromSecond != first {
		t.Errorf("second connection resolved to %+v, first to %+v — one paper is one catalog row", fromSecond, first)
	}
	if src.instrumentCalls["uid-sber"] != 1 {
		t.Errorf("InstrumentByUID called %d times across two connections resolving one instrument, want exactly 1 — the second must be served from the passport cache", src.instrumentCalls["uid-sber"])
	}
	if catalog.createCalls != 1 {
		t.Errorf("catalog.Create called %d times, want exactly 1 — the shared catalog holds one row per paper", catalog.createCalls)
	}
	// And the second connection got its own map row: the cache saves the
	// broker call, it does not stand in for the connection's own memory.
	if _, err := f.store.mapByInstrumentUID(f.ctx, second.ID, "uid-sber"); err != nil {
		t.Errorf("mapByInstrumentUID(second connection) = %v, want the row the second Resolve wrote", err)
	}
}

// A ref without instrument_uid is resolved but never saved: the map is
// unique per (connection, uid), and "" would let unrelated figi-only refs
// collide.
func TestResolve_EmptyInstrumentUIDIsNeverPersisted(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	src := newFakePassportSource()
	src.instruments[""] = InstrumentBrief{
		FIGI: "FIGI-NO-UID", ISIN: "RU0000000A01", Ticker: "NOUID",
		Name: "Без uid", Currency: "RUB", InstrumentType: "share",
	}

	r := NewResolver(f.store, catalog, nil)
	got, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{FIGI: "FIGI-NO-UID"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Type != instrument.TypeShare {
		t.Fatalf("Resolve = %+v, want a resolved share", got)
	}

	var count int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM tinvest_instrument_map WHERE connection_id = $1 AND instrument_uid = ''`,
		f.conn.ID).Scan(&count); err != nil {
		t.Fatalf("count map rows: %v", err)
	}
	if count != 0 {
		t.Errorf("tinvest_instrument_map has %d row(s) with an empty instrument_uid, want 0 — Resolve must never write under it", count)
	}
}

// What a passport must carry before a catalog row is made.

// No row from a passport without a name or currency: instrument.Store
// validates nothing, and the HTTP handler's rules do not apply to an importer.
func TestResolve_IncompletePassportCreatesNothing(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	src := newFakePassportSource()
	src.instruments["uid-nameless"] = InstrumentBrief{
		UID: "uid-nameless", FIGI: "BBG00NONAME0", ISIN: "RU000A10NAME",
		Ticker: "NONAME", Name: "", Currency: "RUB", InstrumentType: "share",
	}
	src.instruments["uid-no-currency"] = InstrumentBrief{
		UID: "uid-no-currency", FIGI: "BBG00NOCUR00", ISIN: "RU000A10NOCU",
		Ticker: "NOCUR", Name: "Без валюты", Currency: "", InstrumentType: "share",
	}
	src.instruments["uid-bad-currency"] = InstrumentBrief{
		UID: "uid-bad-currency", FIGI: "BBG00BADCUR0", ISIN: "RU000A10BADC",
		Ticker: "BADCUR", Name: "Валюта не кодом", Currency: "рубль", InstrumentType: "share",
	}

	r := NewResolver(f.store, catalog, nil)
	for _, uid := range []string{"uid-nameless", "uid-no-currency", "uid-bad-currency"} {
		_, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: uid})
		if !errors.Is(err, ErrIncompletePassport) {
			t.Errorf("Resolve(%s) err = %v, want ErrIncompletePassport", uid, err)
		}
	}
	if catalog.createCalls != 0 {
		t.Errorf("catalog.Create called %d times, want 0 — an incomplete passport must not reach the catalog", catalog.createCalls)
	}
	var count int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM instruments`).Scan(&count); err != nil {
		t.Fatalf("count instruments: %v", err)
	}
	if count != 0 {
		t.Errorf("the catalog holds %d row(s), want 0", count)
	}
}

// BondNominalByUID's failure modes. A zero or currency-less nominal would
// surface as a bare constraint violation mid-sync; one over money.MaxAmountMinor
// would insert and break the positions screen. Two fractional nominals because
// rounding fails differently: a tenth of a kopeck on top would vanish silently,
// below half a kopeck would round to zero. The call's own failure is pinned too.
func TestResolve_BondNominalRefusals(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	src := newFakePassportSource()
	bond := func(uid, ticker string) InstrumentBrief {
		return InstrumentBrief{
			UID: uid, FIGI: "BBG" + ticker, ISIN: "RU000" + ticker,
			Ticker: ticker, Name: "Облигация " + ticker, Currency: "RUB", InstrumentType: "bond",
		}
	}
	// No nominal registered at all: BondNominalByUID itself fails.
	src.instruments["uid-nominal-fails"] = bond("uid-nominal-fails", "BOND1")
	src.instruments["uid-nominal-zero"] = bond("uid-nominal-zero", "BOND2")
	src.nominals["uid-nominal-zero"] = MoneyValue{Currency: "RUB"}
	src.instruments["uid-nominal-no-currency"] = bond("uid-nominal-no-currency", "BOND3")
	src.nominals["uid-nominal-no-currency"] = MoneyValue{Units: 1000}
	src.instruments["uid-nominal-huge"] = bond("uid-nominal-huge", "BOND4")
	src.nominals["uid-nominal-huge"] = MoneyValue{Currency: "RUB", Units: money.MaxAmountMinor}
	// A tenth of a kopeck: representable on the wire, not in this program's
	// money. Rounded, it would be a kopeck of face value nobody reported.
	src.instruments["uid-nominal-fractional"] = bond("uid-nominal-fractional", "BOND5")
	src.nominals["uid-nominal-fractional"] = MoneyValue{Currency: "RUB", Units: 1000, Nano: 1_000_000}
	// Smaller than half a kopeck, which is the case rounding turns into a zero
	// face value and hands to the database as a raw constraint violation.
	src.instruments["uid-nominal-sliver"] = bond("uid-nominal-sliver", "BOND6")
	src.nominals["uid-nominal-sliver"] = MoneyValue{Currency: "RUB", Nano: 1_000_000}

	r := NewResolver(f.store, catalog, nil)

	if _, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: "uid-nominal-fails"}); err == nil {
		t.Error("Resolve(bond whose nominal call fails) err = nil, want the broker's failure reported")
	} else if !strings.Contains(err.Error(), "uid-nominal-fails") {
		t.Errorf("Resolve err = %v, want it to name the bond it could not price", err)
	}

	for _, uid := range []string{
		"uid-nominal-zero", "uid-nominal-no-currency", "uid-nominal-huge",
		"uid-nominal-fractional", "uid-nominal-sliver",
	} {
		_, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: uid})
		if !errors.Is(err, ErrIncompletePassport) {
			t.Errorf("Resolve(%s) err = %v, want ErrIncompletePassport", uid, err)
		}
	}

	var count int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM instruments`).Scan(&count); err != nil {
		t.Fatalf("count instruments: %v", err)
	}
	if count != 0 {
		t.Errorf("the catalog holds %d row(s), want 0 — no bond may be created without a nominal it can hold", count)
	}
}

// A catalog that is down is reported, not read as "no row" (which would
// create duplicates); and a failed backfill write reaches the caller.
func TestResolve_CatalogFailuresAreReported(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	src := newFakePassportSource()
	src.instruments["uid-sber"] = InstrumentBrief{
		UID: "uid-sber", FIGI: "BBG004730N88", ISIN: "RU0009029540",
		Ticker: "SBER", Name: "Сбер Банк", Currency: "RUB", InstrumentType: "share",
	}
	r := NewResolver(f.store, catalog, nil)

	down := errors.New("catalog is unreachable")
	catalog.failByISIN = down
	_, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: "uid-sber"})
	if !errors.Is(err, down) {
		t.Fatalf("Resolve with a failing ByISIN err = %v, want the catalog's own error", err)
	}
	if catalog.createCalls != 0 {
		t.Errorf("catalog.Create called %d times, want 0 — a lookup that failed says nothing about whether the row exists", catalog.createCalls)
	}
	catalog.failByISIN = nil

	// The backfill write on a ticker-found row with no ISIN.
	if _, err := catalog.Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Сбер Банк", Ticker: "SBER", Currency: "RUB",
	}); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
	catalog.failUpdate = down
	if _, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: "uid-sber"}); !errors.Is(err, down) {
		t.Fatalf("Resolve with a failing Update err = %v, want the catalog's own error", err)
	}
}

// Store-level guards.

// saveMap never erases a stored identifier with an empty one: the row
// accumulates what the connection learned. The figi matters most, being the drift
// fallback, so the assertion is that a figi lookup still finds the instrument.
// The case is an operation naming the paper by instrument_uid only.
func TestSaveMap_EmptyIdentifiersDoNotEraseStoredOnes(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	src := newFakePassportSource()
	src.instruments["uid-sber"] = InstrumentBrief{
		UID: "uid-sber", FIGI: "BBG004730N88", ISIN: "RU0009029540",
		Ticker: "SBER", Name: "Сбер Банк", Currency: "RUB", InstrumentType: "share",
	}

	r := NewResolver(f.store, catalog, nil)
	// A trade: it carries all four identifiers.
	trade := InstrumentRef{
		InstrumentUID: "uid-sber", FIGI: "BBG004730N88",
		PositionUID: "pos-sber", AssetUID: "asset-sber",
	}
	resolved, err := r.Resolve(f.ctx, f.conn.ID, src, trade)
	if err != nil {
		t.Fatalf("Resolve(trade): %v", err)
	}

	// The same paper named by instrument_uid and nothing else.
	if _, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: "uid-sber"}); err != nil {
		t.Fatalf("Resolve(dividend): %v", err)
	}

	var figi, positionUID, assetUID string
	err = f.pool.QueryRow(f.ctx, `
		SELECT figi, position_uid, asset_uid FROM tinvest_instrument_map
		WHERE connection_id = $1 AND instrument_uid = $2`,
		f.conn.ID, "uid-sber").Scan(&figi, &positionUID, &assetUID)
	if err != nil {
		t.Fatalf("read map row: %v", err)
	}
	if figi != "BBG004730N88" || positionUID != "pos-sber" || assetUID != "asset-sber" {
		t.Errorf("map row after an operation carrying none of them = figi %q, position_uid %q, asset_uid %q; want the trade's own, kept",
			figi, positionUID, assetUID)
	}

	m, err := f.store.mapByFIGI(f.ctx, f.conn.ID, "BBG004730N88")
	if err != nil {
		t.Fatalf("mapByFIGI = %v — the fallback lookup must survive an operation that carried no figi", err)
	}
	if m.InstrumentID != resolved.InstrumentID {
		t.Errorf("mapByFIGI = %v, want %v", m.InstrumentID, resolved.InstrumentID)
	}

	// And when the row is really being written: here the catalog's ticker
	// changes (Т-Технологии traded as TCSG before T) while the operation still
	// has no figi. Without this, assigning identifiers outright passed above.
	if err := f.store.saveMap(f.ctx, f.conn.ID, resolved.InstrumentID,
		InstrumentRef{InstrumentUID: "uid-sber"}, "RU0009029540", "SBERX", "RUB"); err != nil {
		t.Fatalf("saveMap with a renamed ticker: %v", err)
	}
	var ticker string
	err = f.pool.QueryRow(f.ctx, `
		SELECT figi, position_uid, asset_uid, ticker FROM tinvest_instrument_map
		WHERE connection_id = $1 AND instrument_uid = $2`,
		f.conn.ID, "uid-sber").Scan(&figi, &positionUID, &assetUID, &ticker)
	if err != nil {
		t.Fatalf("read map row: %v", err)
	}
	if ticker != "SBERX" {
		t.Errorf("map row ticker = %q, want SBERX — the write this case is about did not happen, so it proves nothing", ticker)
	}
	if figi != "BBG004730N88" || positionUID != "pos-sber" || assetUID != "asset-sber" {
		t.Errorf("map row after a write that carried none of them = figi %q, position_uid %q, asset_uid %q; want the trade's own, kept",
			figi, positionUID, assetUID)
	}
}

// A resolution that changes nothing writes nothing, witnessed by
// updated_at, which mapByFIGI also orders by.
func TestSaveMap_ResolutionThatChangesNothingWritesNothing(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	src := newFakePassportSource()
	src.instruments["uid-sber"] = InstrumentBrief{
		UID: "uid-sber", FIGI: "BBG004730N88", ISIN: "RU0009029540",
		Ticker: "SBER", Name: "Сбер Банк", Currency: "RUB", InstrumentType: "share",
	}

	r := NewResolver(f.store, catalog, nil)
	ref := InstrumentRef{InstrumentUID: "uid-sber", FIGI: "BBG004730N88", PositionUID: "pos-sber"}
	if _, err := r.Resolve(f.ctx, f.conn.ID, src, ref); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	readUpdatedAt := func() time.Time {
		t.Helper()
		var at time.Time
		if err := f.pool.QueryRow(f.ctx, `
			SELECT updated_at FROM tinvest_instrument_map
			WHERE connection_id = $1 AND instrument_uid = $2`,
			f.conn.ID, "uid-sber").Scan(&at); err != nil {
			t.Fatalf("read updated_at: %v", err)
		}
		return at
	}
	before := readUpdatedAt()

	if _, err := r.Resolve(f.ctx, f.conn.ID, src, ref); err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if after := readUpdatedAt(); !after.Equal(before) {
		t.Errorf("updated_at moved from %v to %v on a resolution that changed nothing, want it left alone", before, after)
	}

	// A real change still writes.
	if _, err := r.Resolve(f.ctx, f.conn.ID, src,
		InstrumentRef{InstrumentUID: "uid-sber", FIGI: "BBG004730N88", PositionUID: "pos-sber-new"}); err != nil {
		t.Fatalf("third Resolve: %v", err)
	}
	if after := readUpdatedAt(); !after.After(before) {
		t.Errorf("updated_at = %v after a drifted position_uid, want it moved past %v", after, before)
	}
}

// An empty identifier is never a match in either lookup.
func TestInstrumentMapLookups_EmptyIdentifierIsNeverAMatch(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	inst, err := catalog.Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Без идентификаторов", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
	// A row that legitimately carries empty figi (never observed for this
	// instrument) alongside a real instrument_uid.
	if err := f.store.saveMap(f.ctx, f.conn.ID, inst.ID,
		InstrumentRef{InstrumentUID: "uid-no-figi"}, "", "", "RUB"); err != nil {
		t.Fatalf("seed map: %v", err)
	}

	if _, err := f.store.mapByInstrumentUID(f.ctx, f.conn.ID, ""); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("mapByInstrumentUID(\"\") = %v, want pgx.ErrNoRows", err)
	}
	if _, err := f.store.mapByFIGI(f.ctx, f.conn.ID, ""); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("mapByFIGI(\"\") = %v, want pgx.ErrNoRows (must not match the row seeded with an empty figi)", err)
	}
}

// fakeRates answers official rates without a database; a missing code
// errors, as the table does before the backfill.
type fakeRates struct {
	// byCode is what the table holds.
	byCode map[string]decimal.Decimal
	err    error
	asks   int
}

func (f *fakeRates) Rate(_ context.Context, from, _ string, _ time.Time) (decimal.Decimal, time.Time, error) {
	f.asks++
	if f.err != nil {
		return decimal.Zero, time.Time{}, f.err
	}
	rate, ok := f.byCode[from]
	if !ok {
		return decimal.Zero, time.Time{}, fmt.Errorf("fakeRates: no rate for %s", from)
	}
	return rate, time.Time{}, nil
}

func ratesOf(pairs map[string]string) *fakeRates {
	byCode := make(map[string]decimal.Decimal, len(pairs))
	for code, rate := range pairs {
		byCode[code] = decimal.RequireFromString(rate)
	}
	return &fakeRates{byCode: byCode}
}

func currencyHint(ticker, price string) CurrencyHint {
	return CurrencyHint{
		Ticker:       ticker,
		Settlement:   "RUB",
		PricePerUnit: decimal.RequireFromString(price),
		On:           time.Date(2021, 2, 22, 0, 0, 0, 0, time.UTC),
	}
}

// A delisted pair's name gives the code (a guess); the trade price against
// the official rate proves it: 74.465 ₽ per unit beside 74.30 is a dollar.
func TestResolveCurrency_ForgottenPairIsWorkedOutFromItsNameAndProvedByTheRate(t *testing.T) {
	f := newFixture(t)
	src := newFakePassportSource() // registers no currency nominal: the broker 404s
	rates := ratesOf(map[string]string{"USD": "74.30"})

	r := NewResolver(f.store, &countingCatalog{Store: instrument.NewStore(f.pool)}, nil).WithRates(rates)
	got, err := r.ResolveCurrency(f.ctx, src, "uid-usd-gone", currencyHint("USD000UTSTOM", "74.465"))
	if err != nil {
		t.Fatalf("ResolveCurrency: %v", err)
	}
	if got.Code != "USD" {
		t.Errorf("code = %q, want USD", got.Code)
	}
	if !got.NominalPerUnit.Equal(decimal.NewFromInt(1)) {
		t.Errorf("nominal = %s, want 1 — that is what the agreement with the official rate PROVED", got.NominalPerUnit)
	}
}

// A pair quoted per hundred units is refused: 110 ₽ per unit against a som
// at 1.10 is a hundredfold off, a misread nominal.
func TestResolveCurrency_APairQuotedPerHundredUnitsIsRefused(t *testing.T) {
	f := newFixture(t)
	src := newFakePassportSource()
	rates := ratesOf(map[string]string{"KGS": "1.10"})

	r := NewResolver(f.store, &countingCatalog{Store: instrument.NewStore(f.pool)}, nil).WithRates(rates)
	_, err := r.ResolveCurrency(f.ctx, src, "uid-kgs-gone", currencyHint("KGSRUB_TOM", "110"))
	if !errors.Is(err, ErrInstrumentNotFound) {
		t.Fatalf("ResolveCurrency = %v, want the broker's own refusal to stand", err)
	}
}

// GLDRUB_TOM: "GLD" is not a currency, refused before any rate.
func TestResolveCurrency_ANameThatIsNotACurrencyIsRefused(t *testing.T) {
	f := newFixture(t)
	src := newFakePassportSource()
	// No source publishes GLD; that is what actually stops it.
	rates := ratesOf(map[string]string{"USD": "74.30"})

	r := NewResolver(f.store, &countingCatalog{Store: instrument.NewStore(f.pool)}, nil).WithRates(rates)
	_, err := r.ResolveCurrency(f.ctx, src, "uid-gold", currencyHint("GLDRUB_TOM", "8422.2"))
	if !errors.Is(err, ErrInstrumentNotFound) {
		t.Fatalf("ResolveCurrency = %v, want the broker's own refusal to stand", err)
	}
	if rates.asks != 1 {
		t.Errorf("the rate table was asked %d times, want once: three uppercase letters is all the shape check can say, and the table is what knows GLD is not money", rates.asks)
	}
}

// No official rate yet: the pair stays unparsed.
func TestResolveCurrency_NoOfficialRateLeavesThePairUnparsed(t *testing.T) {
	f := newFixture(t)
	src := newFakePassportSource()
	rates := &fakeRates{err: errors.New("no rate for that day")}

	r := NewResolver(f.store, &countingCatalog{Store: instrument.NewStore(f.pool)}, nil).WithRates(rates)
	_, err := r.ResolveCurrency(f.ctx, src, "uid-usd-gone", currencyHint("USD000UTSTOM", "74.465"))
	if !errors.Is(err, ErrInstrumentNotFound) {
		t.Fatalf("ResolveCurrency = %v, want the broker's own refusal to stand", err)
	}
}

// Where the broker can answer, its nominal wins and no rate is asked.
func TestResolveCurrency_TheBrokersOwnAnswerIsPreferred(t *testing.T) {
	f := newFixture(t)
	src := newFakePassportSource()
	src.currencyNominals["uid-kgs"] = MoneyValue{Currency: "kgs", Units: 100}
	rates := ratesOf(map[string]string{"KGS": "1.10"})

	r := NewResolver(f.store, &countingCatalog{Store: instrument.NewStore(f.pool)}, nil).WithRates(rates)
	got, err := r.ResolveCurrency(f.ctx, src, "uid-kgs", currencyHint("KGSRUB_TOM", "110"))
	if err != nil {
		t.Fatalf("ResolveCurrency: %v", err)
	}
	if got.Code != "KGS" || !got.NominalPerUnit.Equal(decimal.NewFromInt(100)) {
		t.Errorf("got %s per %s, want 100 per KGS — the broker said so itself", got.NominalPerUnit, got.Code)
	}
	if rates.asks != 0 {
		t.Errorf("the rate table was asked %d times though the broker answered", rates.asks)
	}
}

// A forgotten paper is asked about once a run, however many operations
// name it.
func TestResolve_TheBrokersRefusalIsAskedOnceARun(t *testing.T) {
	f := newFixture(t)
	src := newFakePassportSource()
	src.instrumentErrs["uid-gone"] = fmt.Errorf("%w: uid-gone", ErrInstrumentNotFound)

	r := NewResolver(f.store, &countingCatalog{Store: instrument.NewStore(f.pool)}, nil)
	ref := InstrumentRef{InstrumentUID: "uid-gone", FIGI: "TCS33A101X68", Ticker: "RU000A101X68"}
	for i := range 2 {
		if _, err := r.Resolve(f.ctx, f.conn.ID, src, ref); !errors.Is(err, ErrInstrumentNotFound) {
			t.Fatalf("Resolve #%d = %v, want the broker's refusal", i+1, err)
		}
	}
	if src.instrumentCalls["uid-gone"] != 1 {
		t.Errorf("InstrumentByUID called %d times, want once", src.instrumentCalls["uid-gone"])
	}
}

// Any other failure is not an answer about the paper, and the next operation
// asks again: the broker may answer this time.
func TestResolve_AFailureThatIsNotARefusalIsAskedAgain(t *testing.T) {
	f := newFixture(t)
	src := newFakePassportSource()
	src.instrumentErrs["uid-sber"] = errors.New("tinvest: InstrumentsService/GetInstrumentBy: request: dial tcp: connection refused")

	r := NewResolver(f.store, &countingCatalog{Store: instrument.NewStore(f.pool)}, nil)
	ref := InstrumentRef{InstrumentUID: "uid-sber"}
	for range 2 {
		if _, err := r.Resolve(f.ctx, f.conn.ID, src, ref); err == nil {
			t.Fatal("Resolve succeeded, want the failure")
		}
	}
	if src.instrumentCalls["uid-sber"] != 2 {
		t.Errorf("InstrumentByUID called %d times, want twice", src.instrumentCalls["uid-sber"])
	}
}

// Likewise for a forgotten pair, while each operation's own description
// is still read.
func TestResolveCurrency_TheBrokersRefusalIsAskedOnceARun(t *testing.T) {
	f := newFixture(t)
	src := newFakePassportSource()
	rates := ratesOf(map[string]string{"USD": "74.30"})

	r := NewResolver(f.store, &countingCatalog{Store: instrument.NewStore(f.pool)}, nil).WithRates(rates)
	if _, err := r.ResolveCurrency(f.ctx, src, "uid-usd-gone", currencyHint("GLDRUB_TOM", "74.465")); !errors.Is(err, ErrInstrumentNotFound) {
		t.Fatalf("first ResolveCurrency = %v, want the broker's refusal", err)
	}
	got, err := r.ResolveCurrency(f.ctx, src, "uid-usd-gone", currencyHint("USD000UTSTOM", "74.465"))
	if err != nil || got.Code != "USD" {
		t.Fatalf("second ResolveCurrency = %+v, %v, want USD from the trade's own name", got, err)
	}
	if src.currencyNominalCalls["uid-usd-gone"] != 1 {
		t.Errorf("CurrencyNominalByUID called %d times, want once", src.currencyNominalCalls["uid-usd-gone"])
	}
}

// A refusal is remembered per call: a uid the general passport has forgotten is
// not therefore one CurrencyBy has, and that call is still made.
func TestResolve_ARefusalIsRememberedForTheCallThatGaveIt(t *testing.T) {
	f := newFixture(t)
	src := newFakePassportSource()
	src.instrumentErrs["uid-usd"] = fmt.Errorf("%w: uid-usd", ErrInstrumentNotFound)
	src.currencyNominals["uid-usd"] = MoneyValue{Currency: "usd", Units: 1}

	r := NewResolver(f.store, &countingCatalog{Store: instrument.NewStore(f.pool)}, nil)
	if _, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: "uid-usd"}); !errors.Is(err, ErrInstrumentNotFound) {
		t.Fatalf("Resolve = %v, want the passport's refusal", err)
	}
	got, err := r.ResolveCurrency(f.ctx, src, "uid-usd", CurrencyHint{})
	if err != nil || got.Code != "USD" {
		t.Errorf("ResolveCurrency = %+v, %v, want USD from CurrencyBy", got, err)
	}
}
