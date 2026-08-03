package instrument_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/platform/testdb"
)

// newStore also hands back the pool, unused by most tests but needed by
// TestByIDs to inspect Stat().AcquireCount() around the batched read — the
// same technique marketdata.Store's own batch test (FxRatesOn) uses to pin
// its round-trip count.
func newStore(t *testing.T) (*instrument.Store, context.Context, *pgxpool.Pool) {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()
	return instrument.NewStore(pool), ctx, pool
}

func TestInstrumentLifecycle(t *testing.T) {
	st, ctx, _ := newStore(t)

	face := int64(1_000_00)
	faceCur := "RUB"
	bond, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeBond, Name: "ОФЗ 26238", Ticker: "SU26238RMFS4",
		ISIN: "RU000A1038V6", Currency: "RUB",
		FaceValueMinor: &face, FaceCurrency: &faceCur,
	})
	if err != nil {
		t.Fatalf("Create bond: %v", err)
	}
	if bond.ID.String() == "" || bond.FaceValueMinor == nil || *bond.FaceValueMinor != 1_000_00 {
		t.Fatalf("bond = %+v", bond)
	}

	share, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER",
		ISIN: "RU0009029540", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("Create share: %v", err)
	}

	// search by ticker fragment, case-insensitive
	found, err := st.Search(ctx, "sber", 10)
	if err != nil || len(found) != 1 || found[0].ID != share.ID {
		t.Fatalf("Search sber = %+v, %v", found, err)
	}
	// search by name fragment
	if found, _ = st.Search(ctx, "офз", 10); len(found) != 1 {
		t.Fatalf("Search офз = %+v", found)
	}
	// empty query returns all ordered by name
	if found, _ = st.Search(ctx, "", 10); len(found) != 2 {
		t.Fatalf("Search all = %d", len(found))
	}

	// update: freeze + rename ticker
	frozen := true
	newTicker := "SBERP"
	upd, err := st.Update(ctx, share.ID, instrument.Update{Frozen: &frozen, Ticker: &newTicker})
	if err != nil || !upd.Frozen || upd.Ticker != "SBERP" {
		t.Fatalf("Update = %+v, %v", upd, err)
	}

	// tri-state: clear face value on the bond
	var nilFace *int64
	var nilCur *string
	upd, err = st.Update(ctx, bond.ID, instrument.Update{FaceValueMinor: &nilFace, FaceCurrency: &nilCur})
	if err != nil || upd.FaceValueMinor != nil {
		t.Fatalf("clear face = %+v, %v", upd, err)
	}

	if _, err := st.ByID(ctx, share.ID); err != nil {
		t.Fatalf("ByID: %v", err)
	}
}

func TestListTradable(t *testing.T) {
	st, ctx, _ := newStore(t)

	share, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("Create share: %v", err)
	}
	bond, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeBond, Name: "ОФЗ 26238", Ticker: "SU26238RMFS4", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("Create bond: %v", err)
	}
	etf, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeETF, Name: "FinEx USA", Ticker: "FXUS", Currency: "USD",
	})
	if err != nil {
		t.Fatalf("Create etf: %v", err)
	}
	// no ticker -> excluded even though the type is tradable.
	if _, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Без тикера", Currency: "RUB",
	}); err != nil {
		t.Fatalf("Create tickerless: %v", err)
	}
	// non-tradable type with a ticker -> excluded.
	if _, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeCurrency, Name: "USD", Ticker: "USD000UTSTOM", Currency: "USD",
	}); err != nil {
		t.Fatalf("Create currency: %v", err)
	}

	got, err := st.ListTradable(ctx)
	if err != nil {
		t.Fatalf("ListTradable: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ListTradable len = %d, want 3: %+v", len(got), got)
	}
	ids := map[uuid.UUID]bool{}
	for _, i := range got {
		if i.Ticker == "" {
			t.Fatalf("ListTradable returned tickerless instrument: %+v", i)
		}
		ids[i.ID] = true
	}
	for _, want := range []uuid.UUID{share.ID, bond.ID, etf.ID} {
		if !ids[want] {
			t.Fatalf("ListTradable missing %v", want)
		}
	}
}

