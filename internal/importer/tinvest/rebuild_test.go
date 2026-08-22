package tinvest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/portfolio"
)

// These tests run against a real database and the REAL operation.Service —
// the rebuild's whole job is to hand a difference to the journal's own write
// path, and a fake journal would leave every rule that path enforces (the
// engine's replay, the FIFO release behind a transfer, the per-type
// validation) unexercised by the very tests that are supposed to prove the
// difference was computed right.
//
// EVERY EXPECTED VALUE IS A LITERAL, for the reason projection_test.go states
// at its own head: an expectation derived from the implementation moves with
// it and stays green through a change of rule.

const (
	// uidSber and uidAAPL are the instrument_uids the operation fixtures in
	// testdata/ops carry. They are spelled here so a passport can be put
	// behind them; nothing computes them.
	uidSber    = "e6123145-9665-43e0-8413-cd61b8aa9b13"
	uidAAPL    = "9654c2dd-6993-427e-80fa-04e80a1cf4da"
	uidFutures = "b0e1a5f2-1a3c-4f0e-9b7a-9d6a6f4c1a11"
	uidUSDRUB  = "a22a1263-8e1b-4546-a1aa-416463f104d3"
	uidBond    = "00e0a5a6-3f9a-4b26-bdb9-95cbbaa1c0f2"
)

// recordingDelta wraps the real operation.Service and remembers every delta it
// was handed. It is what lets a test say "the second rebuild asked for
// nothing" rather than merely "the journal ended up the same" — a rebuild that
// deleted every row and wrote it back would satisfy the second sentence and
// break the property this whole file exists for.
type recordingDelta struct {
	inner  *operation.Service
	deltas []operation.ImportDelta
}

func (d *recordingDelta) ApplyImportDelta(ctx context.Context, spaceID uuid.UUID, delta operation.ImportDelta) (
	[]operation.Operation, []operation.ImportRefusal, error,
) {
	d.deltas = append(d.deltas, delta)
	return d.inner.ApplyImportDelta(ctx, spaceID, delta)
}

type rebuildFixture struct {
	fixture
	ops     *operation.Store
	applier *recordingDelta
	src     *fakePassportSource
	catalog *countingCatalog
	rates   *fakeRates
	reb     *Rebuilder
}

func newRebuildFixture(t *testing.T) *rebuildFixture {
	t.Helper()
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
	// An empty rate table by default: the fallback that reads a forgotten
	// currency pair from its name needs an official rate to check against, and
	// a test that wants it says so by putting one in (see fakeRates).
	rates := ratesOf(map[string]string{})
	src := newFakePassportSource()
	src.instruments[uidSber] = InstrumentBrief{
		UID: uidSber, FIGI: "BBG004730N88", ISIN: "RU0009029540",
		Ticker: "SBER", Name: "Сбербанк", Currency: "RUB", InstrumentType: "share",
	}
	src.instruments[uidAAPL] = InstrumentBrief{
		UID: uidAAPL, FIGI: "BBG000BVPV84", ISIN: "US0378331005",
		Ticker: "AAPL", Name: "Apple", Currency: "USD", InstrumentType: "share",
	}
	// The broker answers about a futures contract and about a currency too,
	// and both answers are ones this program cannot use. They are here so that
	// the tests below fail on the CALL — a rebuild that asks about these is
	// asking for nothing — rather than on a fake that had no answer ready.
	src.instruments[uidFutures] = InstrumentBrief{
		UID: uidFutures, FIGI: "FUTSI0323000", Ticker: "SiH3",
		Name: "Фьючерс USD/RUB", Currency: "RUB", InstrumentType: "futures",
	}
	src.instruments[uidUSDRUB] = InstrumentBrief{
		UID: uidUSDRUB, FIGI: "BBG0013HGFT4", Ticker: "USDRUB_TOM",
		Name: "Доллар США", Currency: "RUB", InstrumentType: "currency",
	}
	// The bond the redemption fixtures redeem, drawn from the owner's own
	// account: denominated in yuan and traded and redeemed in roubles. The two
	// currencies are the point rather than colour — they are why the count of
	// bonds cannot be divided out of the payment, and why it has to come from
	// the journal.
	src.instruments[uidBond] = InstrumentBrief{
		UID: uidBond, FIGI: "BBG00T22WKV5", ISIN: "RU000A1075J3",
		Ticker: "RU000A1075J3", Name: "МФК Быстроденьги", Currency: "CNY", InstrumentType: "bond",
	}
	src.nominals[uidBond] = MoneyValue{Currency: "CNY", Units: 100}
	ops := operation.NewStore(f.pool)
	applier := &recordingDelta{inner: operation.NewService(ops)}
	return &rebuildFixture{
		fixture: f, ops: ops, applier: applier, src: src, catalog: catalog,
		rates: rates,
		reb:   NewRebuilder(f.store, NewResolver(f.store, catalog, nil).WithRates(rates), applier, ops, nil),
	}
}

// sync puts one broker account's WHOLE history into the mirror through the
// real SyncMirror — the mirror rows a rebuild reads have to be rows the sync
// could actually have written.
func (f *rebuildFixture) sync(t *testing.T, link AccountLink, items ...OperationItem) {
	t.Helper()
	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, link, items, time.Now().UTC()); err != nil {
		t.Fatalf("SyncMirror: %v", err)
	}
}

func (f *rebuildFixture) rebuild(t *testing.T, links ...AccountLink) RebuildStats {
	t.Helper()
	if len(links) == 0 {
		links = []AccountLink{f.link}
	}
	stats, err := f.reb.Rebuild(f.ctx, f.conn, links, f.src)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	return stats
}

// journalOf reads one account's imported operations back out of the database,
// which is the only place an assertion may look: what the rebuild handed over
// and what the journal kept are exactly the two things this path must not let
// diverge.
func (f *rebuildFixture) journalOf(t *testing.T, accountID uuid.UUID) []operation.Operation {
	t.Helper()
	ops, err := f.ops.ListBySource(f.ctx, f.spaceID, accountID, Source)
	if err != nil {
		t.Fatalf("ListBySource: %v", err)
	}
	return ops
}

// mustListForEngine reads one account's whole journal the way every later read
// does — through the engine's own listing, not through what a rebuild returned.
// Those two are exactly the pair this path must not let diverge.
func mustListForEngine(t *testing.T, f *rebuildFixture, accountID uuid.UUID) []operation.Operation {
	t.Helper()
	ops, err := f.ops.ListForEngine(f.ctx, f.spaceID, accountID)
	if err != nil {
		t.Fatalf("ListForEngine: %v", err)
	}
	return ops
}

// realizedOf folds one account's WHOLE journal — read back through the engine's
// own listing, not through what a rebuild returned — and sums the realized
// profit over every position in it. It goes through portfolio.Compute because
// that is what every screen and every tax figure is derived from: a number this
// test could compute for itself would only prove this test's arithmetic.
func (f *rebuildFixture) realizedOf(t *testing.T, accountID uuid.UUID) int64 {
	t.Helper()
	positions, err := portfolio.Compute(mustListForEngine(t, f, accountID))
	if err != nil {
		t.Fatalf("the journal that was written does not replay when read back: %v", err)
	}
	var total int64
	for _, p := range positions {
		total += realizedOf(t, p)
	}
	return total
}

func (f *rebuildFixture) mirrorRow(t *testing.T, link AccountLink, brokerOpID string) MirrorRow {
	t.Helper()
	rows, err := f.store.MirrorRowsByLink(f.ctx, link.ID)
	if err != nil {
		t.Fatalf("MirrorRowsByLink: %v", err)
	}
	for _, row := range rows {
		if row.BrokerOperationID == brokerOpID {
			return row
		}
	}
	t.Fatalf("no mirror row with broker operation id %q", brokerOpID)
	return MirrorRow{}
}

// deltasSince returns the deltas recorded after mark, so a test can say what a
// particular rebuild asked for rather than what every rebuild together did.
func (f *rebuildFixture) deltasSince(mark int) []operation.ImportDelta {
	return f.applier.deltas[mark:]
}

