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

// These tests use a real database and the real operation.Service, so every rule
// of the write path (engine replay, FIFO release, per-type validation) is
// exercised. Expected values are literals, so they do not move with the
// implementation.

const (
	// The instrument_uids the testdata/ops fixtures carry.
	uidSber    = "e6123145-9665-43e0-8413-cd61b8aa9b13"
	uidAAPL    = "9654c2dd-6993-427e-80fa-04e80a1cf4da"
	uidFutures = "b0e1a5f2-1a3c-4f0e-9b7a-9d6a6f4c1a11"
	uidUSDRUB  = "a22a1263-8e1b-4546-a1aa-416463f104d3"
	uidBond    = "00e0a5a6-3f9a-4b26-bdb9-95cbbaa1c0f2"
)

// recordingDelta wraps the real operation.Service and remembers every delta, so
// a test can say "the second rebuild asked for nothing", which a delete-and-rewrite
// rebuild would fail while leaving the same journal.
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
	// No rates by default; a test that wants the forgotten-pair fallback adds
	// one (see fakeRates).
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
	// Answers about a futures contract and a currency, both unusable, so a
	// rebuild that asks about them fails on the call count.
	src.instruments[uidFutures] = InstrumentBrief{
		UID: uidFutures, FIGI: "FUTSI0323000", Ticker: "SiH3",
		Name: "Фьючерс USD/RUB", Currency: "RUB", InstrumentType: "futures",
	}
	src.instruments[uidUSDRUB] = InstrumentBrief{
		UID: uidUSDRUB, FIGI: "BBG0013HGFT4", Ticker: "USDRUB_TOM",
		Name: "Доллар США", Currency: "RUB", InstrumentType: "currency",
	}
	// The redeemed bond, from the owner's account: yuan-denominated, traded
	// and redeemed in roubles, which is why its count cannot be divided out of
	// the payment.
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

// sync puts one broker account's whole history into the mirror through the
// real SyncMirror.
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

// journalOf reads one account's imported operations back from the database.
func (f *rebuildFixture) journalOf(t *testing.T, accountID uuid.UUID) []operation.Operation {
	t.Helper()
	ops, err := f.ops.ListBySource(f.ctx, f.spaceID, accountID, Source)
	if err != nil {
		t.Fatalf("ListBySource: %v", err)
	}
	return ops
}

// mustListForEngine reads one account's journal through the engine's listing,
// not from what a rebuild returned.
func mustListForEngine(t *testing.T, f *rebuildFixture, accountID uuid.UUID) []operation.Operation {
	t.Helper()
	ops, err := f.ops.ListForEngine(f.ctx, f.spaceID, accountID)
	if err != nil {
		t.Fatalf("ListForEngine: %v", err)
	}
	return ops
}

// realizedOf folds an account's journal through portfolio.Compute and sums
// realized profit, the figure screens and taxes come from.
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

// mirrorVersions is each mirror row's xmin, so a test can tell that a rebuild
// wrote nothing: rewriting a value with itself still makes a new row version.
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

// externalIDFor is the projection's name for a row's n-th (1-based) entry,
// spelled out so a change to the scheme reddens these tests.
func externalIDFor(row MirrorRow, leg int) string {
	return row.ID.String() + "/" + strconv.Itoa(leg)
}

// Idempotence.

// The assertion is on the delta: a rebuild over an unchanged mirror must ask
// for nothing. A rebuild that rewrote everything would leave the right rows but
// renumber them.
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
	// Each entry carries the broker's instant: within a day the journal folds
	// by it (#198).
	for _, ext := range []string{"op-input-1", "op-buy-1", "op-div-1"} {
		row := f.mirrorRow(t, f.link, ext)
		entry := byExternalID(t, before, externalIDFor(row, 1))
		if entry.OccurredAt == nil || !entry.OccurredAt.Equal(row.OccurredAt) {
			t.Errorf("%s: occurred_at = %v, want the broker's %s", ext, entry.OccurredAt, row.OccurredAt)
		}
	}

	mark := len(f.applier.deltas)
	versions := f.mirrorVersions(t)
	second := f.rebuild(t)
	if second != (RebuildStats{}) {
		t.Errorf("second rebuild reported %+v, want a rebuild that changed nothing", second)
	}
	versionsNow := f.mirrorVersions(t)
	// Count first: the loop below would not notice a deleted row.
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