// TestByIDs pins the batched read the positions screen uses in place of one
// ByID per position: every id that has a row comes back with its full record,
// an id that has none is simply ABSENT — never a zero-valued Instrument, which
// would carry an empty name and an invalid type and read exactly like a real
// catalog row to a caller that skipped the comma-ok — the read costs exactly
// one round trip for the whole set, and an empty request is answered without
// asking the database anything.
func TestByIDs(t *testing.T) {
	st, ctx, pool := newStore(t)

	face := int64(1_000_00)
	faceCur := "USD"
	bond, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeBond, Name: "ОФЗ 26238", Ticker: "SU26238RMFS4",
		Currency: "RUB", FaceValueMinor: &face, FaceCurrency: &faceCur,
	})
	if err != nil {
		t.Fatalf("Create bond: %v", err)
	}
	share, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("Create share: %v", err)
	}
	// Created but not asked for: a batch must answer the ids it was given and
	// nothing else, or a caller indexing by id would silently carry rows it
	// never requested.
	if _, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Лукойл", Ticker: "LKOH", Currency: "RUB",
	}); err != nil {
		t.Fatalf("Create unrelated share: %v", err)
	}

	absent := uuid.New()
	// Discriminating check: fetching a set of ids must take exactly one round
	// trip to the database, not one per id — that is the entire reason ByIDs
	// exists instead of a loop of ByID calls (see marketdata.Store.FxRatesOn's
	// identical AcquireCount check for the sibling batch primitive this one is
	// modelled on). AcquireCount is a lifetime counter on the pool, so
	// comparing before and after catches an implementation that compiles and
	// returns the right instruments while quietly issuing one query per id.
	before := pool.Stat().AcquireCount()
	got, err := st.ByIDs(ctx, []uuid.UUID{bond.ID, share.ID, absent})
	if err != nil {
		t.Fatalf("ByIDs: %v", err)
	}
	if after := pool.Stat().AcquireCount(); after-before != 1 {
		t.Fatalf("ByIDs(3 ids) acquired %d connections, want exactly 1", after-before)
	}
	if len(got) != 2 {
		t.Fatalf("ByIDs len = %d, want 2 (the two ids that have rows): %+v", len(got), got)
	}
	if _, found := got[absent]; found {
		t.Errorf("ByIDs answered for an id with no row: %+v", got[absent])
	}

	// The whole record, not just the id: the positions screen reads type,
	// face value and face currency off these rows to value a bond, so a
	// batch that returned a thinner instrument than ByID would change the
	// numbers on the page rather than only their cost.
	gotBond, found := got[bond.ID]
	if !found {
		t.Fatalf("ByIDs missing the bond")
	}
	byID, err := st.ByID(ctx, bond.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	// DeepEqual rather than ==: the face value and face currency are
	// pointers, and two reads of one row hold equal values behind different
	// addresses.
	if !reflect.DeepEqual(gotBond, byID) {
		t.Errorf("ByIDs bond = %+v, ByID bond = %+v — the two reads must return the identical row", gotBond, byID)
	}
	if got[share.ID].Name != "Сбербанк" {
		t.Errorf("ByIDs share = %+v, want Сбербанк", got[share.ID])
	}

	// Empty input -> empty map and not a single round trip to the database.
	// AcquireCount is a lifetime counter on the pool, so comparing before and
	// after catches an implementation that dropped the len(ids)==0
	// short-circuit and queried the database with an empty array instead
	// (see marketdata.Store.FxRatesOn's identical check for the same claim).
	beforeEmpty := pool.Stat().AcquireCount()
	empty, err := st.ByIDs(ctx, nil)
	if err != nil {
		t.Fatalf("ByIDs(nil): %v", err)
	}
	if afterEmpty := pool.Stat().AcquireCount(); afterEmpty != beforeEmpty {
		t.Errorf("ByIDs(nil) acquired a connection (before=%d after=%d), want zero round trips for empty input", beforeEmpty, afterEmpty)
	}
	if len(empty) != 0 {
		t.Errorf("ByIDs(nil) = %+v, want an empty map", empty)
	}
}