// mirrorVersions is each mirror row's xmin — the transaction that last wrote
// it. It is how a test can say that a rebuild wrote NOTHING to the mirror,
// which no read of the rows' own columns could: a statement that sets a column
// to the value it already holds leaves the row looking identical and still
// makes a new version of it.
func (f *rebuildFixture) mirrorVersions(t *testing.T) map[uuid.UUID]string {
	t.Helper()
	rows, err := f.pool.Query(f.ctx,
		`SELECT id, xmin::text FROM tinvest_operations_mirror WHERE connection_id = $1`, f.conn.ID)
	if err != nil {
		t.Fatalf("read mirror versions: %v", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]string{}
	for rows.Next() {
		var id uuid.UUID
		var version string
		if err := rows.Scan(&id, &version); err != nil {
			t.Fatalf("read mirror versions: %v", err)
		}
		out[id] = version
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read mirror versions: %v", err)
	}
	return out
}

func byExternalID(t *testing.T, ops []operation.Operation, id string) operation.Operation {
	t.Helper()
	for _, o := range ops {
		if o.ExternalID != nil && *o.ExternalID == id {
			return o
		}
	}
	t.Fatalf("no journal operation with external id %q among %d", id, len(ops))
	return operation.Operation{}
}

// externalIDFor is the name the projection gives the n-th (1-based) journal
// entry of one mirror row. The scheme is spelled out here rather than imported
// from the implementation so that a change to it reddens these tests instead
// of moving with them.
func externalIDFor(row MirrorRow, leg int) string {
	return row.ID.String() + "/" + strconv.Itoa(leg)
}

// -------------------------------------------------------------------------
// idempotence — the property the whole architecture rests on
// -------------------------------------------------------------------------

// TestRebuildOverAnUnchangedMirrorAsksForNothing is the load-bearing test of
// this file. A rebuild that recomputed the journal from scratch every hour
// would still leave the right rows there — and would renumber every one of
// them, break every external reference to them, and make "what changed" a
// question nothing could answer. So the assertion is on the DELTA, not on the
// journal: the second rebuild must ask for nothing at all.
func TestRebuildOverAnUnchangedMirrorAsksForNothing(t *testing.T) {
	f := newRebuildFixture(t)
	f.sync(t, f.link,
		loadOperationItem(t, "input.json"),
		loadOperationItem(t, "buy.json"),
		loadOperationItem(t, "dividend.json"))

	first := f.rebuild(t)
	if first.Added != 3 {
		t.Fatalf("first rebuild added %d operations, want 3", first.Added)
	}
	if first.Removed != 0 || first.Unparsed != 0 {
		t.Errorf("first rebuild removed %d and left %d unparsed, want 0 and 0", first.Removed, first.Unparsed)
	}

	before := f.journalOf(t, f.accountID)
	if len(before) != 3 {
		t.Fatalf("journal holds %d operations, want 3", len(before))
	}
	deposit := byExternalID(t, before, externalIDFor(f.mirrorRow(t, f.link, "op-input-1"), 1))
	if deposit.Type != operation.TypeDeposit || deposit.AmountMinor != 5_000_000 || deposit.Currency != "RUB" {
		t.Errorf("deposit = %s %d %s, want deposit 5000000 RUB", deposit.Type, deposit.AmountMinor, deposit.Currency)
	}
	if want := day(t, "2026-01-09"); !deposit.OccurredOn.Equal(want) {
		t.Errorf("deposit occurred_on = %s, want %s", deposit.OccurredOn.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	buy := byExternalID(t, before, externalIDFor(f.mirrorRow(t, f.link, "op-buy-1"), 1))
	if buy.Type != operation.TypeBuy || buy.AmountMinor != -2_750_000 || buy.FeeMinor != 825 {
		t.Errorf("buy = %s amount %d fee %d, want buy -2750000 825", buy.Type, buy.AmountMinor, buy.FeeMinor)
	}
	if buy.Quantity == nil || !buy.Quantity.Equal(decimal.RequireFromString("100")) {
		t.Errorf("buy quantity = %v, want 100", buy.Quantity)
	}
	if buy.InstrumentID == nil {
		t.Error("buy carries no instrument, want the resolved catalog row")
	}
	dividend := byExternalID(t, before, externalIDFor(f.mirrorRow(t, f.link, "op-div-1"), 1))
	if dividend.Type != operation.TypeDividend || dividend.AmountMinor != 135_075 {
		t.Errorf("dividend = %s %d, want dividend 135075", dividend.Type, dividend.AmountMinor)
	}

	mark := len(f.applier.deltas)
	versions := f.mirrorVersions(t)
	second := f.rebuild(t)
	if second != (RebuildStats{}) {
		t.Errorf("second rebuild reported %+v, want a rebuild that changed nothing", second)
	}
	versionsNow := f.mirrorVersions(t)
	// The count first: the loop below walks the rows that are there NOW and
	// looks each one up among the rows that were there BEFORE, so a row the
	// rebuild deleted would be invisible to it — the missing id is simply never
	// asked about.
	if len(versionsNow) != len(versions) {
		t.Errorf("the mirror holds %d rows after a rebuild that changed nothing, and held %d before",
			len(versionsNow), len(versions))
	}
	for id, now := range versionsNow {
		was, existed := versions[id]
		if !existed {
			t.Errorf("mirror row %s appeared during a rebuild that changed nothing", id)
			continue
		}
		if now != was {
			t.Errorf("mirror row %s was written again by a rebuild that changed nothing (version %s, was %s)", id, now, was)
		}
	}
	for i, d := range f.deltasSince(mark) {
		if len(d.Add) != 0 || len(d.Remove) != 0 {
			t.Errorf("the second rebuild's delta %d asks to add %d and remove %d, want an empty delta",
				i, len(d.Add), len(d.Remove))
		}
	}

	after := f.journalOf(t, f.accountID)
	if len(after) != len(before) {
		t.Fatalf("journal holds %d operations after the second rebuild, want %d", len(after), len(before))
	}
	for i := range before {
		if before[i].ID != after[i].ID {
			t.Errorf("operation %d is row %s after the second rebuild and was %s before — an unchanged mirror must not renumber the journal",
				i, after[i].ID, before[i].ID)
		}
	}
}

// TestRebuildRewritesAnOperationWhoseMirrorRowChanged is the other half of
// idempotence: the difference is by external id but the COMPARISON is by
// value. A mirror row carries the broker's latest word, so a commission the
// broker corrected has to reach the journal — and a rebuild that stopped at
// "this id is already there" would leave the old number in place for good.
// TestRebuildResolvesAPaperTheBrokerForgotByTheIsinTheOperationCarries is the
// wiring that makes the resolver's last resort reachable at all: the ticker the
// OPERATION carries has to travel from the mirror row into the ref.
//
// A fund wound up and its passport answers 404 for ever, while the owner's
// history stays full of operations on it. For such an instrument the broker
// writes the ISIN into the ticker field, and the catalog already knows the
// paper by that ISIN — so the operation lands in the journal instead of joining
// the unparsed list.
func TestRebuildResolvesAPaperTheBrokerForgotByTheIsinTheOperationCarries(t *testing.T) {
	f := newRebuildFixture(t)
	const uidGone = "11111111-2222-3333-4444-555555555555"
	f.src.instrumentErrs[uidGone] = fmt.Errorf("%w: %s", ErrInstrumentNotFound, uidGone)
	if _, err := f.catalog.Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeETF, Name: "Технологии Америки",
		Ticker: "TECH", ISIN: "RU000A101X68", FIGI: "TCS20A101X68", Currency: "RUB",
	}); err != nil {
		t.Fatalf("seed the catalog: %v", err)
	}

	item := loadOperationItem(t, "buy.json")
	item.ID = "op-forgotten-1"
	item.InstrumentUID = uidGone
	// The figi the OPERATION carries differs from the catalog's for the same
	// paper — the broker re-issues it per listing — so nothing but the ISIN
	// can join the two.
	item.FIGI = "TCS33A101X68"
	item.Ticker = "RU000A101X68"
	f.sync(t, f.link, item)

	stats := f.rebuild(t)
	if stats.Unparsed != 0 {
		t.Fatalf("left %d rows unparsed, want none: the catalog knows this paper by the ISIN the operation carries", stats.Unparsed)
	}
	if stats.Added != 1 {
		t.Fatalf("added %d operations, want 1", stats.Added)
	}
	journal := f.journalOf(t, f.accountID)
	if len(journal) != 1 || journal[0].InstrumentID == nil {
		t.Fatalf("journal = %+v, want one operation carrying the catalog row", journal)
	}
}

func TestRebuildRewritesAnOperationWhoseMirrorRowChanged(t *testing.T) {
	f := newRebuildFixture(t)
	f.sync(t, f.link, loadOperationItem(t, "buy.json"))
	f.rebuild(t)
	before := f.journalOf(t, f.accountID)
	if len(before) != 1 || before[0].FeeMinor != 825 {
		t.Fatalf("journal = %d rows with fee %d, want 1 row with fee 825", len(before), before[0].FeeMinor)
	}

	// The same operation, with the commission the broker now reports. The
	// content key is built from the instant, the type, the paper, the currency,
	// the payment and the quantity — not from the commission — so this is the
	// SAME mirror row, refreshed.
	corrected := loadOperationItem(t, "buy.json")
	corrected.Commission = MoneyValue{Currency: "rub", Units: -5, Nano: 0}
	f.sync(t, f.link, corrected)
	rows, err := f.store.MirrorRowsByLink(f.ctx, f.link.ID)
	if err != nil {
		t.Fatalf("MirrorRowsByLink: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the mirror holds %d rows, want 1 — the correction refreshed the row rather than adding one", len(rows))
	}

	stats := f.rebuild(t)
	if stats.Added != 1 || stats.Removed != 1 {
		t.Errorf("rebuild added %d and removed %d, want 1 and 1", stats.Added, stats.Removed)
	}
	after := f.journalOf(t, f.accountID)
	if len(after) != 1 {
		t.Fatalf("journal holds %d operations, want 1", len(after))
	}
	if after[0].FeeMinor != 500 {
		t.Errorf("fee = %d, want 500 — the broker's corrected commission", after[0].FeeMinor)
	}
	if after[0].ID == before[0].ID {
		t.Error("the journal row kept its id: a changed row is removed and written again, not updated in place")
	}
}

// TestRebuildKeepsRealizedProfitWhenTheBrokerRewordsAPurchase is the reason a
// rewritten row inherits the created_at of the row it replaces.
//
// A rewrite is a removal and an insertion, and an insertion stamped afresh is
// the YOUNGEST row of its day — so a purchase the broker merely REWORDED moves
// to the end of its own day. Within a day the journal's order is the order the
// FIFO queue breaks ties in, and the queue is what a later sale consumes from.
// So a description nobody asked about changes which parcel was sold, and the
// realized profit — a tax figure, final once it is struck — moves with it.
//
// The numbers are literal and the two purchases are deliberately far apart in
// price: ten shares at 30 000 ₽ in the morning, ten at 33 000 ₽ in the
// afternoon of the SAME day, ten sold a month later for 32 000 ₽. Selling the
// morning parcel realizes +200 000 kopecks; selling the afternoon one realizes
// −100 000. The sale is never touched by the rebuild below — only the wording
// of the morning purchase is — and the profit must not move at all.
func TestRebuildKeepsRealizedProfitWhenTheBrokerRewordsAPurchase(t *testing.T) {
	f := newRebuildFixture(t)

	morning := loadOperationItem(t, "buy.json")
	morning.ID = "op-buy-morning"
	morning.Date = time.Date(2026, 3, 2, 7, 15, 0, 0, time.UTC) // 10:15 Moscow
	morning.Description = "Покупка 10 шт."
	morning.Payment = MoneyValue{Currency: "rub", Units: -30_000}
	morning.Price = MoneyValue{Currency: "rub", Units: 3_000}
	morning.Commission = MoneyValue{Currency: "rub"}
	morning.Quantity = 10

	afternoon := loadOperationItem(t, "buy.json")
	afternoon.ID = "op-buy-afternoon"
	afternoon.Date = time.Date(2026, 3, 2, 12, 40, 0, 0, time.UTC) // 15:40 Moscow
	afternoon.Description = "Покупка 10 шт."
	afternoon.Payment = MoneyValue{Currency: "rub", Units: -33_000}
	afternoon.Price = MoneyValue{Currency: "rub", Units: 3_300}
	afternoon.Commission = MoneyValue{Currency: "rub"}
	afternoon.Quantity = 10

	sale := loadOperationItem(t, "sell.json")
	sale.Date = time.Date(2026, 4, 1, 7, 0, 0, 0, time.UTC)
	sale.Payment = MoneyValue{Currency: "rub", Units: 32_000}
	sale.Price = MoneyValue{Currency: "rub", Units: 3_200}
	sale.Commission = MoneyValue{Currency: "rub"}
	sale.Quantity = 10

	f.sync(t, f.link, morning, afternoon, sale)
	f.rebuild(t)
	before := f.realizedOf(t, f.accountID)
	if before != 200_000 {
		t.Fatalf("realized profit before the rewording is %d, want 200000 — the morning parcel is the one FIFO sells", before)
	}

	// The broker rewords the MORNING purchase and says nothing new about
	// anything else. The description is not part of the content key, so this is
	// the same mirror row refreshed rather than a new one.
	reworded := morning
	reworded.Description = "Покупка ценных бумаг, 10 шт."
	f.sync(t, f.link, reworded, afternoon, sale)

	mark := len(f.applier.deltas)
	stats := f.rebuild(t)
	if stats.Added != 1 || stats.Removed != 1 {
		t.Fatalf("rebuild added %d and removed %d, want 1 and 1 — only the reworded purchase changed", stats.Added, stats.Removed)
	}
	deltas := f.deltasSince(mark)
	if len(deltas) != 1 || len(deltas[0].Add) != 1 || len(deltas[0].Remove) != 1 {
		t.Fatalf("the rebuild asked for %+v, want one delta rewriting one row", deltas)
	}

	journal := f.journalOf(t, f.accountID)
	if len(journal) != 3 {
		t.Fatalf("journal holds %d operations, want 3", len(journal))
	}
	if journal[0].Note != "Покупка ценных бумаг, 10 шт." || journal[1].Note != "Покупка 10 шт." {
		t.Errorf("the day reads back as %q then %q, want the reworded morning purchase first",
			journal[0].Note, journal[1].Note)
	}
	if after := f.realizedOf(t, f.accountID); after != before {
		t.Errorf("realized profit is %d after the rewording and was %d before — a wording the broker changed moved a tax figure",
			after, before)
	}
}

// TestRebuildRemovesTheJournalRowOfAVanishedMirrorRow: the broker rewrites
// history, and an operation it stops reporting must stop being in the journal.
func TestRebuildRemovesTheJournalRowOfAVanishedMirrorRow(t *testing.T) {
	f := newRebuildFixture(t)
	f.sync(t, f.link, loadOperationItem(t, "input.json"), loadOperationItem(t, "buy.json"))
	f.rebuild(t)
	if got := len(f.journalOf(t, f.accountID)); got != 2 {
		t.Fatalf("journal holds %d operations, want 2", got)
	}

	// The broker's whole history, now without the top-up.
	f.sync(t, f.link, loadOperationItem(t, "buy.json"))
	if row := f.mirrorRow(t, f.link, "op-input-1"); row.DisappearedAt == nil {
		t.Fatal("the mirror row was not marked gone, so this test would prove nothing")
	}

	stats := f.rebuild(t)
	if stats.Added != 0 || stats.Removed != 1 {
		t.Errorf("rebuild added %d and removed %d, want 0 and 1", stats.Added, stats.Removed)
	}
	after := f.journalOf(t, f.accountID)
	if len(after) != 1 || after[0].Type != operation.TypeBuy {
		t.Fatalf("journal = %d rows, first %s, want 1 buy", len(after), after[0].Type)
	}
}

// -------------------------------------------------------------------------
// transfers
// -------------------------------------------------------------------------

// TestRebuildPairsTwoLegsOfOneConnection pins the reason a rebuild is done per
// CONNECTION rather than per account: the two sides of a move between the
// owner's own accounts are two mirror rows under two links, and only something
// looking at both at once can see that they are one event.
func TestRebuildPairsTwoLegsOfOneConnection(t *testing.T) {
	f := newRebuildFixture(t)
	second := f.secondLink(t)
	f.sync(t, f.link, loadOperationItem(t, "buy.json"), loadOperationItem(t, "trans_bs_bs_out.json"))
	f.sync(t, second, loadOperationItem(t, "trans_bs_bs_in.json"))

	stats := f.rebuild(t, f.link, second)
	if stats.Added != 3 {
		t.Fatalf("rebuild added %d operations, want 3 (a buy and two legs)", stats.Added)
	}

	out := byExternalID(t, f.journalOf(t, f.accountID), externalIDFor(f.mirrorRow(t, f.link, "op-trans-2"), 1))
	in := byExternalID(t, f.journalOf(t, second.AccountID), externalIDFor(f.mirrorRow(t, second, "op-trans-1"), 1))
	if out.Type != operation.TypeTransferOut || in.Type != operation.TypeTransferIn {
		t.Fatalf("legs are %s and %s, want transfer_out and transfer_in", out.Type, in.Type)
	}
	if out.TransferGroupID == nil || in.TransferGroupID == nil || *out.TransferGroupID != *in.TransferGroupID {
		t.Fatalf("legs carry groups %v and %v, want one group on both", out.TransferGroupID, in.TransferGroupID)
	}
	// The note is the broker's own wording and nothing else. A leg that can be
	// paired never carries the mark saying its cost is unknown — the projection
	// puts that only on shares arriving from outside — so there is nothing here
	// for the pairing to take back, and a mark appearing on a leg whose basis
	// the journal knows exactly would be a false caption on a true number.
	if in.Note != "Перевод бумаг между счетами" {
		t.Errorf("the paired arrival's note is %q, want the broker's own description alone", in.Note)
	}
	// The 100 shares cost 27508.25 — the 27500.00 paid plus the 8.25
	// commission — so five of them carry 1375.4125, and the journal holds whole
	// minor units. The broker says nothing about any of this; the journal
	// worked it out from the buy.
	if out.AmountMinor != 137_541 || in.AmountMinor != 137_541 {
		t.Errorf("the pair moved %d out and %d in, want 137541 on both — the basis released from the source account",
			out.AmountMinor, in.AmountMinor)
	}
	if len(in.TransferLots) != 1 {
		t.Fatalf("the arriving leg carries %d lots, want 1", len(in.TransferLots))
	}
	if in.TransferLots[0].AcquiredOn == nil || !in.TransferLots[0].AcquiredOn.Equal(day(t, "2026-03-15")) {
		t.Errorf("the parcel was acquired on %v, want 2026-03-15 — the day of the buy it came out of", in.TransferLots[0].AcquiredOn)
	}

	group := *out.TransferGroupID
	mark := len(f.applier.deltas)
	if stats := f.rebuild(t, f.link, second); stats != (RebuildStats{}) {
		t.Errorf("the second rebuild reported %+v, want a rebuild that changed nothing", stats)
	}
	for i, d := range f.deltasSince(mark) {
		if len(d.Add) != 0 || len(d.Remove) != 0 {
			t.Errorf("the second rebuild's delta %d asks to add %d and remove %d, want an empty delta",
				i, len(d.Add), len(d.Remove))
		}
	}
	again := byExternalID(t, f.journalOf(t, f.accountID), externalIDFor(f.mirrorRow(t, f.link, "op-trans-2"), 1))
	if again.TransferGroupID == nil || *again.TransferGroupID != group {
		t.Errorf("the group is %v after a second rebuild and was %s, want the same one — it is derived from the legs' own names, not invented",
			again.TransferGroupID, group)
	}
}

// TestRebuildRewritesBothLegsWhenOneOfThemChanged is why a transfer is diffed
// as one event rather than leg by leg. The journal refuses to remove one leg of
// a pair and leave the other (operation.importRemovals), and it refuses the
// WHOLE difference when asked to — so a difference that noticed only the leg
// that changed would not write half a transfer, it would write nothing at all
// and fail the rebuild.
func TestRebuildRewritesBothLegsWhenOneOfThemChanged(t *testing.T) {
	f := newRebuildFixture(t)
	second := f.secondLink(t)
	departure := loadOperationItem(t, "trans_bs_bs_out.json")
	f.sync(t, f.link, loadOperationItem(t, "buy.json"), departure)
	f.sync(t, second, loadOperationItem(t, "trans_bs_bs_in.json"))
	f.rebuild(t, f.link, second)
	group := *byExternalID(t, f.journalOf(t, f.accountID),
		externalIDFor(f.mirrorRow(t, f.link, "op-trans-2"), 1)).TransferGroupID

	// The broker rewords the departure and nothing else. The description is not
	// part of the content key, so this is the same mirror row — with a note the
	// journal has to follow.
	departure.Description = "Перевод бумаг между счетами (уточнено)"
	f.sync(t, f.link, loadOperationItem(t, "buy.json"), departure)

	stats := f.rebuild(t, f.link, second)
	if stats.Added != 2 || stats.Removed != 2 {
		t.Errorf("rebuild added %d and removed %d, want 2 and 2 — one leg changed and a transfer moves whole",
			stats.Added, stats.Removed)
	}
	out := byExternalID(t, f.journalOf(t, f.accountID), externalIDFor(f.mirrorRow(t, f.link, "op-trans-2"), 1))
	in := byExternalID(t, f.journalOf(t, second.AccountID), externalIDFor(f.mirrorRow(t, second, "op-trans-1"), 1))
	if out.Note != "Перевод бумаг между счетами (уточнено)" {
		t.Errorf("the departure's note is %q, want the broker's new wording", out.Note)
	}
	if out.TransferGroupID == nil || in.TransferGroupID == nil ||
		*out.TransferGroupID != group || *in.TransferGroupID != group {
		t.Errorf("the rewritten pair carries groups %v and %v, want the %s it had — the group is derived from the legs' names, which did not change",
			out.TransferGroupID, in.TransferGroupID, group)
	}
	if in.AmountMinor != 137_541 {
		t.Errorf("the arrival carries a basis of %d after the rewrite, want 137541", in.AmountMinor)
	}
}

// TestRebuildLeavesAnUnpairedLegAlone: shares that left for a broker this
// program knows nothing about are a lone leg, and that is not an error.
func TestRebuildLeavesAnUnpairedLegAlone(t *testing.T) {
	f := newRebuildFixture(t)
	f.sync(t, f.link, loadOperationItem(t, "buy.json"), loadOperationItem(t, "trans_bs_bs_out.json"))

	stats := f.rebuild(t)
	if stats.Added != 2 {
		t.Fatalf("rebuild added %d operations, want 2", stats.Added)
	}
	out := byExternalID(t, f.journalOf(t, f.accountID), externalIDFor(f.mirrorRow(t, f.link, "op-trans-2"), 1))
	if out.TransferGroupID != nil {
		t.Errorf("the lone leg carries group %s, want none — nothing on the other side of it is an account this program mirrors", out.TransferGroupID)
	}
	if out.AmountMinor != 137_541 {
		t.Errorf("the lone leg moved %d, want 137541 — the basis is released from the journal whether or not the leg is paired", out.AmountMinor)
	}
}

// TestRebuildDoesNotPairSharesCrossingToAndFromTheOutsideWorld is the sharp
// case of "pair only what is one event". Shares leaving for a depositary
// outside this program and shares arriving from one are, by the names of the
// broker's own operation types, moves with the OUTSIDE WORLD — so two of them
// that happen to agree on paper, count and day are two unrelated parcels, and
// joining them would invent an event nobody reported.
//
// What that invention costs is why it is refused rather than merely doubted:
// the arriving account would be handed a cost basis and acquisition dates
// released from ANOTHER account's queue — a tax basis that is not its own —
// and the honest mark saying the cost of those shares is unknown would be
// wiped off in the bargain.
//
// If a broker turns out to report a move between two of the owner's own
// accounts under these types after all — still unasked, since the owner's
// history holds no such move — the answer is two lone
// legs with the honest mark on the arrival — the safe side of the mistake.
func TestRebuildDoesNotPairSharesCrossingToAndFromTheOutsideWorld(t *testing.T) {
	f := newRebuildFixture(t)
	second := f.secondLink(t)

	// One paper, one count, one day, two accounts of one connection: everything
	// the pairing looks at, and it must still refuse.
	leaving := loadOperationItem(t, "output_securities.json")
	leaving.Date = time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	arriving := loadOperationItem(t, "input_securities.json")
	arriving.Date = time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)

	f.sync(t, f.link, loadOperationItem(t, "buy.json"), leaving)
	f.sync(t, second, arriving)
	f.rebuild(t, f.link, second)

	in := byExternalID(t, f.journalOf(t, second.AccountID), externalIDFor(f.mirrorRow(t, second, "op-insec-1"), 1))
	if in.TransferGroupID != nil {
		t.Errorf("the arrival was paired into group %s with a departure to a depositary outside this program", in.TransferGroupID)
	}
	if in.Note != "Перевод бумаг от другого брокера — стоимость приобретения неизвестна: брокер её не передаёт" {
		t.Errorf("the arrival's note is %q, want the mark saying its cost is unknown — nobody here knows what those shares cost", in.Note)
	}
	if in.AmountMinor != 0 {
		t.Errorf("the arrival declares a basis of %d, want 0 — a basis taken from the other account's queue would be somebody else's",
			in.AmountMinor)
	}
	out := byExternalID(t, f.journalOf(t, f.accountID), externalIDFor(f.mirrorRow(t, f.link, "op-outsec-1"), 1))
	if out.TransferGroupID != nil {
		t.Errorf("the departure was paired into group %s", out.TransferGroupID)
	}
	// 40 of the 100 shares that cost 27508.25 — the payment plus the
	// commission — is 11003.30. The departing leg gives up its basis whether or
	// not anything is paired with it.
	if out.AmountMinor != 1_100_330 {
		t.Errorf("the departure moved %d, want 1100330", out.AmountMinor)
	}
	if len(in.TransferLots) != 0 {
		t.Errorf("the arrival carries %d lots, want none — no parcel was released to it", len(in.TransferLots))
	}
}

