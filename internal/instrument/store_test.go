package instrument_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	found, hasMore, err := st.Search(ctx, "sber", 10, 0)
	if err != nil || len(found) != 1 || found[0].ID != share.ID || hasMore {
		t.Fatalf("Search sber = %+v, %v, %v", found, hasMore, err)
	}
	// search by name fragment
	if found, _, _ = st.Search(ctx, "офз", 10, 0); len(found) != 1 {
		t.Fatalf("Search офз = %+v", found)
	}
	// empty query returns all ordered by name
	if found, _, _ = st.Search(ctx, "", 10, 0); len(found) != 2 {
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

// makeCatalog writes n instruments whose names sort as "Бумага 01".."Бумага NN"
// and hands back their ids in that same order, so a test can say which rows a
// page ought to hold rather than only how many.
func makeCatalog(t *testing.T, st *instrument.Store, ctx context.Context, n int) []uuid.UUID {
	t.Helper()
	ids := make([]uuid.UUID, 0, n)
	for i := 1; i <= n; i++ {
		created, err := st.Create(ctx, instrument.Instrument{
			Type:     instrument.TypeShare,
			Name:     fmt.Sprintf("Бумага %02d", i),
			Currency: "RUB",
		})
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		ids = append(ids, created.ID)
	}
	return ids
}

func idsOf(found []instrument.Instrument) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(found))
	for _, i := range found {
		out = append(out, i.ID)
	}
	return out
}

// TestSearchPagesPartitionTheCatalog is #104's own property: consecutive
// offsets hand back the catalog in order, once each, and the flag says when to
// stop. Before this, the endpoint took no offset at all and anything past the
// first page was unreachable by any request that could be made.
//
// The page sizes are written out as literals (5 rows in pages of 2) rather than
// derived from a constant, so that the arithmetic below is checked against
// something and not against itself.
func TestSearchPagesPartitionTheCatalog(t *testing.T) {
	st, ctx, _ := newStore(t)
	ids := makeCatalog(t, st, ctx, 5)

	var walked []uuid.UUID
	for offset, page := 0, 0; ; page++ {
		found, hasMore, err := st.Search(ctx, "", 2, offset)
		if err != nil {
			t.Fatalf("Search offset %d: %v", offset, err)
		}
		walked = append(walked, idsOf(found)...)
		if !hasMore {
			// Three pages of 2, 2 and 1: the last one is short, and its
			// shortness is not what ended the walk — hasMore is.
			if page != 2 {
				t.Errorf("catalog of 5 walked in %d pages of 2, want 3", page+1)
			}
			break
		}
		if page > 5 {
			t.Fatal("hasMore never went false walking a catalog of 5 in pages of 2")
		}
		offset += len(found)
	}
	if !reflect.DeepEqual(walked, ids) {
		t.Errorf("walking the catalog in pages of 2 gave %v, want %v (every instrument once, "+
			"in name order): a page that repeats or skips one is what an offset with no total "+
			"order behind it produces", walked, ids)
	}
}

// TestSearchAnswersHasMoreFromTheCatalogAndNotFromThePagesLength pins the one
// thing that cannot be derived afterwards. A full page at the very end of the
// catalog and a full page with more behind it are the same length, so length
// answers neither question; the probe row is what tells them apart.
func TestSearchAnswersHasMoreFromTheCatalogAndNotFromThePagesLength(t *testing.T) {
	st, ctx, _ := newStore(t)
	makeCatalog(t, st, ctx, 4)

	// A page that exactly exhausts the catalog: 4 rows asked for, 4 returned.
	found, hasMore, err := st.Search(ctx, "", 4, 0)
	if err != nil || len(found) != 4 {
		t.Fatalf("Search(limit 4) = %d rows, %v", len(found), err)
	}
	if hasMore {
		t.Errorf("hasMore = true on a page holding the whole catalog: nothing is behind it")
	}

	// The same length, one row short of the catalog: identical evidence to a
	// reader counting rows, opposite answer.
	found, hasMore, err = st.Search(ctx, "", 3, 0)
	if err != nil || len(found) != 3 {
		t.Fatalf("Search(limit 3) = %d rows, %v", len(found), err)
	}
	if !hasMore {
		t.Errorf("hasMore = false with a fourth instrument behind the page: a client told this " +
			"stops asking, and that instrument is then reachable by nothing")
	}

	// Past the end: an empty page is the end of the catalog, never "there may
	// be more further on".
	found, hasMore, err = st.Search(ctx, "", 2, 4)
	if err != nil || len(found) != 0 || hasMore {
		t.Errorf("Search past the end = %d rows, hasMore %v, %v; want 0, false, nil",
			len(found), hasMore, err)
	}
}