// TestTickerIsUniqueAmongInstrumentsThatCarryOne pins the constraint the
// quotes job has always assumed and never had: at most one instrument per
// ticker. That job maps ticker -> instrument id, because the provider answers
// with tickers and knows nothing about this catalog; a map holds one value per
// key, so a second row under the same ticker meant one of the two was
// overwritten and never priced again — no error, no log line, just a position
// showing no quote forever. The catalog refuses the second row instead, and
// says which rule was broken: a 400 the caller can act on, not the 500 a raw
// unique violation turns into at family.WriteError's default branch.
//
// The EMPTY ticker is deliberately outside the constraint. It is how "this
// instrument has no exchange ticker" is written down — cash, metals, hand-made
// holdings — those rows are excluded from ListTradable and never looked up by
// ticker, so a full unique index would forbid the second one of them for
// nothing.
func TestTickerIsUniqueAmongInstrumentsThatCarryOne(t *testing.T) {
	st, ctx, _ := newStore(t)

	if _, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER", Currency: "RUB",
	}); err != nil {
		t.Fatalf("Create first SBER: %v", err)
	}

	_, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Сбербанк, второй раз", Ticker: "SBER", Currency: "RUB",
	})
	if !errors.Is(err, instrument.ErrTickerTaken) {
		t.Fatalf("Create a second SBER: err = %v, want ErrTickerTaken", err)
	}
	// Wrapping ErrValidation is what makes it a 400 rather than "internal
	// error": the two assertions are separate because the sentinel could be
	// defined without it and the caller would still see a 500.
	if !errors.Is(err, family.ErrValidation) {
		t.Errorf("Create a second SBER: err = %v, want it to wrap family.ErrValidation so the caller gets a 400", err)
	}

	// Renaming an existing instrument onto a taken ticker is the same
	// collision, arrived at through the other write path.
	gazp, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Газпром", Ticker: "GAZP", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("Create GAZP: %v", err)
	}
	taken := "SBER"
	if _, err := st.Update(ctx, gazp.ID, instrument.Update{Ticker: &taken}); !errors.Is(err, instrument.ErrTickerTaken) {
		t.Fatalf("Update GAZP onto SBER: err = %v, want ErrTickerTaken", err)
	}

	// Any number of instruments may carry no ticker at all.
	for _, name := range []string{"Наличные", "Золотой слиток"} {
		if _, err := st.Create(ctx, instrument.Instrument{
			Type: instrument.TypeCustom, Name: name, Currency: "RUB",
		}); err != nil {
			t.Fatalf("Create tickerless %q: %v — the empty ticker must stay outside the unique index", name, err)
		}
	}
}