// TestRebuildTakesATransferLegsCurrencyFromThePaper pins the one entry whose
// currency is not the currency of the money that moved, because no money moved.
//
// The broker attaches a currency to the zero payment of a securities transfer,
// and the documented shape attaches roubles to it whatever the paper is. Taken
// as the leg's currency, that is not merely untidy: the engine fixes a
// position's currency by the first operation on the instrument that touches
// cost or quantity — a transfer leg is one — and refuses every later such
// operation that disagrees. The dollar purchase and the rouble leg below
// are the same paper in the same account, so one of the two would be refused —
// the owner reading "the journal refused this" and nothing anywhere saying that
// the currency had been read off the wrong field.
//
// The pairing is asserted with it because the currency is part of the key two
// legs are matched on (see transferPair): a leg that took roubles from the
// payment and another that took dollars would fail to match and become two lone
// legs, each with a cost basis nobody released.
func TestRebuildTakesATransferLegsCurrencyFromThePaper(t *testing.T) {
	f := newRebuildFixture(t)
	second := f.secondLink(t)

	// A dollar paper, bought in dollars, ten of which then move to the other
	// account — with the broker calling the zero payment roubles on both legs.
	purchase := loadOperationItem(t, "buy.json")
	purchase.InstrumentUID = uidAAPL
	purchase.FIGI = "BBG000BVPV84"
	purchase.Date = time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	purchase.Payment = MoneyValue{Currency: "usd", Units: -2_000}
	purchase.Price = MoneyValue{Currency: "usd", Units: 200}
	purchase.Commission = MoneyValue{Currency: "usd"}
	purchase.Quantity = 10

	departing := loadOperationItem(t, "trans_bs_bs_out.json")
	departing.InstrumentUID = uidAAPL
	departing.FIGI = "BBG000BVPV84"
	departing.Quantity = -10
	arriving := loadOperationItem(t, "trans_bs_bs_in.json")
	arriving.InstrumentUID = uidAAPL
	arriving.FIGI = "BBG000BVPV84"
	arriving.Quantity = 10
	if departing.Payment.Currency != "RUB" || arriving.Payment.Currency != "RUB" {
		t.Fatalf("the fixtures price the move in %q and %q, want roubles on both — this test proves nothing otherwise",
			departing.Payment.Currency, arriving.Payment.Currency)
	}

	f.sync(t, f.link, purchase, departing)
	f.sync(t, second, arriving)

	stats := f.rebuild(t, f.link, second)
	if stats.Added != 3 || stats.Unparsed != 0 {
		t.Fatalf("rebuild added %d and left %d unparsed, want 3 and 0 — nothing here is unreadable",
			stats.Added, stats.Unparsed)
	}
	out := byExternalID(t, f.journalOf(t, f.accountID), externalIDFor(f.mirrorRow(t, f.link, "op-trans-2"), 1))
	in := byExternalID(t, f.journalOf(t, second.AccountID), externalIDFor(f.mirrorRow(t, second, "op-trans-1"), 1))
	if out.Currency != "USD" || in.Currency != "USD" {
		t.Errorf("legs are in %q and %q, want USD on both — the paper's money, not the money beside a zero payment",
			out.Currency, in.Currency)
	}
	if out.TransferGroupID == nil || in.TransferGroupID == nil || *out.TransferGroupID != *in.TransferGroupID {
		t.Fatalf("legs carry groups %v and %v, want one group on both", out.TransferGroupID, in.TransferGroupID)
	}
	// The whole account still replays, which is the thing a rouble leg on a
	// dollar position would have cost.
	if _, err := portfolio.Compute(mustListForEngine(t, f, f.accountID)); err != nil {
		t.Errorf("the account no longer replays: %v", err)
	}
}

// TestRebuildKeepsTheUnknownBasisNoteOnALoneArrival is the other side of the
// test above: with nothing to pair against, the note is the truth.
func TestRebuildKeepsTheUnknownBasisNoteOnALoneArrival(t *testing.T) {
	f := newRebuildFixture(t)
	f.sync(t, f.link, loadOperationItem(t, "input_securities.json"))
	f.rebuild(t)

	in := byExternalID(t, f.journalOf(t, f.accountID), externalIDFor(f.mirrorRow(t, f.link, "op-insec-1"), 1))
	if in.TransferGroupID != nil {
		t.Fatalf("the lone arrival carries group %s, want none", in.TransferGroupID)
	}
	if in.Note != "Перевод бумаг от другого брокера — стоимость приобретения неизвестна: брокер её не передаёт" {
		t.Errorf("the lone arrival's note is %q, want the mark saying its cost is unknown", in.Note)
	}
	if in.AmountMinor != 0 {
		t.Errorf("the lone arrival declares a basis of %d, want 0 — nobody knows what those shares cost", in.AmountMinor)
	}
}