// The ticker the operation carries reaches the resolver: a wound-up fund's
// passport answers 404, the broker puts its ISIN in the ticker field, and the
// catalog knows the paper by that ISIN, so the operation lands in the journal.
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
	// The operation's figi differs from the catalog's (reissued per listing);
	// only the ISIN joins them.
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

// The difference is by external id but the comparison is by value: a commission
// the broker corrected reaches the journal.
func TestRebuildRewritesAnOperationWhoseMirrorRowChanged(t *testing.T) {
	f := newRebuildFixture(t)
	f.sync(t, f.link, loadOperationItem(t, "buy.json"))
	f.rebuild(t)
	before := f.journalOf(t, f.accountID)
	if len(before) != 1 || before[0].FeeMinor != 825 {
		t.Fatalf("journal = %d rows with fee %d, want 1 row with fee 825", len(before), before[0].FeeMinor)
	}

	// The same operation with a corrected commission; the commission is not
	// in the content key, so it is the same mirror row refreshed.
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

// A rewritten row inherits the replaced row's created_at. Otherwise a
// purchase the broker merely rewords moves to the end of its day, changing which
// same-day parcel a later sale consumes, and the realized profit (a final tax
// figure) moves with it.
//
// Ten at 30 000 ₽ in the morning, ten at 33 000 ₽ in the afternoon of the same
// day, ten sold a month later at 32 000 ₽: the morning parcel realizes +200 000
// kopecks, the afternoon one −100 000. Only the morning purchase's wording
// changes.
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

	// Only the morning purchase is reworded; the description is not in the
	// content key.
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

// Transfers.

// The two sides of a move between the owner's accounts are rows under two
// links; a per-connection rebuild pairs them.
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
	// The broker's own note only: a pairable leg never carries the "cost
	// unknown" mark.
	if in.Note != "Перевод бумаг между счетами" {
		t.Errorf("the paired arrival's note is %q, want the broker's own description alone", in.Note)
	}
	// 100 shares cost 27 508.25 (27 500.00 plus 8.25 commission), so five
	// carry 1 375.4125, held as whole minor units. The journal worked it out.
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

// A transfer is diffed as one event: the journal refuses removing one leg
// of a pair, and would refuse the whole difference.
func TestRebuildRewritesBothLegsWhenOneOfThemChanged(t *testing.T) {
	f := newRebuildFixture(t)
	second := f.secondLink(t)
	departure := loadOperationItem(t, "trans_bs_bs_out.json")
	f.sync(t, f.link, loadOperationItem(t, "buy.json"), departure)
	f.sync(t, second, loadOperationItem(t, "trans_bs_bs_in.json"))
	f.rebuild(t, f.link, second)
	group := *byExternalID(t, f.journalOf(t, f.accountID),
		externalIDFor(f.mirrorRow(t, f.link, "op-trans-2"), 1)).TransferGroupID

	// Only the departure is reworded: same mirror row, a new note.
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

// Shares leaving for and arriving from outside depositaries are, by type,
// moves with the outside world; two that agree on paper, count and day are
// unrelated parcels. Pairing them would hand the arrival another account's basis
// and dates and wipe the "cost unknown" mark. An own-account move reported under
// these types would stay two lone legs with the mark, the safe error.
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
	if in.Note != "Перевод бумаг от другого брокера — стоимость приобретения брокер не передаёт" {
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
	// 40 of the 100 shares that cost 27 508.25 is 11 003.30, paired or not.
	if out.AmountMinor != 1_100_330 {
		t.Errorf("the departure moved %d, want 1100330", out.AmountMinor)
	}
	if len(in.TransferLots) != 0 {
		t.Errorf("the arrival carries %d lots, want none — no parcel was released to it", len(in.TransferLots))
	}
}

// A transfer leg's currency is the paper's: the broker attaches roubles to
// the zero payment whatever the paper. Taken as the leg's currency, the engine
// would refuse either the dollar purchase or the leg. Pairing is asserted too,
// since currency is part of the pairing key (transferPair).
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
	if in.Note != "Перевод бумаг от другого брокера — стоимость приобретения брокер не передаёт" {
		t.Errorf("the lone arrival's note is %q, want the mark saying its cost is unknown", in.Note)
	}
	if in.AmountMinor != 0 {
		t.Errorf("the lone arrival declares a basis of %d, want 0 — nobody knows what those shares cost", in.AmountMinor)
	}
}

// The broker's own-account move types, as literals; the table test below
// drives the rule with a boolean and cannot say which types set it.
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

// The pairing rule one condition at a time, without a database: every item
// on the list is a separate way for unrelated legs to look like one event.
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

// Refusals.

// An operation the journal will not take costs one visible row, not the
// rest.
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
	// And what the journal said: the code covers many faults (134 such rows
	// on the owner's account). Only that the words are there is checked, never
	// their wording.
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

// A refusal is about the journal as it is; when the missing history
// arrives, the row is accepted.
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
	// The detail goes with the code, or it would describe the wrong
	// refusal.
	if got := now.UnparsedDetail; got != "" {
		t.Errorf("the sale still carries %q, the detail of a refusal that has stopped being true", got)
	}
}

