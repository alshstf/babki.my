package tinvest

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
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
	reb     *Rebuilder
}

func newRebuildFixture(t *testing.T) *rebuildFixture {
	t.Helper()
	f := newFixture(t)
	catalog := &countingCatalog{Store: instrument.NewStore(f.pool)}
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
	ops := operation.NewStore(f.pool)
	applier := &recordingDelta{inner: operation.NewService(ops)}
	return &rebuildFixture{
		fixture: f, ops: ops, applier: applier, src: src, catalog: catalog,
		reb: NewRebuilder(f.store, NewResolver(f.store, catalog, nil), applier, ops, nil),
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
	for id, now := range f.mirrorVersions(t) {
		if was := versions[id]; now != was {
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

// TestRebuildPairedArrivalDropsTheUnknownBasisNote pins a caption against the
// number beside it. The projection marks shares arriving from another broker
// with "the cost is unknown, the broker does not report it" — true of a leg
// that stands alone, and false the moment the leg is paired, because then the
// basis is released from the source account and is known exactly.
func TestRebuildPairedArrivalDropsTheUnknownBasisNote(t *testing.T) {
	f := newRebuildFixture(t)
	second := f.secondLink(t)

	// The same day on both legs — the pairing needs one paper, one count, one
	// day. The fixtures carry two different days, so the day is stated here.
	leaving := loadOperationItem(t, "output_securities.json")
	leaving.Date = time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	arriving := loadOperationItem(t, "input_securities.json")
	arriving.Date = time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)

	f.sync(t, f.link, loadOperationItem(t, "buy.json"), leaving)
	f.sync(t, second, arriving)
	f.rebuild(t, f.link, second)

	in := byExternalID(t, f.journalOf(t, second.AccountID), externalIDFor(f.mirrorRow(t, second, "op-insec-1"), 1))
	if in.TransferGroupID == nil {
		t.Fatal("the arrival was not paired with the departure, so this test would prove nothing")
	}
	if in.Note != "Перевод бумаг от другого брокера" {
		t.Errorf("the paired arrival's note is %q, want the broker's own description alone: its cost basis is known exactly", in.Note)
	}
	// 40 of the 100 shares that cost 27508.25 — the payment plus the
	// commission — is 11003.30.
	if in.AmountMinor != 1_100_330 {
		t.Errorf("the arrival carries a basis of %d, want 1100330", in.AmountMinor)
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
	if got := f.mirrorRow(t, f.link, "op-sell-1").UnparsedReason; got != string(ReasonEngineRefused) {
		t.Errorf("the refused row's reason is %q, want %q", got, ReasonEngineRefused)
	}
	if got := f.mirrorRow(t, f.link, "op-input-1").UnparsedReason; got != "" {
		t.Errorf("the top-up's reason is %q, want empty", got)
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
	if got := f.mirrorRow(t, f.link, "op-sell-1").UnparsedReason; got != string(ReasonEngineRefused) {
		t.Fatalf("the sale's reason is %q, want %q — this test starts from a refusal", got, ReasonEngineRefused)
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
	if got := f.mirrorRow(t, f.link, "op-sell-1").UnparsedReason; got != "" {
		t.Errorf("the sale's reason is %q, want empty — the journal takes it now", got)
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

// TestRebuildGivesACurrencyTradeItsOwnReason is the sharp case of the rule
// above. Everything about a currency purchase can be resolved — the broker has
// a passport for it and would gladly say it is a currency — and if the rebuild
// asked, the answer would be "this program does not account for that kind of
// asset". That answer is FALSE here: the journal has a type for a conversion,
// and what is missing is the data to build the second leg from
// (ReasonCurrencyTrade). So the projection, which knows that, is the one that
// names the fault.
func TestRebuildGivesACurrencyTradeItsOwnReason(t *testing.T) {
	f := newRebuildFixture(t)
	f.sync(t, f.link, loadOperationItem(t, "currency_buy.json"))

	stats := f.rebuild(t)
	if stats.Added != 0 || stats.Unparsed != 1 {
		t.Errorf("rebuild added %d and left %d unparsed, want 0 and 1", stats.Added, stats.Unparsed)
	}
	if got := f.mirrorRow(t, f.link, "op-cur-1").UnparsedReason; got != string(ReasonCurrencyTrade) {
		t.Errorf("the currency purchase's reason is %q, want %q", got, ReasonCurrencyTrade)
	}
	if calls := f.src.instrumentCalls[uidUSDRUB]; calls != 0 {
		t.Errorf("the broker was asked %d times about the currency, want 0", calls)
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
	f.reb.project = func(row MirrorRow, accountID uuid.UUID, resolved *Resolved) ([]operation.Operation, *UnparsedError) {
		ops, refusal := inner(row, accountID, resolved)
		for i := range ops {
			if ops[i].Type == operation.TypeDeposit {
				ops[i].Note = "переведено по новому правилу"
			}
		}
		return ops, refusal
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

// -------------------------------------------------------------------------
// the comparison itself
// -------------------------------------------------------------------------

// TestSameJournalRowNoticesEveryFieldItCompares walks the fields one at a time.
// A comparison that quietly stopped looking at one of them would leave the
// journal holding a value the mirror no longer says, and nothing anywhere
// would report a difference.
func TestSameJournalRowNoticesEveryFieldItCompares(t *testing.T) {
	instrumentA := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	instrumentB := uuid.MustParse("44444444-4444-4444-8444-444444444444")
	groupA := uuid.MustParse("55555555-5555-4555-8555-555555555555")
	qty := decimal.RequireFromString("10")
	price := decimal.RequireFromString("275")
	settled := day(t, "2026-03-16")

	base := operation.Operation{
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
	cases := []struct {
		field  string
		mutate func(*operation.Operation)
	}{
		{"account", func(o *operation.Operation) { o.AccountID = uuid.New() }},
		{"instrument", func(o *operation.Operation) { o.InstrumentID = &instrumentB }},
		{"instrument dropped", func(o *operation.Operation) { o.InstrumentID = nil }},
		{"type", func(o *operation.Operation) { o.Type = operation.TypeSell }},
		{"day", func(o *operation.Operation) { o.OccurredOn = day(t, "2026-03-16") }},
		{"settlement day", func(o *operation.Operation) { o.SettledOn = &settled }},
		{"quantity", func(o *operation.Operation) { q := decimal.RequireFromString("11"); o.Quantity = &q }},
		{"quantity dropped", func(o *operation.Operation) { o.Quantity = nil }},
		{"price", func(o *operation.Operation) { p := decimal.RequireFromString("276"); o.Price = &p }},
		{"price dropped", func(o *operation.Operation) { o.Price = nil }},
		{"amount", func(o *operation.Operation) { o.AmountMinor = -2_750_001 }},
		{"currency", func(o *operation.Operation) { o.Currency = "USD" }},
		{"fee", func(o *operation.Operation) { o.FeeMinor = 826 }},
		{"note", func(o *operation.Operation) { o.Note = "что-то другое" }},
		{"source", func(o *operation.Operation) { o.Source = "csv" }},
		{"transfer group", func(o *operation.Operation) { o.TransferGroupID = &groupA }},
		{"split ratio", func(o *operation.Operation) { r := decimal.RequireFromString("2"); o.SplitRatio = &r }},
	}
	if !sameJournalRow(base, base) {
		t.Fatal("a row differs from itself")
	}
	for _, c := range cases {
		changed := base
		c.mutate(&changed)
		if sameJournalRow(base, changed) {
			t.Errorf("a row whose %s changed reads as unchanged", c.field)
		}
		if sameJournalRow(changed, base) {
			t.Errorf("a row whose %s changed reads as unchanged the other way round", c.field)
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