// TestPairableLegIsOnlyAMoveBetweenTheOwnersOwnAccounts names the broker's own
// operation types in full, as literals. The table test below drives the rule
// with a boolean, so it says nothing about WHICH broker types set that boolean
// — and a rule read out of the same table it is meant to pin would move with
// it in silence.
func TestPairableLegIsOnlyAMoveBetweenTheOwnersOwnAccounts(t *testing.T) {
	want := map[string]bool{
		"OPERATION_TYPE_TRANS_IIS_BS":      true,
		"OPERATION_TYPE_TRANS_BS_BS":       true,
		"OPERATION_TYPE_INPUT_SECURITIES":  false,
		"OPERATION_TYPE_OUTPUT_SECURITIES": false,
		"OPERATION_TYPE_BUY":               false,
		"OPERATION_TYPE_DIV_EXT":           false,
		"":                                 false,
		"OPERATION_TYPE_SOMETHING_NEW":     false,
	}
	for opType, wantPairable := range want {
		if got := pairableLeg(MirrorRow{OpType: opType}); got != wantPairable {
			t.Errorf("pairableLeg(%q) = %v, want %v", opType, got, wantPairable)
		}
	}
	// And nothing else the broker has: a type added to the mapping table
	// tomorrow is not pairable until somebody says so here.
	for opType := range brokerOpTypes {
		if _, listed := want[opType]; listed {
			continue
		}
		if pairableLeg(MirrorRow{OpType: opType}) {
			t.Errorf("pairableLeg(%q) = true, and this test was never told that type may be paired", opType)
		}
	}
}

// TestPairTransfersJoinsOnlyWhatIsOneEvent walks the pairing rule one
// condition at a time, straight over the function, with no database in the
// way. The database tests above prove what the journal then does with a pair;
// this proves the LIST — and the list is the thing that is easy to get one
// short, because every item on it is a separate way for two legs that are not
// one event to look like one.
func TestPairTransfersJoinsOnlyWhatIsOneEvent(t *testing.T) {
	accountA := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	accountB := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	sber := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	aapl := uuid.MustParse("44444444-4444-4444-8444-444444444444")
	moved := day(t, "2026-05-05")

	type legSpec struct {
		account    uuid.UUID
		typ        operation.Type
		instrument uuid.UUID
		quantity   string
		day        time.Time
		currency   string
		pairable   bool
	}
	departure := legSpec{accountA, operation.TypeTransferOut, sber, "5", moved, "RUB", true}
	arrival := legSpec{accountB, operation.TypeTransferIn, sber, "5", moved, "RUB", true}
	with := func(spec legSpec, change func(*legSpec)) legSpec {
		change(&spec)
		return spec
	}
	build := func(spec legSpec, name string) desired {
		q := decimal.RequireFromString(spec.quantity)
		id := name
		return desired{
			op: operation.Operation{
				AccountID: spec.account, InstrumentID: &spec.instrument, Type: spec.typ,
				OccurredOn: spec.day, Quantity: &q, Currency: spec.currency,
				Source: Source, ExternalID: &id,
			},
			pairable: spec.pairable,
		}
	}

	cases := []struct {
		name     string
		out, in  legSpec
		wantPair bool
	}{
		{"one move between the owner's own accounts", departure, arrival, true},
		{
			"the departure is a move with the outside world",
			with(departure, func(s *legSpec) { s.pairable = false }), arrival, false,
		},
		{
			"the arrival is a move with the outside world",
			departure, with(arrival, func(s *legSpec) { s.pairable = false }), false,
		},
		{
			"both are moves with the outside world",
			with(departure, func(s *legSpec) { s.pairable = false }),
			with(arrival, func(s *legSpec) { s.pairable = false }), false,
		},
		{
			"different currencies",
			departure, with(arrival, func(s *legSpec) { s.currency = "USD" }), false,
		},
		{
			"different days",
			departure, with(arrival, func(s *legSpec) { s.day = day(t, "2026-05-06") }), false,
		},
		{
			"different quantities",
			departure, with(arrival, func(s *legSpec) { s.quantity = "6" }), false,
		},
		{
			"different instruments",
			departure, with(arrival, func(s *legSpec) { s.instrument = aapl }), false,
		},
		{
			"one and the same account",
			departure, with(arrival, func(s *legSpec) { s.account = accountA }), false,
		},
		{
			"two departures and no arrival",
			departure, with(arrival, func(s *legSpec) { s.typ = operation.TypeTransferOut }), false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := []desired{build(c.out, "row-out/1"), build(c.in, "row-in/1")}
			pairTransfers(want)
			out, in := want[0].op.TransferGroupID, want[1].op.TransferGroupID
			switch {
			case c.wantPair && (out == nil || in == nil):
				t.Fatalf("legs carry groups %v and %v, want one group on both", out, in)
			case c.wantPair && *out != *in:
				t.Errorf("legs carry different groups %s and %s, want one", out, in)
			case !c.wantPair && (out != nil || in != nil):
				t.Errorf("legs were joined into groups %v and %v, want neither — they are not one event", out, in)
			}
		})
	}
}

// -------------------------------------------------------------------------
// refusals
// -------------------------------------------------------------------------

// TestRebuildRecordsTheJournalsOwnRefusalAgainstTheMirrorRow: an operation the
// journal will not take must cost the owner one visible row, not the other
// four thousand.
func TestRebuildRecordsTheJournalsOwnRefusalAgainstTheMirrorRow(t *testing.T) {
	f := newRebuildFixture(t)
	// A sale of 100 shares of a position that was never bought.
	f.sync(t, f.link, loadOperationItem(t, "input.json"), loadOperationItem(t, "sell.json"))

	stats := f.rebuild(t)
	if stats.Added != 1 {
		t.Errorf("rebuild added %d operations, want 1 — the sale is refused, the top-up is not", stats.Added)
	}
	if stats.Unparsed != 1 {
		t.Errorf("rebuild left %d rows unparsed, want 1", stats.Unparsed)
	}
	journal := f.journalOf(t, f.accountID)
	if len(journal) != 1 || journal[0].Type != operation.TypeDeposit {
		t.Fatalf("journal = %d rows, first %s, want 1 deposit", len(journal), journal[0].Type)
	}
	refused := f.mirrorRow(t, f.link, "op-sell-1")
	if got := refused.UnparsedReason; got != string(ReasonEngineRefused) {
		t.Errorf("the refused row's reason is %q, want %q", got, ReasonEngineRefused)
	}
	// AND WHAT THE JOURNAL SAID, not only that it said no. The code is the same
	// over a sale with nothing behind it, an amount the journal will not hold,
	// and a transfer whose other leg failed: the owner met 134 rows carrying it
	// and could act on none of them, because the sentence behind it went to a
	// log line and nowhere else.
	//
	// The assertion is that the words are THERE and say something the code does
	// not, never that they read one particular way — nothing may depend on their
	// wording, this test least of all.
	if refused.UnparsedDetail == "" {
		t.Errorf("the refused row's detail is empty — the journal's own words about it were dropped")
	}
	if got := refused.UnparsedDetail; got == string(ReasonEngineRefused) {
		t.Errorf("the detail is the code spelled out again (%q), which adds nothing to it", got)
	}
	if got := f.mirrorRow(t, f.link, "op-input-1").UnparsedReason; got != "" {
		t.Errorf("the top-up's reason is %q, want empty", got)
	}
	if got := f.mirrorRow(t, f.link, "op-input-1").UnparsedDetail; got != "" {
		t.Errorf("the top-up was read successfully and carries the detail %q", got)
	}
}

// TestRebuildClearsAReasonThatStoppedBeingTrue: the refusal above is the
// journal's answer about the journal AS IT IS, and the answer changes when the
// missing history arrives. Nothing about the refused row is remembered as a
// verdict — it is offered again on the next rebuild.
func TestRebuildClearsAReasonThatStoppedBeingTrue(t *testing.T) {
	f := newRebuildFixture(t)
	f.sync(t, f.link, loadOperationItem(t, "sell.json"))
	f.rebuild(t)
	first := f.mirrorRow(t, f.link, "op-sell-1")
	if got := first.UnparsedReason; got != string(ReasonEngineRefused) {
		t.Fatalf("the sale's reason is %q, want %q — this test starts from a refusal", got, ReasonEngineRefused)
	}
	if first.UnparsedDetail == "" {
		t.Fatalf("the sale carries no detail — this test starts from a refusal that explained itself")
	}

	// The buy the broker had not reported yet.
	f.sync(t, f.link, loadOperationItem(t, "buy.json"), loadOperationItem(t, "sell.json"))
	stats := f.rebuild(t)
	if stats.Added != 2 {
		t.Errorf("rebuild added %d operations, want 2", stats.Added)
	}
	if stats.Unparsed != 0 {
		t.Errorf("rebuild left %d rows unparsed, want 0", stats.Unparsed)
	}
	now := f.mirrorRow(t, f.link, "op-sell-1")
	if got := now.UnparsedReason; got != "" {
		t.Errorf("the sale's reason is %q, want empty — the journal takes it now", got)
	}
	// AND THE WORDS GO WITH THE CODE. A sentence explaining a refusal that is
	// no longer being made is the exact shape this project keeps being bitten
	// by: it would sit under whatever code lands on this row next, describing
	// something else entirely, and it would read as current.
	if got := now.UnparsedDetail; got != "" {
		t.Errorf("the sale still carries %q, the detail of a refusal that has stopped being true", got)
	}
}

// TestRebuildRefusesTheProjectionsOwnUnreadableRow pins that a refusal the
// projection makes — not the journal — reaches the mirror with the
// projection's own code on it, and that the reason is not a stand-in.
func TestRebuildRefusesTheProjectionsOwnUnreadableRow(t *testing.T) {
	f := newRebuildFixture(t)
	f.sync(t, f.link, loadOperationItem(t, "input.json"), loadOperationItem(t, "delivery_buy.json"))

	stats := f.rebuild(t)
	if stats.Added != 1 || stats.Unparsed != 1 {
		t.Errorf("rebuild added %d and left %d unparsed, want 1 and 1", stats.Added, stats.Unparsed)
	}
	if got := f.mirrorRow(t, f.link, "op-delivery-1").UnparsedReason; got != string(ReasonUnsupportedType) {
		t.Errorf("the futures delivery's reason is %q, want %q", got, ReasonUnsupportedType)
	}
	// The broker was never asked about a futures contract: the row's own
	// instrument type is outside what this program accounts for, and paying for
	// a passport to be told so would be a call for nothing.
	if calls := f.src.instrumentCalls[uidFutures]; calls != 0 {
		t.Errorf("the broker was asked %d times about the futures instrument, want 0", calls)
	}
}

// TestRebuildTurnsACurrencyTradeIntoBothItsLegs is what a currency purchase
// actually is: money left the account in one currency and arrived in another,
// on the same day. Neither leg alone is true of anything — a single one would
// say the rubles vanished.
//
// The traded currency comes from the broker (CurrencyBy) and from nowhere else:
// the operation row names its own PAYMENT currency, which is the rubles handed
// over, and says nothing about what was bought. The fixture buys 1 000 units at
// 90 ₽, so 90 000 ₽ leave and 1 000 $ arrive.
func TestRebuildTurnsACurrencyTradeIntoBothItsLegs(t *testing.T) {
	f := newRebuildFixture(t)
	f.src.currencyNominals[uidUSDRUB] = MoneyValue{Currency: "usd", Units: 1}
	f.sync(t, f.link, loadOperationItem(t, "currency_buy.json"))

	stats := f.rebuild(t)
	if stats.Added != 2 || stats.Unparsed != 0 {
		t.Fatalf("rebuild added %d and left %d unparsed, want 2 and 0", stats.Added, stats.Unparsed)
	}
	journal := f.journalOf(t, f.accountID)
	if len(journal) != 2 {
		t.Fatalf("journal holds %d operations, want 2", len(journal))
	}
	byCurrency := map[string]operation.Operation{}
	for _, o := range journal {
		if o.Type != operation.TypeConversion {
			t.Fatalf("a currency trade produced a %s — the journal's word for exchanging money is conversion, and the engine skips it whole rather than folding it into a position", o.Type)
		}
		byCurrency[o.Currency] = o
	}
	paid, ok := byCurrency["RUB"]
	if !ok {
		t.Fatalf("no ruble leg among %+v", byCurrency)
	}
	if paid.AmountMinor != -9_000_000 {
		t.Errorf("the ruble leg is %d, want -9000000 (90 000 ₽ left the account)", paid.AmountMinor)
	}
	if paid.FeeMinor != 2_700 {
		t.Errorf("the commission is %d, want 2700 — it was charged in rubles, which is the leg it belongs on", paid.FeeMinor)
	}
	received, ok := byCurrency["USD"]
	if !ok {
		t.Fatalf("no dollar leg among %+v — the traded currency comes from the broker's nominal, and without it the trade says money vanished", byCurrency)
	}
	if received.AmountMinor != 100_000 {
		t.Errorf("the dollar leg is %d, want 100000 (1 000 $ arrived)", received.AmountMinor)
	}
	if received.OccurredOn != paid.OccurredOn {
		t.Errorf("the two legs fall on %s and %s — one exchange happens on one day", received.OccurredOn, paid.OccurredOn)
	}
	// Asked once, and about the currency rather than about a catalog row: a
	// currency trade names no instrument in the journal at all.
	if calls := f.src.currencyNominalCalls[uidUSDRUB]; calls != 1 {
		t.Errorf("the broker was asked %d times for the nominal, want 1", calls)
	}
	if calls := f.src.instrumentCalls[uidUSDRUB]; calls != 0 {
		t.Errorf("the general passport was asked %d times, want 0 — a currency needs no catalog row", calls)
	}
}