// A projection refusal reaches the mirror with the projection's own code.
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
	// The broker was not asked about a futures contract.
	if calls := f.src.instrumentCalls[uidFutures]; calls != 0 {
		t.Errorf("the broker was asked %d times about the futures instrument, want 0", calls)
	}
}

// A currency purchase is two legs: money left in one currency and arrived in
// another. The traded currency comes from the broker (CurrencyBy); the row names
// only its payment. 1 000 units at 90 ₽: 90 000 ₽ leave, 1 000 $ arrive.
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

// When the broker cannot say what the instrument trades, the row stays
// visible as a currency trade with a missing fact, not as an unsupported
// asset.
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

// Same-day order comes from the broker's clock. The sale is the older
// mirror row (seen first and refused) and its covering purchase arrives later but
// is dated earlier the same day; ordered by first sight, the sale would be
// refused falsely.
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
	// Two more of the same day, so the order is not a coin toss: one of
	// twenty-four orders is right.
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

// Owner's decision 6: half an event is a lie. A trade with a commission in
// another currency is two entries; when the trade is refused and the commission
// is not, the commission must not stay.
func TestRebuildAppliesBothEntriesOfOneRowOrNeither(t *testing.T) {
	f := newRebuildFixture(t)
	// As a sale it sells shares never held, refused, while its commission
	// alone would be taken.
	sale := loadOperationItem(t, "buy_fee_in_another_currency.json")
	sale.Type = "OPERATION_TYPE_SELL"
	sale.Payment = MoneyValue{Currency: "usd", Units: 1200, Nano: 500000000}
	f.sync(t, f.link, loadOperationItem(t, "input.json"), sale)

	stats := f.rebuild(t)
	if stats.Added != 1 {
		t.Errorf("rebuild added %d operations, want 1 — only the top-up survives", stats.Added)
	}
	// Written and withdrawn, and reported, since it repeats every hour.
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

// When only one entry of a row changes, the other already matches and is left
// alone; if the changed one is refused, the untouched half must still be
// withdrawn.
func TestRebuildWithdrawsTheHalfOfAnEventItHadLeftInPlace(t *testing.T) {
	f := newRebuildFixture(t)
	f.sync(t, f.link, loadOperationItem(t, "div_ext.json"))
	if stats := f.rebuild(t); stats.Added != 2 {
		t.Fatalf("the first rebuild added %d entries, want 2 — a dividend paid to a card is income and the money leaving", stats.Added)
	}

	// A rule that changes only the income entry (as a corrected instrument
	// match does) into one the engine refuses; the withdrawal beside it is
	// not offered.
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

// A row with an empty instrument_type but a resolvable instrument_uid
// reaches the resolver: the gate reads passport types, and the broker warns old
// rows can be incomplete.
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

// A delisted paper the broker will never answer about costs one visible
// row, not every future sync (see ErrInstrumentNotFound).
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
	// And which of the resolver's three faults it was; the expectation is
	// this test's input, so only that the sentence travels is pinned.
	if !strings.Contains(refused.UnparsedDetail, brokersWords) {
		t.Errorf("the refused row's detail is %q, want it to carry what the resolver said (%q)",
			refused.UnparsedDetail, brokersWords)
	}
	journal := f.journalOf(t, f.accountID)
	if len(journal) != 1 || journal[0].Type != operation.TypeDeposit {
		t.Fatalf("journal = %d rows, want 1 deposit", len(journal))
	}
}

// An unreachable broker fails the run rather than blaming the operation
// with "not matched".
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

// A cancelled row produces nothing and keeps its stored reason, still true
// of it.
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

// A withdrawn row keeps its reason too: UnparsedByConnection lists it with
// DisappearedAt.
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

// A cancelled order and a withdrawn row project to nothing.
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

// A full redemption closes the position the journal holds.

// The broker reports a full redemption as money only, so the count is the
// position held then. Eight bought and two sold: six, a number in no fixture. The
// payment (10 000 ₽) over the 100 CNY nominal gives 100, which is not a count.
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

	// A redeemed bond must not linger as a position.
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

	// Idempotent on the derived count too.
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

// With none of the bond held there is nothing to count, and the reason is
// "nothing held", not "no quantity named": different faults, different places
// to look.
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

// A redemption counted through an entry whose effect cannot be stated (a
// split; no shape here produces one) stops the rebuild rather than guessing.
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

// unitsMoved against portfolio.Compute on the same entries. The two share
// no code, and the final literal is written out.
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

// Changing the rule.

// A corrected rule reaches the whole history without asking the broker
// again.
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

// What the rebuild refuses to be asked.

func TestRebuildRefusesALinkOfAnotherConnection(t *testing.T) {
	f := newRebuildFixture(t)
	stranger := f.link
	stranger.ConnectionID = uuid.New()

	if _, err := f.reb.Rebuild(f.ctx, f.conn, []AccountLink{stranger}, f.src); !errors.Is(err, ErrLinkNotInConnection) {
		t.Errorf("Rebuild with a foreign link = %v, want ErrLinkNotInConnection", err)
	}
}

// A link of another space would diff one space's journal and hand it to
// another's.
func TestRebuildRefusesALinkOfAnotherSpace(t *testing.T) {
	f := newRebuildFixture(t)
	stranger := f.link
	stranger.SpaceID = uuid.New()

	if _, err := f.reb.Rebuild(f.ctx, f.conn, []AccountLink{stranger}, f.src); !errors.Is(err, ErrLinkOutsideSpace) {
		t.Errorf("Rebuild with a link of another space = %v, want ErrLinkOutsideSpace", err)
	}
}

// The comparison itself.

// comparedField is one change sameJournalRow must notice; field names the
// operation.Operation field, checked against the type by reflection.
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
		{"OccurredAt", "instant", func(o *operation.Operation) { o.OccurredAt = timePtr(time.Date(2026, 3, 15, 9, 0, 0, 0, time.UTC)) }},
		{"OccurredAt", "instant dropped", func(o *operation.Operation) { o.OccurredAt = nil }},
		{"FaceBeforeMinor", "face value before a repayment", func(o *operation.Operation) { v := int64(80_000); o.FaceBeforeMinor = &v }},
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
		{"TradingMode", "trading mode", func(o *operation.Operation) { m := "FINEX_OTC"; o.TradingMode = &m }},
		{"TradingMode", "trading mode dropped", func(o *operation.Operation) { o.TradingMode = nil }},
	}
}

