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

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/platform/money"
)

// countingCatalog wraps the REAL *instrument.Store — the one
// instrument/store_test.go already proves ByISIN/ByTickerTradable/
// Create/Update against — and adds nothing but call counters.
//
// It is not an in-memory fake, even though the task brief's own phrasing
// ("фейковые catalog/passportSource") suggested one. tinvest_instrument_map
// carries a real foreign key to instruments(id) (migration 0014); an
// in-memory catalog that never writes a real row there makes every save a
// TestResolve_* reaches for fail with a foreign-key violation, which was
// confirmed empirically while writing these tests rather than assumed. The
// real store is a fixed, already-tested dependency, so nothing about
// resolver.go's own logic goes untested by using it here — only its own
// SQL, covered separately, is exercised alongside it. passportSource below
// stays a genuine in-memory fake: nothing about IT needs a database row to
// exist.
//
// failByISIN/failUpdate, when set, replace that one catalog call's answer with
// the given error and skip the real store — the only way to reach the
// resolver's own handling of a catalog that is simply down, which no fixture
// of rows can produce.
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

// secondConnection adds another connection to the same space — a second
// broker token for the same family, which is what a person holding two
// T-Invest logins has. It exists for the tests about what the Resolver
// carries ACROSS connections (see
// TestResolve_PassportCacheServesASecondConnection).
func (f fixture) secondConnection(t *testing.T) Connection {
	t.Helper()
	conn, err := f.store.CreateConnection(f.ctx, f.spaceID, []byte("nonce||ciphertext-2"), "7b1e", StatusActive)
	if err != nil {
		t.Fatalf("CreateConnection (second): %v", err)
	}
	return conn
}

// raceCatalog simulates createInstrument losing a race on the ticker's
// unique index (decision 4 of the task brief). The FIRST Create for
// raceOnTicker inserts racedWinner through the same real store first — as
// if a concurrent writer's INSERT had already committed — so the resolver's
// own attempt right after it hits the database's own unique_violation and
// gets back a genuine instrument.ErrTickerTaken, exactly as it would from a
// second process. Every other ticker passes straight through.
type raceCatalog struct {
	*countingCatalog
	raceOnTicker string
	racedWinner  instrument.Instrument
}

func (c *raceCatalog) Create(ctx context.Context, inst instrument.Instrument) (instrument.Instrument, error) {
	if c.raceOnTicker != "" && inst.Ticker == c.raceOnTicker {
		c.raceOnTicker = ""
		// Straight to the embedded *instrument.Store, deliberately past both
		// wrappers: seeding the winner is the test's own setup, not a call the
		// resolver made, so it must not show up in createCalls.
		if _, err := c.Store.Create(ctx, c.racedWinner); err != nil {
			return instrument.Instrument{}, fmt.Errorf("raceCatalog: seed the winning row: %w", err)
		}
	}
	return c.countingCatalog.Create(ctx, inst)
}

// fakePassportSource is an in-memory stand-in for *Client, satisfying
// passportSource without a network call. It counts calls per method so
// tests can assert the resolver's memoization actually happens.
//
// instrumentErrs answers one uid with a failure instead of a passport, which
// is how a test says which KIND of failure the broker met — the broker
// answering "no such instrument" and the broker not answering at all are two
// different pieces of news, and callers act differently on them (see
// ErrInstrumentNotFound).
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

// CurrencyNominalByUID answers what a currency instrument trades. Nothing is
// registered by default, so a test that does not set one gets the broker's
// "no such instrument" and the refusal that follows — never a silent zero
// nominal, which is the shape that would put an unnamed currency in the journal.
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

// -------------------------------------------------------------------------
// map-hit paths: no catalog write, no broker
// -------------------------------------------------------------------------

// TestResolve_MapHitByInstrumentUID pins the cheapest and most common path:
// a connection that has already resolved this exact instrument_uid before
// gets its answer from tinvest_instrument_map alone, touching neither the
// catalog nor the broker.
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