// TestRebuildRefusesACurrencyTradeTheBrokerWillNotNameIsTheOtherHalf: the
// broker cannot say what the instrument trades, so the row stays visible with
// its own reason. NOT «this program does not account for that kind of asset» —
// the journal has a type for a conversion and the rule is written — but «the one
// fact needed is missing», which is a different sentence to whoever reads it.
func TestRebuildRefusesACurrencyTradeTheBrokerWillNotName(t *testing.T) {
	f := newRebuildFixture(t)
	// No nominal registered: the broker answers "no such instrument".
	f.sync(t, f.link, loadOperationItem(t, "currency_buy.json"))

	stats := f.rebuild(t)
	if stats.Added != 0 || stats.Unparsed != 1 {
		t.Errorf("rebuild added %d and left %d unparsed, want 0 and 1", stats.Added, stats.Unparsed)
	}
	if got := f.mirrorRow(t, f.link, "op-cur-1").UnparsedReason; got != string(ReasonCurrencyTrade) {
		t.Errorf("the currency purchase's reason is %q, want %q", got, ReasonCurrencyTrade)
	}
	// And nothing half-written: a single leg would say the rubles vanished.
	if journal := f.journalOf(t, f.accountID); len(journal) != 0 {
		t.Errorf("journal holds %d operations, want 0 — one leg alone is not true of anything", len(journal))
	}
}

// TestRebuildOrdersOneDayByTheBrokersClock pins where the order of two
// operations of the same day comes from. The journal keeps a DAY, and the write
// path folds one day's operations in the order they are handed to it, so this
// is the last place the time of day can say anything.
//
// The sale below is the mirror's OLDER row — it was seen first, on a sync that
// refused it — and the purchase that covers it arrives later and is dated
// earlier the same day. Ordered by when the mirror met them, the sale would be
// offered against a position that is not there yet and refused for a reason
// that is not true.
func TestRebuildOrdersOneDayByTheBrokersClock(t *testing.T) {
	f := newRebuildFixture(t)
	sale := loadOperationItem(t, "sell.json") // 2026-05-20T07:05:00Z
	f.sync(t, f.link, sale)
	f.rebuild(t)
	if got := f.mirrorRow(t, f.link, "op-sell-1").UnparsedReason; got != string(ReasonEngineRefused) {
		t.Fatalf("the sale's reason is %q, want %q — this test starts from a sale with nothing behind it", got, ReasonEngineRefused)
	}

	purchase := loadOperationItem(t, "buy.json")
	purchase.Date = time.Date(2026, 5, 20, 6, 0, 0, 0, time.UTC) // the same day, an hour earlier
	// Two more of that same day, so that the assertion below is about an ORDER
	// and not about a coin toss: four operations have twenty-four orders, and
	// only one of them is the broker's.
	dividend := loadOperationItem(t, "dividend.json")
	dividend.Date = time.Date(2026, 5, 20, 8, 0, 0, 0, time.UTC)
	fee := loadOperationItem(t, "service_fee.json")
	fee.Date = time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)
	f.sync(t, f.link, sale, purchase, dividend, fee)

	stats := f.rebuild(t)
	if stats.Added != 4 || stats.Unparsed != 0 {
		t.Errorf("rebuild added %d and left %d unparsed, want 4 and 0 — the purchase of that morning covers the sale", stats.Added, stats.Unparsed)
	}
	journal := f.journalOf(t, f.accountID)
	if len(journal) != 4 {
		t.Fatalf("journal holds %d operations, want 4", len(journal))
	}
	// ListBySource reads in the order the engine folds them, which for one day
	// is the order they were handed over in.
	want := []operation.Type{operation.TypeBuy, operation.TypeSell, operation.TypeDividend, operation.TypeFee}
	for i, typ := range want {
		if journal[i].Type != typ {
			t.Errorf("the journal's operation %d of that day is %s, want %s — the day is ordered by the broker's clock",
				i, journal[i].Type, typ)
		}
	}
}

// TestRebuildAppliesBothEntriesOfOneRowOrNeither is the owner's decision 6:
// half an event in the journal is a lie. A trade whose commission was charged
// in another currency becomes two journal entries, and the batch below treats
// them as two independent candidates — so when the trade is refused and the
// commission is not, the commission must not be left behind.
func TestRebuildAppliesBothEntriesOfOneRowOrNeither(t *testing.T) {
	f := newRebuildFixture(t)
	// The fixture is a purchase; turned into a sale it is a sale of shares the
	// account never held, which the engine refuses — while its commission,
	// charged in another currency, is an ordinary fee the journal would take.
	sale := loadOperationItem(t, "buy_fee_in_another_currency.json")
	sale.Type = "OPERATION_TYPE_SELL"
	sale.Payment = MoneyValue{Currency: "usd", Units: 1200, Nano: 500000000}
	f.sync(t, f.link, loadOperationItem(t, "input.json"), sale)

	stats := f.rebuild(t)
	if stats.Added != 1 {
		t.Errorf("rebuild added %d operations, want 1 — only the top-up survives", stats.Added)
	}
	// The commission WAS written and then taken back, and this run will do it
	// again every hour for as long as the sale is refused. A summary that
	// reported that hour as nothing at all would hide the one piece of work
	// that keeps repeating.
	if stats.Withdrawn != 1 {
		t.Errorf("rebuild reports %d entries written and withdrawn, want 1 — the commission the journal took before refusing the sale",
			stats.Withdrawn)
	}
	if stats.Removed != 0 {
		t.Errorf("rebuild reports %d removed, want 0 — nothing that was in the journal before this rebuild is gone", stats.Removed)
	}
	journal := f.journalOf(t, f.accountID)
	if len(journal) != 1 || journal[0].Type != operation.TypeDeposit {
		for _, o := range journal {
			t.Logf("journal holds %s %d %s", o.Type, o.AmountMinor, o.Currency)
		}
		t.Fatalf("journal holds %d operations, want 1 (the top-up): half of an event was written", len(journal))
	}
	if got := f.mirrorRow(t, f.link, "op-buy-2").UnparsedReason; got != string(ReasonEngineRefused) {
		t.Errorf("the sale's reason is %q, want %q", got, ReasonEngineRefused)
	}
}

// TestRebuildWithdrawsTheHalfOfAnEventItHadLeftInPlace is the case the
// withdrawal above does NOT catch on its own, and the one that makes "whole or
// not at all" a promise rather than a hope.
//
// The two entries of one mirror row are two units of the difference, and they
// need not both be offered: when only ONE of them changes, the other matches
// what the journal already holds and is left alone. If the changed one is then
// refused, the untouched half is still sitting there — money leaving the
// account for a dividend that is not in the journal. Nothing this rebuild
// applied is involved, so a withdrawal that looks only at what it just wrote
// leaves that half in place for ever.
func TestRebuildWithdrawsTheHalfOfAnEventItHadLeftInPlace(t *testing.T) {
	f := newRebuildFixture(t)
	f.sync(t, f.link, loadOperationItem(t, "div_ext.json"))
	if stats := f.rebuild(t); stats.Added != 2 {
		t.Fatalf("the first rebuild added %d entries, want 2 — a dividend paid to a card is income and the money leaving", stats.Added)
	}

	// A rule that changes ONE of that row's two entries and leaves the other
	// exactly as the journal holds it — which is what a corrected instrument
	// match does in real life, since only the income entry names a security.
	// The income entry becomes a move of shares the account never held, which
	// the engine refuses; the withdrawal beside it is not offered at all.
	inner := f.reb.project
	f.reb.project = func(row MirrorRow, accountID uuid.UUID, resolved *Resolved, traded *TradedCurrency) ([]operation.Operation, Deferred, *UnparsedError) {
		ops, deferred, refusal := inner(row, accountID, resolved, traded)
		for i := range ops {
			if ops[i].Type != operation.TypeDividend {
				continue
			}
			qty := decimal.RequireFromString("5")
			ops[i].Type = operation.TypeTransferOut
			ops[i].AmountMinor = 0
			ops[i].Quantity = &qty
		}
		return ops, deferred, refusal
	}

	stats := f.rebuild(t)
	journal := f.journalOf(t, f.accountID)
	if len(journal) != 0 {
		for _, o := range journal {
			t.Logf("journal holds %s %d %s", o.Type, o.AmountMinor, o.Currency)
		}
		t.Fatalf("journal holds %d operations, want 0 — half of an event was left behind", len(journal))
	}
	if got := f.mirrorRow(t, f.link, "op-divext-1").UnparsedReason; got != string(ReasonEngineRefused) {
		t.Errorf("the refused row's reason is %q, want %q", got, ReasonEngineRefused)
	}
	if stats.Added != 0 {
		t.Errorf("rebuild reports %d added, want 0", stats.Added)
	}
	// Both: the income entry the difference asked to replace, and the
	// withdrawal that was in the journal before this rebuild and is not now.
	if stats.Removed != 2 {
		t.Errorf("rebuild reports %d removed, want 2 — the entry the difference replaced and the half it took back", stats.Removed)
	}
}

// TestRebuildResolvesASecurityWhoseTypeTheBrokerDidNotState: the gate in front
// of the resolver reads a set of PASSPORT types, and what an operation row
// carries is not a passport. The broker's own documentation warns that history
// after a corporate action can be incomplete and that identifiers on old
// operations have been rewritten, so a row whose instrument_type is empty —
// and whose instrument_uid the broker will answer about perfectly well — must
// reach the resolver rather than be refused with a type nobody stated.
func TestRebuildResolvesASecurityWhoseTypeTheBrokerDidNotState(t *testing.T) {
	f := newRebuildFixture(t)
	purchase := loadOperationItem(t, "buy.json")
	purchase.InstrumentType = ""
	f.sync(t, f.link, purchase)
	if got := f.mirrorRow(t, f.link, "op-buy-1").InstrumentType; got != "" {
		t.Fatalf("the mirror row's instrument type is %q, want empty — this test proves nothing otherwise", got)
	}

	stats := f.rebuild(t)
	if stats.Added != 1 || stats.Unparsed != 0 {
		t.Errorf("rebuild added %d and left %d unparsed, want 1 and 0 — the passport says it is a share", stats.Added, stats.Unparsed)
	}
	if got := f.mirrorRow(t, f.link, "op-buy-1").UnparsedReason; got != "" {
		t.Errorf("the purchase's reason is %q, want empty", got)
	}
	buy := byExternalID(t, f.journalOf(t, f.accountID), externalIDFor(f.mirrorRow(t, f.link, "op-buy-1"), 1))
	if buy.InstrumentID == nil {
		t.Error("the purchase carries no instrument, want the row the passport resolved to")
	}
	if calls := f.src.instrumentCalls[uidSber]; calls != 1 {
		t.Errorf("the broker was asked %d times about the instrument, want 1", calls)
	}
}

// TestRebuildRefusesOneRowWhenTheBrokerHasNoSuchInstrument: a delisted paper
// the broker will never answer about again must cost the owner one visible
// row. Taking the whole rebuild down over it would mean the connection never
// synced again, for ever — and the broker says "no such instrument" plainly
// enough to tell it from being briefly unreachable (see ErrInstrumentNotFound).
func TestRebuildRefusesOneRowWhenTheBrokerHasNoSuchInstrument(t *testing.T) {
	f := newRebuildFixture(t)
	const brokersWords = "tinvest: InstrumentsService/GetInstrumentBy: status 404: " +
		`{"code":5,"message":"Instrument not found","description":"50002"}`
	f.src.instrumentErrs[uidSber] = fmt.Errorf("%s: %w", brokersWords, ErrInstrumentNotFound)
	f.sync(t, f.link, loadOperationItem(t, "input.json"), loadOperationItem(t, "buy.json"))

	stats := f.rebuild(t)
	if stats.Added != 1 || stats.Unparsed != 1 {
		t.Errorf("rebuild added %d and left %d unparsed, want 1 and 1 — the top-up is not about that paper", stats.Added, stats.Unparsed)
	}
	refused := f.mirrorRow(t, f.link, "op-buy-1")
	if got := refused.UnparsedReason; got != string(ReasonInstrumentUnresolved) {
		t.Errorf("the purchase's reason is %q, want %q — the security was not matched, and that is what happened",
			got, ReasonInstrumentUnresolved)
	}
	// AND WHICH OF THE THREE WAYS IT WAS NOT MATCHED. One code covers a paper
	// the broker has never heard of, a passport too incomplete to file, and a
	// catalog row that carries this ticker for a DIFFERENT security — three
	// faults with three different remedies. The words that tell them apart are
	// the resolver's, and they have to reach the row.
	//
	// The expectation is this test's own input rather than any production
	// wording, so what is pinned is that the sentence travels, not how it reads.
	if !strings.Contains(refused.UnparsedDetail, brokersWords) {
		t.Errorf("the refused row's detail is %q, want it to carry what the resolver said (%q)",
			refused.UnparsedDetail, brokersWords)
	}
	journal := f.journalOf(t, f.accountID)
	if len(journal) != 1 || journal[0].Type != operation.TypeDeposit {
		t.Fatalf("journal = %d rows, want 1 deposit", len(journal))
	}
}