// notComparedFields is every field sameJournalRow ignores, with the reason.
// Each field is in exactly one of the two lists.
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
		// The base has a mode, so both replacing and dropping it are real
		// differences.
		TradingMode: strPtr("TQBR"),
		// An instant for the same reason: "moved" and "dropped" are both
		// differences only from a base that has one.
		OccurredAt: timePtr(time.Date(2026, 3, 15, 7, 30, 15, 0, time.UTC)),
		Source:     Source,
	}
}

func strPtr(s string) *string { return &s }

func timePtr(t time.Time) *time.Time { return &t }

// Each compared field, one at a time.
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

// Every field of operation.Operation, read by reflection, is either exercised
// above or named in notComparedFields, so a new field cannot slip out of the
// comparison unnoticed.
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

// The one exclusion and its boundary: a departing leg's or paired arrival's
// basis is the write path's, and would differ every rebuild; a lone arrival
// declares its own, so a change there is real.
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

// realizedOf is a position's realized result and fails the test when there is
// none (settled in another currency), so a missing figure is never read as
// zero.
func realizedOf(t *testing.T, p *portfolio.Position) int64 {
	t.Helper()
	minor, inOneCurrency := p.RealizedPnL()
	if !inOneCurrency {
		t.Fatalf("position %s has no realized result in one currency: a disposal settled in another", p.InstrumentID)
	}
	return minor
}