// TestSearchOrdersInstrumentsOfOneNameByID is the tie-break, and it is the part
// of paging that fails invisibly. Nothing holds instrument names unique — the
// broker importer writes whatever a paper is called — and among equal names an
// ORDER BY that mentions only the name lets the database return rows in any
// order it likes, a different one per query. Two pages read from such a catalog
// repeat one row and skip another while both look perfectly ordinary.
//
// The two rows are written with ids CHOSEN so that id order is the reverse of
// the order they are stored in, which is what makes this test able to tell the
// two ORDER BYs apart at all: without the tie-break the rows come back in the
// order they went in, which is the opposite of the one asserted.
func TestSearchOrdersInstrumentsOfOneNameByID(t *testing.T) {
	st, ctx, pool := newStore(t)

	high := uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffffffff")
	low := uuid.MustParse("00000000-0000-4000-8000-000000000000")
	for _, id := range []uuid.UUID{high, low} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO instruments (id, type, name, currency) VALUES ($1, 'share', 'Дубль', 'RUB')`,
			id); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	found, _, err := st.Search(ctx, "", 10, 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !reflect.DeepEqual(idsOf(found), []uuid.UUID{low, high}) {
		t.Fatalf("two instruments named «Дубль» came back as %v, want %v (by id): "+
			"with no tie-break the order is the database's to choose and may differ "+
			"between two queries of the same catalog", idsOf(found), []uuid.UUID{low, high})
	}

	// And the consequence that matters: read one at a time, each row appears
	// exactly once.
	first, hasMore, err := st.Search(ctx, "", 1, 0)
	if err != nil || len(first) != 1 || !hasMore {
		t.Fatalf("first page = %+v, hasMore %v, %v", idsOf(first), hasMore, err)
	}
	second, _, err := st.Search(ctx, "", 1, 1)
	if err != nil || len(second) != 1 {
		t.Fatalf("second page = %+v, %v", idsOf(second), err)
	}
	if first[0].ID == second[0].ID {
		t.Errorf("both pages returned %s: one instrument twice and the other never", first[0].ID)
	}
}

// TestSearchRefusesBoundsItCannotHonour covers the two bounds the store
// enforces for itself. The handler in front of it answers 400 on both, so
// reaching here with either means the program is wrong — but a limit of zero
// would otherwise fetch the probe row alone and then trim the page to nothing,
// publishing an empty page with hasMore true: a list showing nothing behind a
// control that loads nothing however often it is pressed.
func TestSearchRefusesBoundsItCannotHonour(t *testing.T) {
	st, ctx, _ := newStore(t)
	makeCatalog(t, st, ctx, 2)

	for _, bad := range []struct {
		limit, offset int
		want          string
	}{
		{0, 0, "limit"},
		{-1, 0, "limit"},
		{10, -1, "offset"},
	} {
		found, hasMore, err := st.Search(ctx, "", bad.limit, bad.offset)
		if err == nil {
			t.Errorf("Search(limit %d, offset %d) = %d rows, hasMore %v, no error",
				bad.limit, bad.offset, len(found), hasMore)
			continue
		}
		if !strings.Contains(err.Error(), bad.want) {
			t.Errorf("Search(limit %d, offset %d) error = %q, want it to name %q",
				bad.limit, bad.offset, err, bad.want)
		}
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

// TestByTickerTradableAnswersOnlyWhereOneRowIsGuaranteed carries the rule of
// the test above onto the THIRD place that spells the same filter.
//
// ByTickerTradable returns a single row with no ORDER BY and no LIMIT, and it
// may: its WHERE names the very types migration 0011's partial unique index
// covers, so at most one row can match. That argument is the whole guarantee,
// and it is an argument about a filter — nothing enforced it. Widen the method
// by one type and it goes on compiling, goes on returning a row, and starts
// returning WHICHEVER of several the planner reached first. The caller that
// would live with that is tinvest's instrument resolver: it hands the broker's
// ticker to this method and files the broker's trades against whatever comes
// back, so a widened filter files them against an arbitrary paper.
//
// WHICH types are covered is not written out here — only that there are seven
// of them. The split is read off ListTradable, as the test above reads it, so
// this stays a comparison between what the reader returns and what this method
// answers about rather than a fourth list of the covered types to drift. For a
// type ListTradable does not return, two rows may share a ticker — and this
// method must then answer nothing rather than one of them.
func TestByTickerTradableAnswersOnlyWhereOneRowIsGuaranteed(t *testing.T) {
	st, ctx, _ := newStore(t)

	types := []instrument.Type{
		instrument.TypeShare, instrument.TypeBond, instrument.TypeETF,
		instrument.TypeCurrency, instrument.TypeCrypto, instrument.TypeMetal,
		instrument.TypeCustom,
	}
	tickerOf := func(tp instrument.Type) string { return "TWO" + strings.ToUpper(string(tp)) }
	duplicated := make(map[instrument.Type]bool, len(types))
	for _, tp := range types {
		if _, err := st.Create(ctx, instrument.Instrument{
			Type: tp, Name: "первый " + string(tp), Ticker: tickerOf(tp), Currency: "RUB",
		}); err != nil {
			t.Fatalf("Create first %s: %v", tp, err)
		}
		_, err := st.Create(ctx, instrument.Instrument{
			Type: tp, Name: "второй " + string(tp), Ticker: tickerOf(tp), Currency: "RUB",
		})
		switch {
		case err == nil:
			duplicated[tp] = true
		case errors.Is(err, instrument.ErrTickerTaken):
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
	if len(returned) == 0 {
		t.Fatal("ListTradable returned nothing; there would be nothing to compare this method against")
	}

	for _, tp := range types {
		found, err := st.ByTickerTradable(ctx, tickerOf(tp))
		switch {
		case returned[tp]:
			// The index refused the second row, so exactly one exists and this
			// method has to be the one that finds it.
			if err != nil {
				t.Errorf("ByTickerTradable(%q) = %v, want the single %s instrument ListTradable also returns",
					tickerOf(tp), err, tp)
				continue
			}
			if found.Type != tp {
				t.Errorf("ByTickerTradable(%q) returned a %s, want a %s", tickerOf(tp), found.Type, tp)
			}
		default:
			if !duplicated[tp] {
				t.Fatalf("two %s instruments under one ticker were refused, though ListTradable does not return %s: "+
					"this case cannot show what the method does with several rows", tp, tp)
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Errorf("ByTickerTradable(%q) answered %q (err %v) though two %s instruments carry that ticker: "+
					"outside the unique index the query has no single row to return, so it must return none",
					tickerOf(tp), found.Name, err, tp)
			}
		}
	}
}

func TestTypeValid(t *testing.T) {
	if !instrument.TypeShare.Valid() || instrument.Type("nope").Valid() {
		t.Error("Type.Valid broken")
	}
}

// TestByISIN_ExactMatch pins that the lookup is an exact comparison, not a
// substring search: Search already exists for "found something containing
// this text" (ILIKE '%...%'), and an importer resolving a broker's ISIN
// needs the one instrument that IS that ISIN, not every row whose ISIN
// happens to contain it as a fragment.
func TestByISIN_ExactMatch(t *testing.T) {
	st, ctx, _ := newStore(t)

	sber, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER",
		ISIN: "RU0009029540", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("Create share: %v", err)
	}

	got, err := st.ByISIN(ctx, "RU0009029540")
	if err != nil || got.ID != sber.ID {
		t.Fatalf("ByISIN(exact) = %+v, %v, want %v", got, err, sber.ID)
	}

	// A substring of a real ISIN must not match — proves this is not built
	// on ILIKE '%...%' the way Search is.
	if _, err := st.ByISIN(ctx, "RU000902954"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ByISIN(substring) = %v, want pgx.ErrNoRows", err)
	}

	// An ISIN nothing carries at all.
	if _, err := st.ByISIN(ctx, "US0000000000"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ByISIN(unknown) = %v, want pgx.ErrNoRows", err)
	}

	// The empty string is not "no filter" here (unlike Search's query
	// parameter): the catalog carries no uniqueness on ISIN, so a bare
	// `isin = ''` could match every instrument nobody has entered one for
	// and hand back whichever is oldest — a plausible-looking wrong answer
	// instead of the honest "no exact match" this refuses with instead.
	if _, err := st.ByISIN(ctx, ""); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ByISIN(\"\") = %v, want pgx.ErrNoRows", err)
	}
}

// TestByISIN_DuplicateReturnsOldest pins the tie-break the doc comment
// promises for the one case migration 0011 never covered: ISIN carries no
// unique constraint (duplicates were entered by hand before this method
// existed), so an importer resolving one still needs a single, deterministic
// answer rather than whichever row Postgres happens to return first.
func TestByISIN_DuplicateReturnsOldest(t *testing.T) {
	st, ctx, _ := newStore(t)

	first, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Дубль, первый", Ticker: "DUP1",
		ISIN: "RU0000000001", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	// created_at is assigned by now() inside each INSERT's own implicit
	// transaction; a short sleep keeps the two rows apart at whatever
	// precision the column actually holds, rather than relying on two
	// statements issued back to back always landing in different
	// microseconds.
	time.Sleep(5 * time.Millisecond)
	if _, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Дубль, второй", Ticker: "DUP2",
		ISIN: "RU0000000001", Currency: "RUB",
	}); err != nil {
		t.Fatalf("Create second: %v", err)
	}

	got, err := st.ByISIN(ctx, "RU0000000001")
	if err != nil {
		t.Fatalf("ByISIN: %v", err)
	}
	if got.ID != first.ID {
		t.Fatalf("ByISIN(duplicate) = %q (%v), want the oldest, %q (%v)",
			got.Name, got.ID, first.Name, first.ID)
	}
}

// TestByTickerTradable pins the two things ByISIN's sibling method has to
// get right: an exact match among share/bond/etf, and exclusion of exactly
// what ListTradable excludes (currency/crypto and tickerless rows) — the
// partial unique index behind "at most one" (migration 0011) only ever
// covers the rows ListTradable returns, so this method's WHERE has to name
// the identical set or its own "at most one" guarantee would not hold.
func TestByTickerTradable(t *testing.T) {
	st, ctx, _ := newStore(t)

	share, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("Create share: %v", err)
	}
	if _, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeCurrency, Name: "Доллар США", Ticker: "USD000UTSTOM", Currency: "USD",
	}); err != nil {
		t.Fatalf("Create currency: %v", err)
	}
	if _, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeCrypto, Name: "Bitcoin", Ticker: "BTC", Currency: "USD",
	}); err != nil {
		t.Fatalf("Create crypto: %v", err)
	}

	got, err := st.ByTickerTradable(ctx, "SBER")
	if err != nil || got.ID != share.ID {
		t.Fatalf("ByTickerTradable(SBER) = %+v, %v, want %v", got, err, share.ID)
	}

	if _, err := st.ByTickerTradable(ctx, "USD000UTSTOM"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ByTickerTradable(currency ticker) = %v, want pgx.ErrNoRows (currency is not tradable)", err)
	}
	if _, err := st.ByTickerTradable(ctx, "BTC"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ByTickerTradable(crypto ticker) = %v, want pgx.ErrNoRows (crypto is not tradable)", err)
	}
	if _, err := st.ByTickerTradable(ctx, "NOPE"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ByTickerTradable(unknown) = %v, want pgx.ErrNoRows", err)
	}
	// Empty ticker: never "any tickerless row", for the same reason
	// ByISIN("") refuses rather than picking one — and here there would be
	// many candidates, since any number of instruments may carry no ticker.
	if _, err := st.ByTickerTradable(ctx, ""); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ByTickerTradable(\"\") = %v, want pgx.ErrNoRows", err)
	}
}