// TestRebuildFailsWhenAPassportCannotBeFetchedAtAll is the boundary of the
// test above. A broker that could not be reached says nothing about the
// instrument, and marking the row "the security was not matched" would blame
// the operation for the network — a mark that would then sit there until
// something happened to rebuild that row again. The run fails instead, and the
// next one, which is likely to succeed, states everything afresh.
func TestRebuildFailsWhenAPassportCannotBeFetchedAtAll(t *testing.T) {
	f := newRebuildFixture(t)
	f.src.instrumentErrs[uidSber] = errors.New("tinvest: InstrumentsService/GetInstrumentBy: request: dial tcp: connection refused")
	f.sync(t, f.link, loadOperationItem(t, "input.json"), loadOperationItem(t, "buy.json"))

	if _, err := f.reb.Rebuild(f.ctx, f.conn, []AccountLink{f.link}, f.src); err == nil {
		t.Fatal("Rebuild succeeded while the broker was unreachable, want the run to fail")
	}
	if got := len(f.journalOf(t, f.accountID)); got != 0 {
		t.Errorf("journal holds %d operations, want 0 — a failed run writes nothing", got)
	}
	if got := f.mirrorRow(t, f.link, "op-buy-1").UnparsedReason; got != "" {
		t.Errorf("the purchase's reason is %q, want empty — a network failure is not news about the operation", got)
	}
}

// TestRebuildLeavesTheReasonOfAnOperationThatDidNotHappen: a row the broker
// cancelled produces no journal entries — and the rebuild says nothing about
// it either way. The reason it already carries is a true statement about the
// row ("this program could not read it"), and withdrawing it because the
// broker changed its mind would drop the row off the owner's list in silence.
func TestRebuildLeavesTheReasonOfAnOperationThatDidNotHappen(t *testing.T) {
	f := newRebuildFixture(t)
	f.sync(t, f.link, loadOperationItem(t, "delivery_buy.json"))
	f.rebuild(t)
	if got := f.mirrorRow(t, f.link, "op-delivery-1").UnparsedReason; got != string(ReasonUnsupportedType) {
		t.Fatalf("reason is %q, want %q — this test starts from an unparsed row", got, ReasonUnsupportedType)
	}

	cancelled := loadOperationItem(t, "delivery_buy.json")
	cancelled.State = "OPERATION_STATE_CANCELED"
	f.sync(t, f.link, cancelled)
	if got := f.mirrorRow(t, f.link, "op-delivery-1").State; got != "OPERATION_STATE_CANCELED" {
		t.Fatalf("the mirror row's state is %q, want it cancelled", got)
	}

	stats := f.rebuild(t)
	if stats.Unparsed != 1 {
		t.Errorf("rebuild reports %d unparsed rows, want 1", stats.Unparsed)
	}
	if got := f.mirrorRow(t, f.link, "op-delivery-1").UnparsedReason; got != string(ReasonUnsupportedType) {
		t.Errorf("reason is %q after the operation was cancelled, want it left as %q", got, ReasonUnsupportedType)
	}
}

// TestRebuildLeavesTheReasonOfAWithdrawnOperation is the same decision for the
// other way a row stops being an operation: the broker stops reporting it. Such
// rows stay on the owner's list of things this program could not read, with
// DisappearedAt beside them to say what became of them — which is
// UnparsedByConnection's own deliberate choice, and a rebuild that withdrew the
// reason would take them off that list in silence.
func TestRebuildLeavesTheReasonOfAWithdrawnOperation(t *testing.T) {
	f := newRebuildFixture(t)
	f.sync(t, f.link, loadOperationItem(t, "delivery_buy.json"))
	f.rebuild(t)
	if got := f.mirrorRow(t, f.link, "op-delivery-1").UnparsedReason; got != string(ReasonUnsupportedType) {
		t.Fatalf("reason is %q, want %q — this test starts from an unparsed row", got, ReasonUnsupportedType)
	}

	f.sync(t, f.link) // the broker's whole history, now empty
	if f.mirrorRow(t, f.link, "op-delivery-1").DisappearedAt == nil {
		t.Fatal("the mirror row was not marked gone, so this test would prove nothing")
	}

	if stats := f.rebuild(t); stats.Unparsed != 1 {
		t.Errorf("rebuild reports %d unparsed rows, want 1", stats.Unparsed)
	}
	if got := f.mirrorRow(t, f.link, "op-delivery-1").UnparsedReason; got != string(ReasonUnsupportedType) {
		t.Errorf("reason is %q after the broker stopped reporting the operation, want it left as %q", got, ReasonUnsupportedType)
	}
}

// TestRebuildProducesNothingFromAnOperationThatDidNotHappen pins the two
// shapes that project to nothing at all: a cancelled order and a row the
// broker has stopped returning.
func TestRebuildProducesNothingFromAnOperationThatDidNotHappen(t *testing.T) {
	f := newRebuildFixture(t)
	f.sync(t, f.link, loadOperationItem(t, "cancelled_buy.json"))

	stats := f.rebuild(t)
	if stats != (RebuildStats{}) {
		t.Errorf("rebuild reported %+v over one cancelled order, want nothing at all", stats)
	}
	if got := len(f.journalOf(t, f.accountID)); got != 0 {
		t.Errorf("journal holds %d operations, want 0", got)
	}
	if got := f.mirrorRow(t, f.link, "op-cancelled-1").UnparsedReason; got != "" {
		t.Errorf("the cancelled order's reason is %q, want empty — a cancelled order is not something this program failed to read", got)
	}
}

// -------------------------------------------------------------------------
// a full redemption closes the position the journal holds
// -------------------------------------------------------------------------

// TestRebuildClosesAFullRedemptionWithThePositionTheJournalHolds is the
// property this whole path exists for. The broker reports a bond's full
// redemption as a payment and nothing else — no count, no price — so the
// number of bonds it retires is the position the journal holds when it
// happens, and only the rebuild can know that.
//
// THE COUNT IS NOWHERE IN THE FIXTURES. Eight bonds were bought and two sold,
// so the redemption closes six — a number no fixture carries and no row of the
// mirror holds. Nor can it be divided out of the money: the payment is 10 000 ₽
// and the bond's nominal is 100 CNY, so payment over nominal answers 100, which
// looks like a count of bonds and is not one.
func TestRebuildClosesAFullRedemptionWithThePositionTheJournalHolds(t *testing.T) {
	f := newRebuildFixture(t)
	f.sync(t, f.link,
		loadOperationItem(t, "bond_buy.json"),
		loadOperationItem(t, "bond_sell.json"),
		loadOperationItem(t, "bond_repayment_full_no_quantity.json"))

	stats := f.rebuild(t)
	if stats.Added != 3 {
		t.Fatalf("rebuild added %d operations, want 3 — the purchase, the sale and the redemption", stats.Added)
	}
	if stats.Unparsed != 0 {
		t.Errorf("rebuild left %d rows unparsed, want 0", stats.Unparsed)
	}
	if got := f.mirrorRow(t, f.link, "op-repay-2").UnparsedReason; got != "" {
		t.Errorf("the redemption's reason is %q, want none", got)
	}

	journal := f.journalOf(t, f.accountID)
	redemption := byExternalID(t, journal, externalIDFor(f.mirrorRow(t, f.link, "op-repay-2"), 1))
	if redemption.Type != operation.TypeRedemption {
		t.Errorf("the redemption is a %s, want redemption", redemption.Type)
	}
	if redemption.Quantity == nil || redemption.Quantity.String() != "6" {
		t.Fatalf("the redemption closes %v bonds, want 6 — eight bought less two sold", redemption.Quantity)
	}
	if redemption.AmountMinor != 1_000_000 {
		t.Errorf("the redemption's amount is %d, want 1000000 — the broker's own payment", redemption.AmountMinor)
	}
	if !redemption.OccurredOn.Equal(day(t, "2026-06-03")) {
		t.Errorf("the redemption's day is %s, want 2026-06-03", redemption.OccurredOn.Format("2006-01-02"))
	}

	// What the owner sees afterwards, which is the whole complaint this fixes:
	// a redeemed bond that stays in the journal shows up as a position that no
	// longer exists at the broker.
	positions, err := portfolio.Compute(mustListForEngine(t, f, f.accountID))
	if err != nil {
		t.Fatalf("the journal that was written does not replay when read back: %v", err)
	}
	if len(positions) != 1 {
		t.Fatalf("the account holds %d positions, want 1", len(positions))
	}
	bond := positions[*redemption.InstrumentID]
	if bond == nil {
		t.Fatalf("the account holds no position in the redeemed bond at all")
	}
	if !bond.Quantity.IsZero() {
		t.Errorf("the bond's position is %s after it was redeemed, want 0", bond.Quantity)
	}

	// Idempotence, on the entry whose number this rebuild worked out for
	// itself: the count is derived from the same rows in the same order every
	// time, so a rebuild over an unchanged mirror must ask for nothing.
	mark := len(f.applier.deltas)
	if second := f.rebuild(t); second != (RebuildStats{}) {
		t.Errorf("the second rebuild reported %+v, want a rebuild that changed nothing", second)
	}
	for i, d := range f.deltasSince(mark) {
		if len(d.Add) != 0 || len(d.Remove) != 0 {
			t.Errorf("the second rebuild's delta %d asks to add %d and remove %d, want an empty delta",
				i, len(d.Add), len(d.Remove))
		}
	}
	after := f.journalOf(t, f.accountID)
	if len(after) != len(journal) {
		t.Fatalf("journal holds %d operations after the second rebuild, want %d", len(after), len(journal))
	}
	for i := range journal {
		if journal[i].ID != after[i].ID {
			t.Errorf("operation %d is row %s after the second rebuild and was %s before", i, after[i].ID, journal[i].ID)
		}
	}
}

// TestRebuildLeavesARedemptionOfNothingUnparsed is the honest refusal beside
// the rule above. When the journal holds none of the bond — the purchase is
// older than the window this connection imports, or stayed unparsed for a
// reason of its own — there is no count to take, and the alternatives are a
// sale of nothing and a number this program made up.
//
// THE REASON MUST BE THE NEW ONE. "The broker named no quantity" and "this
// program's journal has nothing to close" are different faults with different
// things to go and look at, and this repository has been caught four times
// printing a true figure under a false cause.
func TestRebuildLeavesARedemptionOfNothingUnparsed(t *testing.T) {
	f := newRebuildFixture(t)
	f.sync(t, f.link, loadOperationItem(t, "bond_repayment_full_no_quantity.json"))

	stats := f.rebuild(t)
	if stats.Added != 0 {
		t.Errorf("rebuild added %d operations, want 0", stats.Added)
	}
	if stats.Unparsed != 1 {
		t.Errorf("rebuild reports %d unparsed rows, want 1", stats.Unparsed)
	}
	if got := len(f.journalOf(t, f.accountID)); got != 0 {
		t.Fatalf("journal holds %d operations, want 0 — there was nothing to redeem", got)
	}
	row := f.mirrorRow(t, f.link, "op-repay-2")
	if row.UnparsedReason != "redemption_nothing_held" {
		t.Errorf("the redemption's reason is %q, want redemption_nothing_held", row.UnparsedReason)
	}
	if row.UnparsedDetail == "" {
		t.Error("the redemption's detail is empty: the code says what kind of fault it is, the detail says which row")
	}
}

// TestRebuildStopsRatherThanCountAPositionItCannotRead pins the one thing
// worse than refusing: closing a redemption with a number counted through an
// entry whose effect on the position this rebuild cannot state. A split
// multiplies a position instead of adding to it, and no shape in this package
// produces one — so this is reachable only from a change to this program, and
// what it must do then is stop rather than guess.
func TestRebuildStopsRatherThanCountAPositionItCannotRead(t *testing.T) {
	f := newRebuildFixture(t)
	f.sync(t, f.link,
		loadOperationItem(t, "bond_buy.json"),
		loadOperationItem(t, "bond_sell.json"),
		loadOperationItem(t, "bond_repayment_full_no_quantity.json"))

	// A rule that turns the sale of two bonds into a split of the same bond —
	// the shape a later change to this package could add.
	inner := f.reb.project
	f.reb.project = func(row MirrorRow, accountID uuid.UUID, resolved *Resolved, traded *TradedCurrency) ([]operation.Operation, Deferred, *UnparsedError) {
		ops, deferred, refusal := inner(row, accountID, resolved, traded)
		for i := range ops {
			if ops[i].Type != operation.TypeSell || ops[i].Quantity == nil {
				continue
			}
			ratio := decimal.RequireFromString("2")
			ops[i].Type = operation.TypeSplit
			ops[i].SplitRatio = &ratio
			ops[i].AmountMinor = 0
		}
		return ops, deferred, refusal
	}

	_, err := f.reb.Rebuild(f.ctx, f.conn, []AccountLink{f.link}, f.src)
	if !errors.Is(err, errHoldingUnreadable) {
		t.Fatalf("Rebuild returned %v, want errHoldingUnreadable", err)
	}
	if got := len(f.journalOf(t, f.accountID)); got != 0 {
		t.Errorf("journal holds %d operations, want 0 — the rebuild stopped before it wrote anything", got)
	}
}