// A commission charged as an operation of its own (#138).

// The ordinary case, 310 of 311: the trade carries the commission and the
// fee is a duplicate. Dropped, and the row stays read, not unparsed.
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

// The 311th: the purchase has no commission field, so the 11,34 ₽ fee is
// the only record and is kept.
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

// With the trade absent either answer costs money, so the fee is unparsed
// and its detail names the trade.
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

// Found on live data: a currency trade the broker will not explain is
// unparsed, and its commission goes with it rather than becoming a second
// unparsed row (79 on the owner's account).
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

// A broker fee is charged to the account, not to the trade's paper.
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

// The owner's two dozen delisted dollar and euro pair trades: the pair's name
// and the trade price travel from the mirror to the resolver, where the official
// rate proves the currency.
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

// Without the rate the forgotten pair stays unparsed.
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

// The owner's October 2025 (live, 2026-08-22): Т-Капитал redeemed part of a
// fund; the broker reported OUTPUT_SECURITIES of "44380.35" (fraction only in the
// prose) and two weeks later BOND_REPAYMENT_FULL. The fraction is read with the
// field as proof, the payout stays unparsed, and the position equals the
// broker's. These rows name no asset_uid, so nothing pairs them (see
// TestRebuildBooksAFundRedemptionFromItsTwoRows).
func TestRebuildLeavesAFundPayoutUnparsedAndThePositionIntact(t *testing.T) {
	const uidTech = "7c3f9a2e-1111-4222-8333-abcdefabcdef"
	f := newRebuildFixture(t)
	f.src.instruments[uidTech] = InstrumentBrief{
		UID: uidTech, FIGI: "TCS20A101X68", ISIN: "RU000A101X68",
		Ticker: "TECH", Name: "Технологии Америки", Currency: "RUB", InstrumentType: "etf",
	}

	buy := loadOperationItem(t, "buy.json")
	buy.ID = "op-tech-buy"
	buy.InstrumentUID, buy.FIGI, buy.InstrumentType = uidTech, "TCS20A101X68", "etf"

	out := loadOperationItem(t, "output_securities.json")
	out.ID = "op-tech-out"
	out.InstrumentUID, out.FIGI, out.InstrumentType = uidTech, "TCS20A101X68", "etf"
	out.Quantity = 30
	out.Description = "Вывод 30.5 лотов фонда Технологии Америки в другой депозитарий"
	// Its own day, between purchase and payout, since the fixture's date is
	// in the future relative to when this was written.
	out.Date = time.Date(2026, 5, 20, 8, 0, 0, 0, time.UTC)

	payout := loadOperationItem(t, "bond_repayment_full_no_quantity.json")
	payout.ID = "op-tech-payout"
	// The figi differs from the catalog's for the same paper — the broker
	// re-issues it per listing, and that is exactly how the live payout came.
	payout.InstrumentUID, payout.FIGI, payout.InstrumentType = uidTech, "TCS97A101X68", "etf"

	f.sync(t, f.link, buy, out, payout)
	stats := f.rebuild(t)

	if stats.Added != 2 {
		t.Fatalf("rebuild added %d operations, want 2 — the purchase and the fractional transfer out", stats.Added)
	}
	if stats.Unparsed != 1 {
		t.Errorf("rebuild left %d rows unparsed, want 1 — the payout, whose units the broker does not name", stats.Unparsed)
	}
	row := f.mirrorRow(t, f.link, "op-tech-payout")
	if row.UnparsedReason != "fund_payout_units_unknown" {
		t.Errorf("the payout's reason is %q, want fund_payout_units_unknown", row.UnparsedReason)
	}

	journal := f.journalOf(t, f.accountID)
	moved := byExternalID(t, journal, externalIDFor(f.mirrorRow(t, f.link, "op-tech-out"), 1))
	if moved.Type != operation.TypeTransferOut {
		t.Errorf("the withdrawal is a %s, want transfer_out", moved.Type)
	}
	if moved.Quantity == nil || moved.Quantity.String() != "30.5" {
		t.Errorf("the withdrawal moved %v units, want 30.5 — the description's figure, proved by the field's 30", moved.Quantity)
	}

	positions, err := portfolio.Compute(mustListForEngine(t, f, f.accountID))
	if err != nil {
		t.Fatalf("the journal that was written does not replay when read back: %v", err)
	}
	fund := positions[*moved.InstrumentID]
	if fund == nil {
		t.Fatal("the account holds no position in the fund at all")
	}
	if fund.Quantity.String() != "69.5" {
		t.Errorf("the fund's position is %s, want 69.5 — 100 bought, 30.5 withdrawn, and the payout closing nothing", fund.Quantity)
	}
}