// TestResolve_MapHitByFIGI pins the fallback lookup: an operation whose
// instrument_uid this connection has never mapped, but whose figi matches a
// row already on file (drift, or an operation old enough to predate
// instrument_uid). It also pins that the hit is recorded under the NEW
// instrument_uid too — decision 1 of the task brief, "remembered under all
// four identifiers so any one of them can drift" — by resolving the same
// new uid again with the broker made to fail, which only a map hit by uid
// (not by figi) would survive.
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

// TestResolve_MapLookupPrefersInstrumentUIDOverFIGI pins the ORDER the task
// brief names explicitly ("по instrument_uid, затем figi"): when both would
// match — a different row for each, a case that can arise once figi has
// been reused across two instrument_uids over the map's history — the
// instrument_uid match wins.
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

// TestResolve_MapHitRefreshesDriftedAttributes pins that a hit REWRITES the
// row with what the current call sees, rather than freezing it at first
// sight — the same rule MirrorRow's own doc comment gives for confirmed
// mirror rows, applied here to the instrument map: a figi that drifted while
// instrument_uid stayed put must still be captured, and only a hit-path
// write can do it, since mapByInstrumentUID alone would keep matching the
// stale row forever otherwise.
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

// -------------------------------------------------------------------------
// broker/catalog paths
// -------------------------------------------------------------------------

// TestResolve_UnsupportedInstrumentType pins decision 3 of the task brief: a
// futures/options/etc. instrument refuses with ErrUnsupportedInstrumentType
// before any catalog call, rather than being filed as some generic "other"
// row the rest of the program cannot value.
//
// The literal is "futures" and not "future": that is the spelling the API's
// own instrument_type carries for them. Nothing in this repository can be
// pointed at to confirm it — the fixtures in testdata show the field's shape
// ("share", "bond") and hold no derivative at all — which is exactly how the
// wrong spelling survived here unread. The refusal is identical for any string
// brokerInstrumentTypes does not hold, so this test would pass either way; a
// test whose stated subject is futures still has to name futures the way they
// arrive, or the case it claims to cover is not the case it runs.
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

// TestResolve_CurrencyIsRefusedRatherThanCreated pins that "currency" is
// outside the types this resolver accepts, and pins it as a REFUSAL — the one
// answer that cannot go unnoticed.
//
// Buying and selling currency becomes a journal row of type "conversion",
// which the engine skips without ever asking which instrument it names, so
// nothing about a currency operation needs a catalog row and this resolver
// should not be called for one at all. If it is called anyway, the alternative
// to refusing is not "one harmless extra row": neither lookup can find a
// currency (ByTickerTradable does not cover the type, and an ISIN identifies a
// security rather than a currency), creation is what is left, and the unique
// ticker index that would stop a duplicate share, bond or fund covers exactly
// those three types and leaves currency out (migration 0011). Measured with
// currency accepted: two connections resolving one currency left two rows for
// it in the instance-wide catalog.
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

// TestResolve_CatalogHitByISIN pins the shared-catalog path: this
// connection has never resolved this instrument, but the catalog already
// carries a row for its ISIN — entered by hand, or resolved first by
// another connection.
//
// THE SEEDED ROW CARRIES NO TICKER ON PURPOSE, and that is what makes this a
// test of the ISIN step rather than of Resolve in general: with a ticker of
// "SBER" on it, deleting the ISIN lookup from findOrCreate outright left this
// test green — the very next step found the same row by ticker, the backfill
// had nothing to fill, and every assertion below passed. A row entered from a
// statement that named only the ISIN can be reached by nothing but the ISIN.
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