// TestUnitsMovedAgreesWithTheEngine is what keeps this file's second statement
// of the engine's arithmetic from drifting from the engine's own. The journal
// below is folded by portfolio.Compute — the code every screen and every tax
// figure comes from — and the position it hands back is compared with the sum
// of unitsMoved over the very same entries.
//
// It is a differential test and it is not blind: the two sides share no code
// at all, so a sign or a type dropped from unitsMoved moves one and not the
// other. The literal at the end is written out rather than derived for the
// same reason.
func TestUnitsMovedAgreesWithTheEngine(t *testing.T) {
	instrumentID := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	accountID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	qty := func(s string) *decimal.Decimal {
		d := decimal.RequireFromString(s)
		return &d
	}
	entry := func(typ operation.Type, on string, amount int64, quantity *decimal.Decimal) operation.Operation {
		return operation.Operation{
			AccountID: accountID, InstrumentID: &instrumentID, Type: typ,
			OccurredOn: day(t, on), AmountMinor: amount, Quantity: quantity,
			Currency: "RUB", Source: Source,
		}
	}
	ops := []operation.Operation{
		entry(operation.TypeBuy, "2026-01-10", -1_000_000, qty("100")),
		entry(operation.TypeDividend, "2026-02-01", 50_000, nil),
		entry(operation.TypeCoupon, "2026-02-02", 30_000, nil),
		entry(operation.TypeTax, "2026-02-03", -10_000, nil),
		entry(operation.TypeFee, "2026-02-04", -5_000, nil),
		entry(operation.TypeAmortization, "2026-03-01", 20_000, nil),
		entry(operation.TypeSell, "2026-04-01", 400_000, qty("30")),
		entry(operation.TypeTransferIn, "2026-05-01", 50_000, qty("5")),
		entry(operation.TypeTransferOut, "2026-06-01", 0, qty("2")),
	}

	positions, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("the engine refused this journal, so it cannot be compared against: %v", err)
	}
	held := positions[instrumentID]
	if held == nil {
		t.Fatalf("the engine returned no position for the instrument every entry names")
	}

	sum := decimal.Zero
	for _, o := range ops {
		moved, readable := unitsMoved(o)
		if !readable {
			t.Fatalf("unitsMoved cannot read a %s, which the engine folds into a position", o.Type)
		}
		sum = sum.Add(moved)
	}
	if !sum.Equal(held.Quantity) {
		t.Errorf("unitsMoved sums to %s and the engine holds %s", sum, held.Quantity)
	}
	if !sum.Equal(decimal.RequireFromString("73")) {
		t.Errorf("unitsMoved sums to %s, want 73 — bought 100, sold 30, five arrived, two left", sum)
	}
}

// -------------------------------------------------------------------------
// changing the rule
// -------------------------------------------------------------------------

// TestRebuildChangesTheJournalWhenTheRuleChanges is what the mirror is FOR: a
// projection rule that is corrected must be able to reach the whole history
// again without the broker being asked for any of it a second time.
func TestRebuildChangesTheJournalWhenTheRuleChanges(t *testing.T) {
	f := newRebuildFixture(t)
	f.sync(t, f.link, loadOperationItem(t, "input.json"), loadOperationItem(t, "buy.json"))
	f.rebuild(t)
	callsAfterFirst := f.src.instrumentCalls[uidSber]
	if callsAfterFirst == 0 {
		t.Fatal("the broker was never asked about the instrument, so the count below would prove nothing")
	}

	// A new rule: every top-up is recorded with a note of its own. Nothing else
	// about the run changes — no mirror row, no broker call.
	inner := f.reb.project
	f.reb.project = func(row MirrorRow, accountID uuid.UUID, resolved *Resolved, traded *TradedCurrency) ([]operation.Operation, Deferred, *UnparsedError) {
		ops, deferred, refusal := inner(row, accountID, resolved, traded)
		for i := range ops {
			if ops[i].Type == operation.TypeDeposit {
				ops[i].Note = "переведено по новому правилу"
			}
		}
		return ops, deferred, refusal
	}

	stats := f.rebuild(t)
	if stats.Added != 1 || stats.Removed != 1 {
		t.Errorf("rebuild added %d and removed %d, want 1 and 1 — only the top-up's rule changed", stats.Added, stats.Removed)
	}
	deposit := byExternalID(t, f.journalOf(t, f.accountID), externalIDFor(f.mirrorRow(t, f.link, "op-input-1"), 1))
	if deposit.Note != "переведено по новому правилу" {
		t.Errorf("the top-up's note is %q, want the new rule's", deposit.Note)
	}
	if got := f.src.instrumentCalls[uidSber]; got != callsAfterFirst {
		t.Errorf("the broker was asked about the instrument %d times, want the %d of the first rebuild — a rule change is rebuilt from the mirror alone",
			got, callsAfterFirst)
	}
}

// -------------------------------------------------------------------------
// what the rebuild refuses to be asked
// -------------------------------------------------------------------------

func TestRebuildRefusesALinkOfAnotherConnection(t *testing.T) {
	f := newRebuildFixture(t)
	stranger := f.link
	stranger.ConnectionID = uuid.New()

	if _, err := f.reb.Rebuild(f.ctx, f.conn, []AccountLink{stranger}, f.src); !errors.Is(err, ErrLinkNotInConnection) {
		t.Errorf("Rebuild with a foreign link = %v, want ErrLinkNotInConnection", err)
	}
}

// TestRebuildRefusesALinkOfAnotherSpace is the second half of that guard. A
// link naming a space other than the connection's would have this rebuild
// compute a difference against one space's journal and hand it to another's,
// and the write path — which is told the space by the CONNECTION — would
// remove rows it was never shown.
func TestRebuildRefusesALinkOfAnotherSpace(t *testing.T) {
	f := newRebuildFixture(t)
	stranger := f.link
	stranger.SpaceID = uuid.New()

	if _, err := f.reb.Rebuild(f.ctx, f.conn, []AccountLink{stranger}, f.src); !errors.Is(err, ErrLinkOutsideSpace) {
		t.Errorf("Rebuild with a link of another space = %v, want ErrLinkOutsideSpace", err)
	}
}

// -------------------------------------------------------------------------
// the comparison itself
// -------------------------------------------------------------------------

// comparedField is one change to a journal row that sameJournalRow has to
// notice. field names the struct field of operation.Operation it touches,
// which is what makes the list checkable against the type itself (see
// TestSameJournalRowLooksAtEveryFieldAnOperationHas) rather than against
// somebody's memory of what the type holds.
type comparedField struct {
	field  string
	label  string
	mutate func(*operation.Operation)
}

func comparedFields(t *testing.T) []comparedField {
	t.Helper()
	instrumentB := uuid.MustParse("44444444-4444-4444-8444-444444444444")
	groupA := uuid.MustParse("55555555-5555-4555-8555-555555555555")
	settled := day(t, "2026-03-16")
	return []comparedField{
		{"AccountID", "account", func(o *operation.Operation) { o.AccountID = uuid.New() }},
		{"InstrumentID", "instrument", func(o *operation.Operation) { o.InstrumentID = &instrumentB }},
		{"InstrumentID", "instrument dropped", func(o *operation.Operation) { o.InstrumentID = nil }},
		{"Type", "type", func(o *operation.Operation) { o.Type = operation.TypeSell }},
		{"OccurredOn", "day", func(o *operation.Operation) { o.OccurredOn = day(t, "2026-03-16") }},
		{"SettledOn", "settlement day", func(o *operation.Operation) { o.SettledOn = &settled }},
		{"Quantity", "quantity", func(o *operation.Operation) { q := decimal.RequireFromString("11"); o.Quantity = &q }},
		{"Quantity", "quantity dropped", func(o *operation.Operation) { o.Quantity = nil }},
		{"Price", "price", func(o *operation.Operation) { p := decimal.RequireFromString("276"); o.Price = &p }},
		{"Price", "price dropped", func(o *operation.Operation) { o.Price = nil }},
		{"AmountMinor", "amount", func(o *operation.Operation) { o.AmountMinor = -2_750_001 }},
		{"Currency", "currency", func(o *operation.Operation) { o.Currency = "USD" }},
		{"FeeMinor", "fee", func(o *operation.Operation) { o.FeeMinor = 826 }},
		{"Note", "note", func(o *operation.Operation) { o.Note = "что-то другое" }},
		{"Source", "source", func(o *operation.Operation) { o.Source = "csv" }},
		{"TransferGroupID", "transfer group", func(o *operation.Operation) { o.TransferGroupID = &groupA }},
		{"SplitRatio", "split ratio", func(o *operation.Operation) { r := decimal.RequireFromString("2"); o.SplitRatio = &r }},
	}
}

// notComparedFields is every field of an operation that sameJournalRow
// deliberately does NOT look at, each with the reason. A field is in here or
// in comparedFields, never in neither and never in both — which is what the
// reflection test below enforces.
var notComparedFields = map[string]string{
	"ID": "the journal's own identity, invented when the row was written; " +
		"the projection never has one to compare",
	"SpaceID": "the journal's own: a delta is applied to the space the CONNECTION names, " +
		"and the projection never states it",
	"ExternalID": "what the two rows were matched BY, so it is equal by construction",
	"CreatedAt": "the journal's own numbering of the row within its day, " +
		"assigned by the write path; the projection never has one",
	"TransferLots": "the parcel the write path released from the source account's history — " +
		"a property of the journal, and the projection never has it (operation.checkImportContract " +
		"refuses one supplied)",
}

func sameJournalRowBase(t *testing.T) operation.Operation {
	t.Helper()
	instrumentA := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	qty := decimal.RequireFromString("10")
	price := decimal.RequireFromString("275")
	return operation.Operation{
		AccountID:    uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		InstrumentID: &instrumentA,
		Type:         operation.TypeBuy,
		OccurredOn:   day(t, "2026-03-15"),
		Quantity:     &qty,
		Price:        &price,
		AmountMinor:  -2_750_000,
		Currency:     "RUB",
		FeeMinor:     825,
		Note:         "Покупка 100 шт.",
		Source:       Source,
	}
}

// TestSameJournalRowNoticesEveryFieldItCompares walks the fields one at a time.
// A comparison that quietly stopped looking at one of them would leave the
// journal holding a value the mirror no longer says, and nothing anywhere
// would report a difference.
func TestSameJournalRowNoticesEveryFieldItCompares(t *testing.T) {
	base := sameJournalRowBase(t)
	if !sameJournalRow(base, base) {
		t.Fatal("a row differs from itself")
	}
	for _, c := range comparedFields(t) {
		changed := base
		c.mutate(&changed)
		if sameJournalRow(base, changed) {
			t.Errorf("a row whose %s changed reads as unchanged", c.label)
		}
		if sameJournalRow(changed, base) {
			t.Errorf("a row whose %s changed reads as unchanged the other way round", c.label)
		}
	}
}

// TestSameJournalRowLooksAtEveryFieldAnOperationHas is what makes the promise
// in sameJournalRow's own documentation — "every column the projection could
// set is compared" — checkable rather than merely stated.
//
// Both the comparison and the table above it are written and maintained BY
// HAND. A field added to operation.Operation tomorrow would slip out of both
// in the same silence: the mirror would stop being able to correct it, and a
// broker's correction would stop reaching the journal with nothing anywhere
// saying so. So the fields are read off the type itself, and every one of them
// must be either exercised by a case above or named in notComparedFields with
// the reason it is not.
func TestSameJournalRowLooksAtEveryFieldAnOperationHas(t *testing.T) {
	covered := map[string]bool{}
	for _, c := range comparedFields(t) {
		covered[c.field] = true
	}

	typ := reflect.TypeOf(operation.Operation{})
	onTheType := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		onTheType[name] = true
		_, excused := notComparedFields[name]
		switch {
		case covered[name] && excused:
			t.Errorf("operation.Operation.%s is both compared and listed as deliberately not compared — one of the two lists is wrong", name)
		case !covered[name] && !excused:
			t.Errorf("operation.Operation.%s is neither compared by sameJournalRow nor listed in notComparedFields: "+
				"a rebuild cannot correct what it does not compare, so add a case for it or say in notComparedFields why the journal owns it", name)
		}
	}
	for name := range covered {
		if !onTheType[name] {
			t.Errorf("a comparison case names field %q, which operation.Operation does not have", name)
		}
	}
	for name := range notComparedFields {
		if !onTheType[name] {
			t.Errorf("notComparedFields names field %q, which operation.Operation does not have", name)
		}
	}
}