// History imported before the instant was kept gets it on the next rebuild,
// each entry keeping its place in its day.
func TestRebuildPutsTheInstantOnEntriesWrittenWithoutIt(t *testing.T) {
	f := newRebuildFixture(t)
	f.sync(t, f.link, loadOperationItem(t, "input.json"), loadOperationItem(t, "buy.json"))
	f.rebuild(t)
	if _, err := f.pool.Exec(f.ctx, `UPDATE operations SET occurred_at = NULL WHERE account_id = $1`, f.accountID); err != nil {
		t.Fatal(err)
	}
	before := map[string]time.Time{}
	for _, o := range f.journalOf(t, f.accountID) {
		before[*o.ExternalID] = o.CreatedAt
	}

	f.rebuild(t)
	after := f.journalOf(t, f.accountID)
	if len(after) != len(before) {
		t.Fatalf("journal holds %d entries after the rebuild, want %d", len(after), len(before))
	}
	for _, o := range after {
		if o.OccurredAt == nil {
			t.Errorf("%s: still no instant after the rebuild", *o.ExternalID)
		}
		if !o.CreatedAt.Equal(before[*o.ExternalID]) {
			t.Errorf("%s: recorded at %s, was %s — a rewrite must keep the entry's place", *o.ExternalID, o.CreatedAt, before[*o.ExternalID])
		}
	}
}

// Р-13 on the shape of the owner's October 2025 (live, 2026-10-05): units
// leave as OUTPUT_SECURITIES of an OTC listing, money comes two weeks later as
// BOND_REPAYMENT_FULL of the exchange listing, sharing only asset_uid. Together:
// one redemption on the payout day.
func TestRebuildBooksAFundRedemptionFromItsTwoRows(t *testing.T) {
	const (
		uidTech = "7c3f9a2e-1111-4222-8333-abcdefabcdef"
		asset   = "76d078d5-8bf9-4377-8e60-36026972fe24"
	)
	f := newRebuildFixture(t)
	f.src.instruments[uidTech] = InstrumentBrief{
		UID: uidTech, FIGI: "TCS97A101X68", ISIN: "RU000A101X68",
		Ticker: "TECH", Name: "Технологии Америки", Currency: "RUB", InstrumentType: "etf",
	}

	buy := loadOperationItem(t, "buy.json")
	buy.ID = "op-tech-buy"
	buy.InstrumentUID, buy.FIGI, buy.InstrumentType, buy.AssetUID = uidTech, "TCS97A101X68", "etf", asset

	out := loadOperationItem(t, "output_securities.json")
	out.ID = "op-tech-out"
	// The over-the-counter listing: a uid the broker answers 404 for, which is
	// never asked about — the withdrawal is part of the redemption.
	out.InstrumentUID, out.FIGI, out.InstrumentType, out.AssetUID = "uid-tech-otc", "TCS33A101X68", "etf", asset
	out.Ticker = "RU000A101X68"
	out.Quantity = 30
	out.Description = "Вывод 30.5 лотов фонда Технологии Америки в другой депозитарий"
	out.Date = time.Date(2026, 5, 20, 8, 0, 0, 0, time.UTC)

	payout := loadOperationItem(t, "bond_repayment_full_no_quantity.json")
	payout.ID = "op-tech-payout"
	payout.InstrumentUID, payout.FIGI, payout.InstrumentType, payout.AssetUID = uidTech, "TCS97A101X68", "etf", asset
	payout.Date = time.Date(2026, 6, 3, 14, 0, 0, 0, time.UTC)

	f.sync(t, f.link, buy, out, payout)
	stats := f.rebuild(t)

	if stats.Unparsed != 0 {
		t.Errorf("rebuild left %d rows unparsed, want none", stats.Unparsed)
	}
	if row := f.mirrorRow(t, f.link, "op-tech-out"); row.UnparsedReason != "" {
		t.Errorf("the withdrawal carries the reason %q, want it read as part of the redemption", row.UnparsedReason)
	}
	journal := f.journalOf(t, f.accountID)
	if len(journal) != 2 {
		t.Fatalf("journal holds %d entries, want 2 — the purchase and the redemption", len(journal))
	}
	redeemed := byExternalID(t, journal, externalIDFor(f.mirrorRow(t, f.link, "op-tech-payout"), 1))
	if redeemed.Type != operation.TypeRedemption {
		t.Errorf("the payout is a %s, want a redemption — the holder did not sell", redeemed.Type)
	}
	if redeemed.Quantity == nil || redeemed.Quantity.String() != "30.5" {
		t.Errorf("the redemption retired %v units, want the 30.5 withdrawn", redeemed.Quantity)
	}
	if !redeemed.OccurredOn.Equal(day(t, "2026-06-03")) {
		t.Errorf("the redemption is dated %s, want the payout's day", redeemed.OccurredOn.Format("2006-01-02"))
	}
	if !strings.Contains(redeemed.Note, "паи выведены под погашение 20.05.2026") {
		t.Errorf("the redemption's note is %q, want the day the units left", redeemed.Note)
	}

	positions, err := portfolio.Compute(mustListForEngine(t, f, f.accountID))
	if err != nil {
		t.Fatalf("the journal does not replay: %v", err)
	}
	if fund := positions[*redeemed.InstrumentID]; fund == nil || fund.Quantity.String() != "69.5" {
		t.Errorf("the fund's position is %v, want 69.5 — 100 bought, 30.5 redeemed", fund)
	}
}