// TestResolve_CatalogHitByTicker_BackfillsISINAndFIGI pins decision 2: a row
// found only by ticker (its ISIN and FIGI never recorded) gets them filled
// in from the broker's passport, because the catalog is shared and an empty
// ISIN there would cost the next connection's exact lookup a hit it should
// have had.
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

	// A row that already carries both must not be touched again. This has
	// to reach findOrCreate's ticker branch a second time WITHOUT a map hit
	// short-circuiting it first — so the second ref carries a figi neither the
	// map nor the catalog has seen, forcing the same ByTickerTradable("LKOH")
	// hit that did the backfill above, on a row that is now already whole.
	//
	// Its passport carries NO ISIN, and that is now the only way to reach this
	// branch on a whole row: a passport naming a different ISIN is refused as a
	// different security (see TestResolve_TickerHitWithAContradictingISINIsRefused),
	// and one naming the same ISIN never gets past the ISIN step. A passport
	// with no ISIN of its own is the remaining case, and the code has always
	// treated it as a reachable one: findOrCreate guards the ISIN step with
	// `brief.ISIN != ""` rather than assuming every passport carries one.
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

// TestResolve_CatalogHitByTicker_CoversBondsAndFunds pins WHICH types the
// exact ticker lookup reaches, which the backfill test above cannot: it holds
// a share alone, so narrowing instrument.ByTickerTradable's own type filter to
// shares would leave it green.
//
// WHAT THAT NARROWING COSTS WAS MEASURED, NOT ASSUMED, because the obvious
// guess about it is wrong. A bond or a fund the catalog already holds stops
// being found and falls through to creation — but it does not become a
// duplicate: the unique ticker index covers those two types as well as shares
// (migration 0011), so the insert loses, and the resolution ends in "instrument
// create lost the ticker race and the re-lookup failed: no rows in result set",
// an error blaming a race that never happened. Every bond and every fund the
// catalog already holds would stop resolving, with that sentence in the log. (Currency is the type where duplicates really do
// pile up, because the index leaves it out; that is a different test.)
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
	// No nominal is registered for the bond uid: if the ticker lookup stopped
	// covering bonds, creation would be reached and would fail on the missing
	// nominal — a second, independent way for this test to notice.

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

// TestResolve_CreatesShare pins plain creation for a type that carries no
// face value, and that Frozen is always false — the API has no field
// meaning "frozen by sanctions" (see createInstrument's own doc comment),
// so a freshly created row must never come out frozen regardless of what
// the passport said.
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

// TestResolve_CreatesBond_CarriesNominalAndCurrency pins the one place a
// bond differs from every other type: its face value comes from a SECOND
// broker call, BondNominalByUID, because GetInstrumentBy (behind
// InstrumentByUID) does not carry a nominal at all (see InstrumentBrief's
// own doc comment). The literal here — 1000 RUB nominal -> face_value_minor
// 100000 — mirrors the live value the task brief's controller checked
// against the sandbox gateway for a real bond uid.
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

