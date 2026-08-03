package instrument_test

import (
	"context"
	"errors"
	"reflect"
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

func TestTypeValid(t *testing.T) {
	if !instrument.TypeShare.Valid() || instrument.Type("nope").Valid() {
		t.Error("Type.Valid broken")
	}
}
