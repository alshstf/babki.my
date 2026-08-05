package tinvest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"babki.my/babki/internal/instrument"
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
type countingCatalog struct {
	*instrument.Store
	createCalls, updateCalls int
}

func (c *countingCatalog) Create(ctx context.Context, inst instrument.Instrument) (instrument.Instrument, error) {
	c.createCalls++
	return c.Store.Create(ctx, inst)
}

func (c *countingCatalog) Update(ctx context.Context, id uuid.UUID, upd instrument.Update) (instrument.Instrument, error) {
	c.updateCalls++
	return c.Store.Update(ctx, id, upd)
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
		if _, err := c.countingCatalog.Store.Create(ctx, c.racedWinner); err != nil {
			return instrument.Instrument{}, fmt.Errorf("raceCatalog: seed the winning row: %w", err)
		}
	}
	return c.countingCatalog.Create(ctx, inst)
}

// fakePassportSource is an in-memory stand-in for *Client, satisfying
// passportSource without a network call. It counts calls per method so
// tests can assert the resolver's memoization actually happens.
type fakePassportSource struct {
	instruments      map[string]InstrumentBrief
	nominals         map[string]MoneyValue
	instrumentCalls  map[string]int
	bondNominalCalls map[string]int
}

func newFakePassportSource() *fakePassportSource {
	return &fakePassportSource{
		instruments:      map[string]InstrumentBrief{},
		nominals:         map[string]MoneyValue{},
		instrumentCalls:  map[string]int{},
		bondNominalCalls: map[string]int{},
	}
}

func (s *fakePassportSource) InstrumentByUID(_ context.Context, uid string) (InstrumentBrief, error) {
	s.instrumentCalls[uid]++
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
		InstrumentRef{InstrumentUID: "uid-sber", FIGI: "BBG004730N88"}, inst.ISIN, inst.Ticker); err != nil {
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
		InstrumentRef{InstrumentUID: "uid-old", FIGI: "FIGI-STABLE"}, inst.ISIN, inst.Ticker); err != nil {
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
		InstrumentRef{InstrumentUID: "uid-a", FIGI: "FIGI-A"}, "", "AAAA"); err != nil {
		t.Fatalf("seed map A: %v", err)
	}
	if err := f.store.saveMap(f.ctx, f.conn.ID, instB.ID,
		InstrumentRef{InstrumentUID: "uid-target", FIGI: "FIGI-B"}, "", "BBBB"); err != nil {
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
		InstrumentRef{InstrumentUID: "uid-gazp", FIGI: "FIGI-OLD"}, "", "GAZP"); err != nil {
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
func TestResolve_UnsupportedInstrumentType(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	src := newFakePassportSource()
	src.instruments["uid-future"] = InstrumentBrief{
		UID: "uid-future", Ticker: "SiZ6", Name: "Фьючерс на USD/RUB", InstrumentType: "future",
	}

	r := NewResolver(f.store, catalog, nil)
	_, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: "uid-future"})
	if !errors.Is(err, ErrUnsupportedInstrumentType) {
		t.Fatalf("Resolve(future) err = %v, want ErrUnsupportedInstrumentType", err)
	}
	if catalog.createCalls != 0 {
		t.Errorf("catalog.Create called %d times, want 0 — an unsupported type must never reach the catalog", catalog.createCalls)
	}
}

// TestResolve_CatalogHitByISIN pins the shared-catalog path: this
// connection has never resolved this instrument, but the catalog already
// carries a row for its ISIN — entered by hand, or resolved first by
// another connection.
func TestResolve_CatalogHitByISIN(t *testing.T) {
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	inst, err := catalog.Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER",
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
	// short-circuiting it first — so the second ref carries a figi and an
	// isin neither the map nor ByISIN has seen, forcing the same
	// ByTickerTradable("LKOH") hit that did the backfill above, on a row
	// that is now already whole.
	src.instruments["uid-lkoh-2"] = InstrumentBrief{
		UID: "uid-lkoh-2", FIGI: "BBG-UNRELATED", ISIN: "RU-UNRELATED-ISIN",
		Ticker: "LKOH", Name: "Лукойл", Currency: "RUB", InstrumentType: "share",
	}
	if _, err := r.Resolve(f.ctx, f.conn.ID, src, InstrumentRef{InstrumentUID: "uid-lkoh-2", FIGI: "BBG-UNRELATED"}); err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if catalog.updateCalls != 1 {
		t.Errorf("catalog.Update called %d times after a row already whole was found again, want still 1", catalog.updateCalls)
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
		UID: "uid-rosn", FIGI: "BBG004731354", ISIN: "RU000A0J2Q07", // deliberately NOT the winner's ISIN, so step 4 (by ISIN) misses
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

// TestResolve_RepeatedResolveMakesOnePassportCallTotal pins decision 5: the
// same instrument resolved twice in one run must not cost the broker's rate
// limit a second InstrumentByUID call. The map write after the first
// Resolve already accounts for most of the saving (see
// TestResolve_MapHitByInstrumentUID); this pins the in-memory passport
// cache as an independent guarantee of the same property.
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
// Store-level guards
// -------------------------------------------------------------------------

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
		InstrumentRef{InstrumentUID: "uid-no-figi"}, "", ""); err != nil {
		t.Fatalf("seed map: %v", err)
	}

	if _, err := f.store.mapByInstrumentUID(f.ctx, f.conn.ID, ""); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("mapByInstrumentUID(\"\") = %v, want pgx.ErrNoRows", err)
	}
	if _, err := f.store.mapByFIGI(f.ctx, f.conn.ID, ""); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("mapByFIGI(\"\") = %v, want pgx.ErrNoRows (must not match the row seeded with an empty figi)", err)
	}
}