// TestResolve_ErrTickerTaken_RetriesAndFindsTheWinner pins decision 4: a
// Create that loses a race on the ticker's unique index does not fail the
// resolution — it looks the ticker up again and uses whichever row won,
// exactly as if that row had been the catalog hit all along.
//
// THE WINNER CARRIES THE PASSPORT'S OWN ISIN, and the ISIN step still misses:
// it ran before the race seeded that row, so there was nothing yet to find.
// The earlier version of this test gave the winner a DIFFERENT ISIN to make
// that step miss, which pinned the very defect this branch fixes — under the
// rule refuseContradiction now states, a row whose ISIN contradicts the
// passport is a different security and taking it is the thing that must not
// happen (see TestResolve_TickerRaceWinnerWithAContradictingISINIsRefused,
// which is now that case's own test).
func TestResolve_ErrTickerTaken_RetriesAndFindsTheWinner(t *testing.T) {
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

// -------------------------------------------------------------------------
// a ticker is not an identity
// -------------------------------------------------------------------------

// TestResolve_TickerHitWithAContradictingISINIsRefused runs the owner's own
// case, the one this rule was written for.
//
// The owner's catalog has AT&T entered by hand under ticker "T" — a foreign
// share whose ISIN was never recorded, which is precisely the row backfilling
// exists to complete. Т-Технологии trade on MOEX under ticker "T" as well, with ISIN
// RU000A107UL4. Connecting the broker used to resolve Т-Технологии to AT&T's
// row and then stamp Т-Технологии's ISIN and figi onto it: permanently, for
// every space in the instance, with every Т-Технологии trade booked against
// AT&T from then on.
//
// So the assertion is in two parts, and the second is the one that matters:
// the refusal names its own reason, AND AT&T's row is exactly as it was.
func TestResolve_TickerHitWithAContradictingISINIsRefused(t *testing.T) {
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
	_, err = r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: "uid-t-tech", FIGI: "TCS10A107UL4"})
	if !errors.Is(err, ErrDifferentSecurity) {
		t.Fatalf("Resolve err = %v, want ErrDifferentSecurity", err)
	}

	after, err := catalog.ByID(f.ctx, att.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if after.ISIN != "US00206R1023" || after.FIGI != "" || after.Name != "AT&T" {
		t.Errorf("AT&T's row after the refused Resolve = %+v, want it untouched (isin US00206R1023, no figi)", after)
	}
	if catalog.updateCalls != 0 {
		t.Errorf("catalog.Update called %d times, want 0 — nothing may be written onto a row that turned out to be a different security", catalog.updateCalls)
	}

	// And the map must not remember the wrong answer either: a resolution that
	// refused resolved nothing, so there is no id to file under this uid.
	var count int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM tinvest_instrument_map WHERE connection_id = $1 AND instrument_uid = $2`,
		f.conn.ID, "uid-t-tech").Scan(&count); err != nil {
		t.Fatalf("count map rows: %v", err)
	}
	if count != 0 {
		t.Errorf("tinvest_instrument_map holds %d row(s) for the refused uid, want 0", count)
	}
}

// TestResolve_TickerHitOfAnotherTypeIsRefused pins the second half of the same
// rule, on a row where the ISIN half cannot fire at all: the catalog row
// carries no ISIN, so only the types can settle whether this is the same
// paper.
//
// A type is not a label here. Every valuation in this program branches on it —
// a bond is priced as a percentage of its face value and a fund at the quote
// itself — so trades filed against a row of the wrong type are mispriced even
// when the ticker really is the same string.
func TestResolve_TickerHitOfAnotherTypeIsRefused(t *testing.T) {
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
	_, err = r.Resolve(f.ctx, f.conn.ID, src,
		InstrumentRef{InstrumentUID: "uid-bond-same-ticker", FIGI: "BBG00SAME001"})
	if !errors.Is(err, ErrDifferentSecurity) {
		t.Fatalf("Resolve err = %v, want ErrDifferentSecurity", err)
	}
	after, err := catalog.ByID(f.ctx, fund.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if after.ISIN != "" || after.FIGI != "" {
		t.Errorf("the fund's row after the refused Resolve = %+v, want no identifiers written onto it", after)
	}
	if catalog.updateCalls != 0 {
		t.Errorf("catalog.Update called %d times, want 0", catalog.updateCalls)
	}
}

// TestResolve_TickerRaceWinnerWithAContradictingISINIsRefused pins the SAME
// rule on the second door to the same backfill: the re-lookup after losing the
// race on a ticker.
//
// That door is if anything the likelier one to meet a stranger behind it. The
// row that won the race was written in the moment between this resolution's
// own lookup and its insert, so it is by construction something this
// resolution has never seen — most plausibly a person entering an unrelated
// paper under this ticker by hand a second earlier.
func TestResolve_TickerRaceWinnerWithAContradictingISINIsRefused(t *testing.T) {
	f := newFixture(t)
	catalog := &raceCatalog{
		countingCatalog: &countingCatalog{Store: instrument.NewStore(f.pool)},
		raceOnTicker:    "T",
		racedWinner: instrument.Instrument{
			Type: instrument.TypeShare, Name: "AT&T", Ticker: "T",
			ISIN: "US00206R1023", Currency: "USD",
		},
	}

	src := newFakePassportSource()
	src.instruments["uid-t-tech"] = InstrumentBrief{
		UID: "uid-t-tech", FIGI: "TCS10A107UL4", ISIN: "RU000A107UL4",
		Ticker: "T", Name: "Т-Технологии", Currency: "RUB", InstrumentType: "share",
	}

	r := NewResolver(f.store, catalog, nil)
	_, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: "uid-t-tech", FIGI: "TCS10A107UL4"})
	if !errors.Is(err, ErrDifferentSecurity) {
		t.Fatalf("Resolve err = %v, want ErrDifferentSecurity", err)
	}
	if catalog.updateCalls != 0 {
		t.Errorf("catalog.Update called %d times, want 0 — the row that won the race is a different security and must not be written to", catalog.updateCalls)
	}
	winner, err := catalog.ByTickerTradable(f.ctx, "T")
	if err != nil {
		t.Fatalf("ByTickerTradable: %v", err)
	}
	if winner.ISIN != "US00206R1023" || winner.FIGI != "" {
		t.Errorf("the winning row after the refused Resolve = %+v, want it untouched", winner)
	}
}

// TestResolve_RepeatedResolveMakesOnePassportCallTotal pins decision 5 at the
// level a sync actually works at: the same instrument resolved twice for the
// same connection costs the broker one InstrumentByUID call, not two.
//
// WHAT MAKES IT PASS IS THE MAP, not the passport cache, and saying so is the
// point of this comment: after the first Resolve the answer is in
// tinvest_instrument_map, so the second never reaches the passport step at
// all. Deleting the cache outright leaves this test green. The cache is pinned
// separately, by the only case that can reach it —
// TestResolve_PassportCacheServesASecondConnection.
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

// TestResolve_PassportCacheServesASecondConnection pins the passport cache
// itself, and it is the only shape of test that can.
//
// The map is per CONNECTION (its uniqueness is (connection_id,
// instrument_uid)), so a second connection holding the same paper starts with
// no memory of it and reaches the passport step for real. If the cache did not
// exist, that step would call the broker a second time for an answer this run
// already has in hand.
//
// It also pins WHAT the cache is keyed by: the instrument, not the connection.
// That is deliberate — an instrument's passport is reference data about the
// paper, identical whichever token asked for it — and it is what makes the
// saving worth having on an account list several connections deep.
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

// TestResolve_EmptyInstrumentUIDIsNeverPersisted pins the guard on Resolve's
// own final write: a ref with no instrument_uid at all is resolved but
// never saved to the map. The table's uniqueness is per (connection_id,
// instrument_uid) (migration 0014); writing under the empty string would
// let two unrelated instruments' figi-only refs collide on the same row the
// next time either was resolved, each silently overwriting the other's
// answer.
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

// -------------------------------------------------------------------------
// what a passport has to carry before a catalog row can be made of it
// -------------------------------------------------------------------------

// TestResolve_IncompletePassportCreatesNothing pins the two fields a catalog
// row cannot honestly be made without, on the door that has no other guard.
//
// instrument.Store validates nothing: name and currency are NOT NULL columns
// with no CHECK behind them (migration 0004), so the empty string satisfies
// both, and every rule about them lives in the catalog's HTTP handler — which
// an importer never goes through. A nameless row would sit in the shared
// catalog looking like a real one and answering no search; a row with no
// currency would publish every figure about itself with no currency on it —
// the failure instrument/http.go spells out for the face currency, one column
// over.
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

// TestResolve_BondNominalRefusals pins the second broker call's own failure
// modes, none of which the catalog would have said anything useful about.
//
// Both halves of this were confirmed by removing the checks and watching what
// happened instead. A nominal that is zero or carries no currency reaches the
// database as "violates check constraint instruments_face_value_sound" — an
// error naming a constraint and no instrument, in the middle of a sync. A
// nominal past money.MaxAmountMinor is not refused by the database at all: the
// row inserted, and it is the account's positions screen that pays, answering
// 500 forever afterwards (the failure instrument/http.go's sweep describes on
// the other door into this column). Refusing here names the bond and what was
// wrong with its nominal.
//
// The two fractional nominals are the third shape, and they are two rather than
// one because rounding them fails differently: a tenth of a kopeck ON TOP of a
// whole nominal would be quietly rounded off and nobody would ever know, while
// a nominal SMALLER than half a kopeck would round to zero and land on the same
// constraint violation as the zero above — from a bond the broker did report a
// nominal for.
//
// The failure of the CALL is pinned alongside them because it was covered by
// nothing: BondNominalByUID is the one request in this file whose answer
// creation cannot proceed without.
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

// TestResolve_CatalogFailuresAreReported pins that a catalog which is simply
// down is reported rather than mistaken for "no such row".
//
// The lookup treats pgx.ErrNoRows as a miss and everything else as a failure,
// and reading those two the same way would make a broken catalog look like an
// empty one — creating a duplicate row for everything the sync touched while
// it was broken. The second half covers the backfill's write, whose failure
// has to reach the caller for its own reason: a resolution that swallowed it
// would report success while the shared catalog learned nothing.
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

	// The backfill's own write, on a row found by ticker with no ISIN of its
	// own. Its failure must reach the caller too: the resolution is only
	// complete once the shared catalog has been told what this connection
	// learned.
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

// -------------------------------------------------------------------------
// Store-level guards
// -------------------------------------------------------------------------

// TestSaveMap_EmptyIdentifiersDoNotEraseStoredOnes pins the rule the map's
// write follows about the three identifiers it reads off ONE operation, while
// the row is what the connection has learned across all of them.
//
// What it would erase first is the figi, which is the whole fallback that lets
// a resolution survive an instrument_uid drifting (see mapByFIGI) — so the
// middle assertion states that consequence rather than the storage detail: the
// figi lookup still finds the instrument afterwards.
//
// The review's example for this was a dividend, and that example does not
// hold up: this package's fixtures have no dividend without a figi (theirs
// carries all four identifiers), and their one row with an empty figi is a
// broker fee, which has no instrument_uid either and so never reaches the map
// at all. The case below is therefore written as what it actually is — an
// operation that names the paper by instrument_uid alone — and what it pins is
// the write's rule about an empty value, reachable for any of the three.
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

	// AND THE SAME WHEN THE ROW REALLY IS BEING WRITTEN. Everything above also
	// holds if the write is skipped for the OTHER reason saveMap skips writes —
	// that nothing changed at all — so on its own it says nothing about what the
	// SET clause does with an empty identifier. This was found by mutation:
	// assigning the three identifiers outright, with the "nothing changed" guard
	// left in place, kept every assertion above green.
	//
	// So here something genuinely changes — the catalog's ticker, which happens
	// (Т-Технологии traded as TCSG before they were T) — while the operation
	// still carries no figi of its own.
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

// TestSaveMap_ResolutionThatChangesNothingWritesNothing pins the other half of
// the same statement. A full history resolves the same instrument on every one
// of its operations — hundreds of times for one paper — and each of those used
// to be a row write saying exactly what the row already said. updated_at is
// what witnesses it here: it is the column such a write would move, and it is
// also the column mapByFIGI orders by, so a no-op write is not even free of
// meaning.
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

	// ...and a resolution that DOES change something still writes: the
	// no-write rule must be about sameness, not about the row already
	// existing.
	if _, err := r.Resolve(f.ctx, f.conn.ID, src,
		InstrumentRef{InstrumentUID: "uid-sber", FIGI: "BBG004730N88", PositionUID: "pos-sber-new"}); err != nil {
		t.Fatalf("third Resolve: %v", err)
	}
	if after := readUpdatedAt(); !after.After(before) {
		t.Errorf("updated_at = %v after a drifted position_uid, want it moved past %v", after, before)
	}
}

// TestInstrumentMapLookups_EmptyIdentifierIsNeverAMatch pins the guard
// mapByInstrumentUID/mapByFIGI both open with: an empty broker identifier
// must read as "no match", never as a query that could pick some unrelated
// row nobody has set that identifier on. Without it, a ref with a blank
// figi (routine — plenty of operations carry none) would risk matching
// whatever row this connection last saved with figi = "".
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