// Two withdrawals before one payout: ambiguous, so nothing is paired.
func TestRebuildPairsAFundPayoutOnlyWhenItIsUnambiguous(t *testing.T) {
	const (
		uidTech = "7c3f9a2e-1111-4222-8333-abcdefabcdef"
		asset   = "76d078d5-8bf9-4377-8e60-36026972fe24"
	)
	f := newRebuildFixture(t)
	f.src.instruments[uidTech] = InstrumentBrief{
		UID: uidTech, FIGI: "TCS97A101X68", ISIN: "RU000A101X68",
		Ticker: "TECH", Name: "Технологии Америки", Currency: "RUB", InstrumentType: "etf",
	}
	buy := loadOperationItem(t, "buy.json")
	buy.ID = "op-tech-buy"
	buy.InstrumentUID, buy.FIGI, buy.InstrumentType, buy.AssetUID = uidTech, "TCS97A101X68", "etf", asset
	var outs []OperationItem
	for i, d := range []time.Time{time.Date(2026, 5, 20, 8, 0, 0, 0, time.UTC), time.Date(2026, 5, 22, 8, 0, 0, 0, time.UTC)} {
		out := loadOperationItem(t, "output_securities.json")
		out.ID = fmt.Sprintf("op-tech-out-%d", i)
		out.InstrumentUID, out.FIGI, out.InstrumentType, out.AssetUID = uidTech, "TCS97A101X68", "etf", asset
		out.Quantity = 10
		out.Description = "Вывод 10 лотов фонда Технологии Америки в другой депозитарий"
		out.Date = d
		outs = append(outs, out)
	}
	payout := loadOperationItem(t, "bond_repayment_full_no_quantity.json")
	payout.ID = "op-tech-payout"
	payout.InstrumentUID, payout.FIGI, payout.InstrumentType, payout.AssetUID = uidTech, "TCS97A101X68", "etf", asset
	payout.Date = time.Date(2026, 6, 3, 14, 0, 0, 0, time.UTC)

	f.sync(t, f.link, buy, outs[0], outs[1], payout)
	f.rebuild(t)
	if row := f.mirrorRow(t, f.link, "op-tech-payout"); row.UnparsedReason != string(ReasonFundPayoutUnitsUnknown) {
		t.Errorf("the payout's reason is %q, want %s", row.UnparsedReason, ReasonFundPayoutUnitsUnknown)
	}
}