// TestUniqueTickerCoversExactlyTheRowsListTradableReturns holds two spellings
// of one rule together. The quotes job keys a map on the ticker of every row
// ListTradable hands it, so each of those rows has to be unique by ticker —
// which is what migration 0011's partial index enforces, with that reader's
// filter written out a second time as its predicate. Two spellings drift apart;
// this fails as soon as they do, in either direction:
//
//   - widen the reader without widening the index, and a row it returns turns
//     out to be duplicable — the silent overwrite of #26 is back, one of the
//     pair never priced and nothing saying why;
//   - widen the index without widening the reader, and a duplicate nobody would
//     ever have priced is refused instead — the create dialog offers all seven
//     instrument types with a free ticker field, and two crypto rows for one
//     coin on two venues, or two metal rows both reading XAU for gold vaulted
//     at two brokers, are things a user legitimately has.
//
// The reader itself says which types are which: the test asks the catalog for a
// duplicate under every type there is and compares refusal against what
// ListTradable actually returned, never against a third list of types written
// out here — which would be a third spelling to drift.
func TestUniqueTickerCoversExactlyTheRowsListTradableReturns(t *testing.T) {
	st, ctx, _ := newStore(t)

	types := []instrument.Type{
		instrument.TypeShare, instrument.TypeBond, instrument.TypeETF,
		instrument.TypeCurrency, instrument.TypeCrypto, instrument.TypeMetal,
		instrument.TypeCustom,
	}
	// One ticker per type, so a pair can only collide with itself and a refusal
	// can only mean "this type is covered".
	refused := make(map[instrument.Type]bool, len(types))
	for _, tp := range types {
		ticker := "DUP" + strings.ToUpper(string(tp))
		if _, err := st.Create(ctx, instrument.Instrument{
			Type: tp, Name: "первый " + string(tp), Ticker: ticker, Currency: "RUB",
		}); err != nil {
			t.Fatalf("Create first %s: %v", tp, err)
		}
		_, err := st.Create(ctx, instrument.Instrument{
			Type: tp, Name: "второй " + string(tp), Ticker: ticker, Currency: "RUB",
		})
		switch {
		case err == nil:
			refused[tp] = false
		case errors.Is(err, instrument.ErrTickerTaken):
			refused[tp] = true
		default:
			t.Fatalf("Create second %s: %v", tp, err)
		}
	}

	tradable, err := st.ListTradable(ctx)
	if err != nil {
		t.Fatalf("ListTradable: %v", err)
	}
	returned := make(map[instrument.Type]bool, len(types))
	for _, inst := range tradable {
		returned[inst.Type] = true
	}
	// The comparison below is against what the reader returned, so it has to
	// have returned something: an empty list would agree with an index that
	// refuses nothing at all.
	if len(returned) == 0 {
		t.Fatal("ListTradable returned nothing; there would be nothing for the index to be compared against")
	}
	for _, tp := range types {
		switch {
		case returned[tp] && !refused[tp]:
			t.Errorf("ListTradable returns %s instruments, but the catalog accepted two of them under one ticker: "+
				"the quotes job maps ticker -> instrument id over that list, so one of the two would never be priced", tp)
		case !returned[tp] && refused[tp]:
			t.Errorf("the catalog refused a second %s instrument under a ticker already taken, "+
				"but ListTradable never returns %s instruments — no price is ever fetched for them, "+
				"so the write costs nobody a quote and must be allowed", tp, tp)
		}
	}

	// The other half of the reader's filter, which the loop above cannot see
	// because every row in it carries a ticker. Any number of rows may have no
	// ticker at all, so the reader must not return them: the job's map would
	// collapse every one of them onto the key "" and price none of them.
	for _, name := range []string{"Наличные", "Золотой слиток"} {
		if _, err := st.Create(ctx, instrument.Instrument{
			Type: instrument.TypeShare, Name: name, Currency: "RUB",
		}); err != nil {
			t.Fatalf("Create tickerless %q: %v — rows with no ticker must stay outside the unique index", name, err)
		}
	}
	tradable, err = st.ListTradable(ctx)
	if err != nil {
		t.Fatalf("ListTradable after the tickerless rows: %v", err)
	}
	for _, inst := range tradable {
		if inst.Ticker == "" {
			t.Errorf("ListTradable returned %q, which has no ticker: several such rows may exist, "+
				"so the quotes job would map them all onto the empty key", inst.Name)
		}
	}
}

func TestTypeValid(t *testing.T) {
	if !instrument.TypeShare.Valid() || instrument.Type("nope").Valid() {
		t.Error("Type.Valid broken")
	}
}