// TestSameJournalRowIgnoresTheBasisTheJournalOwns pins the ONE exclusion, and
// pins its boundary. The basis of a departing leg and of a paired arrival is
// worked out by the write path from the source account's own history — the
// projection is forbidden from supplying it (operation.checkImportContract) —
// so comparing the zero it hands over against the figure the journal computed
// would report a difference on every rebuild, for ever. A LONE arrival is the
// other case: it declares its own basis, so a change in it is a real change.
func TestSameJournalRowIgnoresTheBasisTheJournalOwns(t *testing.T) {
	instrumentA := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	group := uuid.MustParse("55555555-5555-4555-8555-555555555555")
	qty := decimal.RequireFromString("5")

	moved := day(t, "2026-05-05")
	leg := func(typ operation.Type, groupID *uuid.UUID, amount int64) operation.Operation {
		return operation.Operation{
			AccountID:    uuid.MustParse("22222222-2222-4222-8222-222222222222"),
			InstrumentID: &instrumentA, Type: typ, OccurredOn: moved,
			Quantity: &qty, AmountMinor: amount, Currency: "RUB", Source: Source,
			TransferGroupID: groupID,
		}
	}
	if !sameJournalRow(leg(operation.TypeTransferOut, &group, 0), leg(operation.TypeTransferOut, &group, 137_500)) {
		t.Error("a departing leg reads as changed when the journal fills in the basis it owns")
	}
	if !sameJournalRow(leg(operation.TypeTransferIn, &group, 0), leg(operation.TypeTransferIn, &group, 137_500)) {
		t.Error("a paired arrival reads as changed when the journal fills in the basis it owns")
	}
	if !sameJournalRow(leg(operation.TypeTransferOut, nil, 0), leg(operation.TypeTransferOut, nil, 137_500)) {
		t.Error("a lone departing leg reads as changed when the journal fills in the basis it owns")
	}
	if sameJournalRow(leg(operation.TypeTransferIn, nil, 0), leg(operation.TypeTransferIn, nil, 137_500)) {
		t.Error("a lone arrival's basis is its own and a change in it must be seen")
	}
}

// realizedOf is a position's realized result, and it FAILS THE TEST when the
// position has none — a disposal that settled in another currency leaves no
// figure in any single one (see portfolio.Position.RealizedPnL). Every call
// below therefore asserts two things at once: the number, and that there is a
// number, which is what keeps a test from quietly comparing a zero against a
// zero the moment the currency rule starts refusing to answer.
func realizedOf(t *testing.T, p *portfolio.Position) int64 {
	t.Helper()
	minor, inOneCurrency := p.RealizedPnL()
	if !inOneCurrency {
		t.Fatalf("position %s has no realized result in one currency: a disposal settled in another", p.InstrumentID)
	}
	return minor
}

// -------------------------------------------------------------------------
// a commission charged as an operation of its own (#138)
// -------------------------------------------------------------------------

// TestRebuildDropsABrokerFeeTheTradeAlreadyCarries is the ordinary case, and
// the one the old blanket rule got right 310 times out of 311: the trade
// reports the commission in its own field, the broker reports it a second time
// as an operation, and booking both would charge it twice.
//
// The mirror row is left CLEAN rather than marked unparsed — nothing failed to
// be read.
func TestRebuildDropsABrokerFeeTheTradeAlreadyCarries(t *testing.T) {
	f := newRebuildFixture(t)
	f.sync(t, f.link,
		loadOperationItem(t, "buy.json"),
		loadOperationItem(t, "broker_fee.json"))

	stats := f.rebuild(t)
	if stats.Added != 1 {
		t.Errorf("rebuild added %d entries, want 1 — the purchase alone", stats.Added)
	}
	if stats.Unparsed != 0 {
		t.Errorf("rebuild left %d unparsed, want 0 — a duplicate is understood, not unreadable", stats.Unparsed)
	}
	ops := f.journalOf(t, f.link.AccountID)
	if len(ops) != 1 {
		t.Fatalf("journal holds %d entries, want 1: %+v", len(ops), ops)
	}
	// 8,25 ₽ once, on the purchase — not twice, and not as an entry of its own.
	if ops[0].Type != operation.TypeBuy || ops[0].FeeMinor != 825 {
		t.Errorf("entry = %s with fee %d, want a buy carrying 825", ops[0].Type, ops[0].FeeMinor)
	}
	if got := f.mirrorRow(t, f.link, "op-brokerfee-1").UnparsedReason; got != "" {
		t.Errorf("the dropped fee's reason is %q, want none", got)
	}
}

// TestRebuildKeepsABrokerFeeItsTradeDoesNotCarry is the 311th, and the reason
// the rule is no longer "drop them all".
//
// The purchase carries no commission field, so the separate operation is the
// only record of that 11,34 ₽. Dropping it put the charge in neither the
// journal nor the unparsed list — money gone with nothing on any screen saying
// so, which is the one outcome this program forbids in capitals.
func TestRebuildKeepsABrokerFeeItsTradeDoesNotCarry(t *testing.T) {
	f := newRebuildFixture(t)
	f.sync(t, f.link,
		loadOperationItem(t, "buy_without_commission.json"),
		loadOperationItem(t, "broker_fee_without_parent_commission.json"))

	stats := f.rebuild(t)
	if stats.Added != 2 {
		t.Errorf("rebuild added %d entries, want 2 — the purchase and the charge beside it", stats.Added)
	}
	ops := f.journalOf(t, f.link.AccountID)
	if len(ops) != 2 {
		t.Fatalf("journal holds %d entries, want 2: %+v", len(ops), ops)
	}
	var fee *operation.Operation
	for i := range ops {
		if ops[i].Type == operation.TypeFee {
			fee = &ops[i]
		}
	}
	if fee == nil {
		t.Fatalf("no fee entry in the journal: %+v", ops)
	}
	// Negative, because a fee entry's amount is what left the account; the
	// engine turns it into a positive charge.
	if fee.AmountMinor != -1134 {
		t.Errorf("fee amount = %d, want -1134 (11,34 ₽ charged)", fee.AmountMinor)
	}
	if got := f.mirrorRow(t, f.link, "op-brokerfee-2").UnparsedReason; got != "" {
		t.Errorf("the kept fee's reason is %q, want none", got)
	}
}

// TestRebuildRefusesABrokerFeeWhoseTradeIsNotHere. With the trade absent, both
// answers are guesses that cost money in opposite directions: dropping the fee
// loses a real charge, keeping it books a commission twice. The row becomes a
// visible unparsed entry saying exactly that, and the detail names the trade so
// the owner can look it up in the broker's own app.
func TestRebuildRefusesABrokerFeeWhoseTradeIsNotHere(t *testing.T) {
	f := newRebuildFixture(t)
	f.sync(t, f.link, loadOperationItem(t, "broker_fee_orphan.json"))

	stats := f.rebuild(t)
	if stats.Added != 0 {
		t.Errorf("rebuild added %d entries, want 0", stats.Added)
	}
	if stats.Unparsed != 1 {
		t.Errorf("rebuild left %d unparsed, want 1", stats.Unparsed)
	}
	row := f.mirrorRow(t, f.link, "op-brokerfee-3")
	if row.UnparsedReason != string(ReasonBrokerFeeParentMissing) {
		t.Errorf("reason = %q, want %q", row.UnparsedReason, ReasonBrokerFeeParentMissing)
	}
	if !strings.Contains(row.UnparsedDetail, "op-that-is-not-here") {
		t.Errorf("detail = %q, want it to name the trade the fee points at", row.UnparsedDetail)
	}
	if len(f.journalOf(t, f.link.AccountID)) != 0 {
		t.Errorf("the journal took an entry for a fee nothing could judge")
	}
}

// TestRebuildDropsABrokerFeeWhoseTradeIsItselfUnparsed is a regression the
// live data caught and the fixtures did not.
//
// A currency purchase the broker will not explain is not imported — it answers
// nothing about what the pair trades or what one unit of it is — so the trade
// sits on the unparsed list with its own reason. Its commission then has no commission in the journal to duplicate,
// and the rule as first written kept it: 79 of the owner's currency trades each
// grew a SECOND unparsed row, saying nothing the first did not, and saying it
// under a reason about the instrument rather than about the trade.
//
// A commission belongs where its trade went.
func TestRebuildDropsABrokerFeeWhoseTradeIsItselfUnparsed(t *testing.T) {
	f := newRebuildFixture(t)
	trade := loadOperationItem(t, "currency_buy.json")
	fee := loadOperationItem(t, "broker_fee.json")
	fee.ParentOperationID = trade.ID
	f.sync(t, f.link, trade, fee)

	stats := f.rebuild(t)
	if stats.Added != 0 {
		t.Errorf("rebuild added %d entries, want 0", stats.Added)
	}
	// One row on the list, not two: the trade's.
	if stats.Unparsed != 1 {
		t.Errorf("rebuild left %d rows unparsed, want 1 — the trade, and not its commission as well", stats.Unparsed)
	}
	if got := f.mirrorRow(t, f.link, trade.ID).UnparsedReason; got != string(ReasonCurrencyTrade) {
		t.Errorf("the trade's reason is %q, want %q", got, ReasonCurrencyTrade)
	}
	if got := f.mirrorRow(t, f.link, fee.ID).UnparsedReason; got != "" {
		t.Errorf("the commission carries reason %q, want none: its trade is already reported", got)
	}
}

// TestRebuildChargesABrokerFeeToTheAccountNotToAPosition. The security named on
// a commission row is the TRADE's; the commission itself is money off the
// account. Reading it as the fee's own security refused every commission on a
// paper this program does not account for — and attributed the rest to a
// position, which is not what a broker's commission is.
func TestRebuildChargesABrokerFeeToTheAccountNotToAPosition(t *testing.T) {
	f := newRebuildFixture(t)
	trade := loadOperationItem(t, "buy_without_commission.json")
	fee := loadOperationItem(t, "broker_fee_without_parent_commission.json")
	// The broker really does put the trade's security on the fee row.
	fee.InstrumentUID = trade.InstrumentUID
	fee.FIGI = trade.FIGI
	fee.InstrumentType = trade.InstrumentType
	f.sync(t, f.link, trade, fee)

	f.rebuild(t)

	ops := f.journalOf(t, f.link.AccountID)
	var charge *operation.Operation
	for i := range ops {
		if ops[i].Type == operation.TypeFee {
			charge = &ops[i]
		}
	}
	if charge == nil {
		t.Fatalf("no fee entry in the journal: %+v", ops)
	}
	if charge.InstrumentID != nil {
		t.Errorf("the charge names instrument %s, want none: a commission is money off the account", charge.InstrumentID)
	}
	if charge.AmountMinor != -1134 {
		t.Errorf("charge = %d, want -1134", charge.AmountMinor)
	}
}

// TestRebuildWorksOutAForgottenCurrencyPairFromItsName is the owner's two dozen
// unparsed rows, end to end: dollar and euro pairs the broker delisted and now
// answers 404 for, so nothing could say what a trade in them bought.
//
// The wiring is what this test is for — the pair's name and the trade's own
// price have to travel from the mirror row into the resolver, where the official
// rate turns a guess into a fact.
func TestRebuildWorksOutAForgottenCurrencyPairFromItsName(t *testing.T) {
	f := newRebuildFixture(t)
	// The rate the central bank published that day. The fixture's trade bought
	// at 90, which is the same money.
	f.rates.byCode["USD"] = decimal.RequireFromString("89.50")

	item := loadOperationItem(t, "currency_buy.json")
	item.InstrumentUID = "uid-usd-delisted"
	item.Ticker = "USD000UTSTOM"
	f.sync(t, f.link, item)
	// The broker knows nothing about this pair any more, which is the whole
	// premise: newFakePassportSource registers no nominal for that uid.

	stats := f.rebuild(t)
	if stats.Unparsed != 0 {
		t.Fatalf("left %d rows unparsed, want none: the pair's own name says USD and the official rate agrees with the price it traded at", stats.Unparsed)
	}
	journal := f.journalOf(t, f.accountID)
	var currencies []string
	for _, op := range journal {
		if op.Type == operation.TypeConversion {
			currencies = append(currencies, op.Currency)
		}
	}
	if len(currencies) != 2 {
		t.Fatalf("journal holds %+v, want a conversion's two legs", journal)
	}
	// One leg is the rubles that left, the other the dollars that arrived.
	var hasRUB, hasUSD bool
	for _, c := range currencies {
		hasRUB = hasRUB || c == "RUB"
		hasUSD = hasUSD || c == "USD"
	}
	if !hasRUB || !hasUSD {
		t.Errorf("the conversion's legs are %v, want RUB and USD", currencies)
	}
}

// TestRebuildLeavesAForgottenPairUnparsedWithoutARate is the same row with the
// proof missing. A name alone settles nothing — that is the whole design — so
// until the rate table reaches that day the trade stays exactly as unparsed as
// it was.
func TestRebuildLeavesAForgottenPairUnparsedWithoutARate(t *testing.T) {
	f := newRebuildFixture(t)

	item := loadOperationItem(t, "currency_buy.json")
	item.InstrumentUID = "uid-usd-delisted"
	item.Ticker = "USD000UTSTOM"
	f.sync(t, f.link, item)

	stats := f.rebuild(t)
	if stats.Unparsed != 1 {
		t.Fatalf("left %d rows unparsed, want 1: with no official rate to check against, the name is a guess", stats.Unparsed)
	}
}
