package corporateaction_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/corporateaction"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/portfolio"
)

type fixture struct {
	ctx          context.Context
	pool         *pgxpool.Pool
	store        *corporateaction.Store
	ops          *operation.Store
	svc          *operation.Service
	materializer *corporateaction.Materializer
	spaceID      uuid.UUID
	accountID    uuid.UUID
	otherID      uuid.UUID
	amazonID     uuid.UUID
}

// amazonISIN is Amazon's own, and the split below is Amazon's own: twenty for
// one, first traded in the new quantity on 2022-06-06. The owner's account
// holds one share and the broker reports twenty — the difference this whole
// package exists to close.
const amazonISIN = "US0231351067"

func newFixture(t *testing.T) fixture {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()
	fam := family.NewStore(pool)
	u, err := fam.CreateUser(ctx, "alex", "A", "h")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	sp, err := fam.CreateSpaceWithOwner(ctx, "S", u.ID)
	if err != nil {
		t.Fatalf("space: %v", err)
	}
	acc := account.NewStore(pool)
	a1, err := acc.Create(ctx, sp.ID, nil, "Брокер", account.TypeBrokerage, "USD", "")
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	a2, err := acc.Create(ctx, sp.ID, nil, "Брокер 2", account.TypeBrokerage, "USD", "")
	if err != nil {
		t.Fatalf("second account: %v", err)
	}
	amazon, err := instrument.NewStore(pool).Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Amazon", Ticker: "AMZN", ISIN: amazonISIN, Currency: "USD",
	})
	if err != nil {
		t.Fatalf("instrument: %v", err)
	}
	store := corporateaction.NewStore(pool)
	ops := operation.NewStore(pool)
	svc := operation.NewService(ops)
	return fixture{
		ctx: ctx, pool: pool, store: store, ops: ops, svc: svc,
		materializer: corporateaction.NewMaterializer(store, ops, svc, nil, nil),
		spaceID:      sp.ID, accountID: a1.ID, otherID: a2.ID, amazonID: amazon.ID,
	}
}

func date(s string) time.Time {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return d
}

func dec(s string) *decimal.Decimal {
	d := decimal.RequireFromString(s)
	return &d
}

// buy records an ordinary purchase through the hand-entry door.
func (f fixture) buy(t *testing.T, accountID uuid.UUID, on string, qty string, amountMinor int64) {
	t.Helper()
	if _, err := f.svc.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: accountID, InstrumentID: &f.amazonID, Type: operation.TypeBuy,
		OccurredOn: date(on), Quantity: dec(qty), AmountMinor: amountMinor, Currency: "USD",
	}); err != nil {
		t.Fatalf("buy %s on %s: %v", qty, on, err)
	}
}

// sell records an ordinary sale through the hand-entry door.
func (f fixture) sell(t *testing.T, accountID uuid.UUID, on string, qty string, amountMinor int64) {
	t.Helper()
	if _, err := f.svc.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: accountID, InstrumentID: &f.amazonID, Type: operation.TypeSell,
		OccurredOn: date(on), Quantity: dec(qty), AmountMinor: amountMinor, Currency: "USD",
	}); err != nil {
		t.Fatalf("sell %s on %s: %v", qty, on, err)
	}
}

// splitEvent records Amazon's 2022 split in the registry.
func (f fixture) splitEvent(t *testing.T, on string, from, to int64) corporateaction.Event {
	t.Helper()
	e, err := f.store.Create(f.ctx, corporateaction.Event{
		Kind: corporateaction.KindSplit, ISIN: amazonISIN, EffectiveOn: date(on),
		RatioFrom: from, RatioTo: to,
		Source: corporateaction.SourceManual, SourceRef: "https://ir.aboutamazon.com/",
	})
	if err != nil {
		t.Fatalf("record the split: %v", err)
	}
	return e
}

// held is what the engine says the account holds, folding its whole journal.
func (f fixture) held(t *testing.T, accountID uuid.UUID) decimal.Decimal {
	t.Helper()
	journal, err := f.ops.ListForEngine(f.ctx, f.spaceID, accountID)
	if err != nil {
		t.Fatalf("read the journal: %v", err)
	}
	positions, err := portfolio.Compute(journal)
	if err != nil {
		t.Fatalf("fold the journal: %v", err)
	}
	p, ok := positions[f.amazonID]
	if !ok {
		return decimal.Zero
	}
	return p.Quantity
}

// registryRows is every row the registry has written into an account.
func (f fixture) registryRows(t *testing.T, accountID uuid.UUID) []operation.Operation {
	t.Helper()
	rows, err := f.ops.ListBySource(f.ctx, f.spaceID, accountID, operation.SourceRegistry)
	if err != nil {
		t.Fatalf("read the registry's rows: %v", err)
	}
	return rows
}

// TestASplitReachesEveryAccountThatHeldThePaper is the whole point of the
// registry in one test: the fact is recorded ONCE, against the ISIN, and both
// accounts holding Amazon are multiplied.
func TestASplitReachesEveryAccountThatHeldThePaper(t *testing.T) {
	f := newFixture(t)
	f.buy(t, f.accountID, "2021-05-04", "1", -323_000)
	f.buy(t, f.otherID, "2021-06-01", "3", -969_000)

	f.splitEvent(t, "2022-06-06", 1, 20)
	stats, err := f.materializer.ForISIN(f.ctx, amazonISIN)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if stats.Added != 2 || stats.Removed != 0 {
		t.Errorf("stats = %+v, want 2 added and none removed — one row per account holding the paper", stats)
	}
	if got, want := f.held(t, f.accountID), decimal.RequireFromString("20"); !got.Equal(want) {
		t.Errorf("first account holds %s, want %s", got, want)
	}
	if got, want := f.held(t, f.otherID), decimal.RequireFromString("60"); !got.Equal(want) {
		t.Errorf("second account holds %s, want %s", got, want)
	}
}

// TestNothingIsWrittenForAnAccountThatHeldNothingOnTheDay is the rule that
// keeps the registry from inventing holdings.
//
// AN ACCOUNT THAT SOLD OUT BEFORE THE SPLIT still shows up in the list of
// accounts that ever traded the paper — that list is a cheap query over the
// journal, not a judgement about holdings (see Store.holders) — so the only
// thing standing between it and a split row is the fold. A split written there
// multiplies a position of zero, which changes no quantity but puts a row in
// the journal claiming a corporate action reached an account it did not.
//
// The second account, which bought AFTER the split, is the same rule from the
// other side: it held nothing on the day either.
func TestNothingIsWrittenForAnAccountThatHeldNothingOnTheDay(t *testing.T) {
	f := newFixture(t)
	f.buy(t, f.accountID, "2021-05-04", "1", -323_000)
	f.sell(t, f.accountID, "2022-01-10", "1", 300_000)
	f.buy(t, f.otherID, "2023-01-10", "5", -500_000)

	f.splitEvent(t, "2022-06-06", 1, 20)
	stats, err := f.materializer.ForISIN(f.ctx, amazonISIN)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if stats.Added != 0 {
		t.Errorf("stats = %+v, want nothing written — one account had sold out, the other had not bought yet", stats)
	}
	if rows := f.registryRows(t, f.accountID); len(rows) != 0 {
		t.Errorf("the account that sold out got %d registry rows, want none", len(rows))
	}
	if rows := f.registryRows(t, f.otherID); len(rows) != 0 {
		t.Errorf("the account that bought later got %d registry rows, want none", len(rows))
	}
	if got, want := f.held(t, f.otherID), decimal.RequireFromString("5"); !got.Equal(want) {
		t.Errorf("the later purchase is %s, want %s untouched — it was made in post-split shares", got, want)
	}
}

// TestTheHoldingIsTakenAtTheSTARTOfTheEffectiveDay pins the one date rule the
// whole registry rests on.
//
// The effective day is the FIRST day the paper trades in the new quantity, so a
// purchase dated that day is already in post-split shares. Both halves are
// checked here: the ten held from before are multiplied, and the ten bought on
// the day are not.
func TestTheHoldingIsTakenAtTheSTARTOfTheEffectiveDay(t *testing.T) {
	f := newFixture(t)
	f.buy(t, f.accountID, "2022-06-03", "10", -1_000_000)
	f.buy(t, f.accountID, "2022-06-06", "10", -50_000)

	f.splitEvent(t, "2022-06-06", 1, 20)
	if _, err := f.materializer.ForISIN(f.ctx, amazonISIN); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if got, want := f.held(t, f.accountID), decimal.RequireFromString("210"); !got.Equal(want) {
		t.Errorf("holding = %s, want %s — ten held before the day become two hundred, "+
			"and the ten bought on the day are already post-split", got, want)
	}
}

// TestAnAccountWhoseFirstPurchaseIsTheEffectiveDayGetsNoRow is the case that
// separates "held at the START of the day" from "held at any point during it".
//
// A buyer whose first purchase is dated the effective day bought post-split
// shares from a post-split market: there is nothing of theirs for the split to
// multiply, and a row saying otherwise claims a corporate action reached an
// account that was not in the register when it happened. The QUANTITY does not
// give it away — multiplying a holding of zero changes nothing, and the day's
// purchase folds after the split row in any case (see operation.foldRank) — so
// only the presence of the row itself can.
func TestAnAccountWhoseFirstPurchaseIsTheEffectiveDayGetsNoRow(t *testing.T) {
	f := newFixture(t)
	f.buy(t, f.accountID, "2022-06-06", "20", -240_000)

	f.splitEvent(t, "2022-06-06", 1, 20)
	stats, err := f.materializer.ForISIN(f.ctx, amazonISIN)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if stats.Added != 0 {
		t.Errorf("stats = %+v, want nothing written — the account held none of the paper when the day began", stats)
	}
	if rows := f.registryRows(t, f.accountID); len(rows) != 0 {
		t.Errorf("got %d registry rows, want none — a buyer on the effective day bought post-split shares", len(rows))
	}
	if got, want := f.held(t, f.accountID), decimal.RequireFromString("20"); !got.Equal(want) {
		t.Errorf("holding = %s, want %s untouched", got, want)
	}
}

// TestTwoSplitsCompoundInDateOrder: each event acts on the holding the ones
// before it left, so a paper split ten for one and then two for one is held
// twenty times over.
func TestTwoSplitsCompoundInDateOrder(t *testing.T) {
	f := newFixture(t)
	f.buy(t, f.accountID, "2020-01-02", "1", -100_000)
	f.splitEvent(t, "2021-06-01", 1, 10)
	f.splitEvent(t, "2024-06-10", 1, 2)

	if _, err := f.materializer.ForISIN(f.ctx, amazonISIN); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if got, want := f.held(t, f.accountID), decimal.RequireFromString("20"); !got.Equal(want) {
		t.Errorf("holding = %s, want %s", got, want)
	}
}

// TestRunningTwiceChangesNothing is what makes the materialization safe to
// trigger from anywhere: it is a difference against what it wrote last time,
// not an append.
//
// It checks the stored ROW is untouched rather than only counting rows: a
// rewrite that removed and re-inserted the same content would keep the count
// and move the row's created_at, and within a day the journal folds by
// created_at — so a run that "changed nothing" could still move where a split
// sits among that day's operations.
func TestRunningTwiceChangesNothing(t *testing.T) {
	f := newFixture(t)
	f.buy(t, f.accountID, "2021-05-04", "1", -323_000)
	f.splitEvent(t, "2022-06-06", 1, 20)

	if _, err := f.materializer.ForISIN(f.ctx, amazonISIN); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first := f.registryRows(t, f.accountID)
	if len(first) != 1 {
		t.Fatalf("first run wrote %d rows, want 1", len(first))
	}

	stats, err := f.materializer.ForISIN(f.ctx, amazonISIN)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if stats.Added != 0 || stats.Removed != 0 {
		t.Errorf("second run: stats = %+v, want nothing added and nothing removed", stats)
	}
	second := f.registryRows(t, f.accountID)
	if len(second) != 1 {
		t.Fatalf("second run left %d rows, want 1", len(second))
	}
	if second[0].ID != first[0].ID || !second[0].CreatedAt.Equal(first[0].CreatedAt) {
		t.Errorf("the row was rewritten: id %s -> %s, created_at %s -> %s",
			first[0].ID, second[0].ID, first[0].CreatedAt, second[0].CreatedAt)
	}
	if got, want := f.held(t, f.accountID), decimal.RequireFromString("20"); !got.Equal(want) {
		t.Errorf("holding after two runs = %s, want %s — a second run must not multiply again", got, want)
	}
}

// TestDeletingTheEventTakesItsJournalRowsWithIt: the journal rows are derived,
// so a fact withdrawn withdraws them.
func TestDeletingTheEventTakesItsJournalRowsWithIt(t *testing.T) {
	f := newFixture(t)
	f.buy(t, f.accountID, "2021-05-04", "1", -323_000)
	e := f.splitEvent(t, "2022-06-06", 1, 20)
	if _, err := f.materializer.ForISIN(f.ctx, amazonISIN); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	if _, err := f.store.Delete(f.ctx, e.ID); err != nil {
		t.Fatalf("delete the event: %v", err)
	}
	stats, err := f.materializer.ForISIN(f.ctx, amazonISIN)
	if err != nil {
		t.Fatalf("materialize after the deletion: %v", err)
	}
	if stats.Removed != 1 {
		t.Errorf("stats = %+v, want one row removed", stats)
	}
	if rows := f.registryRows(t, f.accountID); len(rows) != 0 {
		t.Errorf("%d registry rows survive the event, want none", len(rows))
	}
	if got, want := f.held(t, f.accountID), decimal.RequireFromString("1"); !got.Equal(want) {
		t.Errorf("holding = %s, want %s — the split is gone", got, want)
	}
}

// TestCorrectingARatioRewritesTheRowInPlace: the ratio is the one field a
// person is likeliest to get wrong, and correcting it must correct the journal
// without moving the row within its day.
func TestCorrectingARatioRewritesTheRowInPlace(t *testing.T) {
	f := newFixture(t)
	f.buy(t, f.accountID, "2021-05-04", "1", -323_000)
	e := f.splitEvent(t, "2022-06-06", 1, 2)
	if _, err := f.materializer.ForISIN(f.ctx, amazonISIN); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	before := f.registryRows(t, f.accountID)
	if len(before) != 1 {
		t.Fatalf("wrote %d rows, want 1", len(before))
	}

	if _, err := f.store.Delete(f.ctx, e.ID); err != nil {
		t.Fatalf("delete the wrong event: %v", err)
	}
	f.splitEvent(t, "2022-06-06", 1, 20)
	if _, err := f.materializer.ForISIN(f.ctx, amazonISIN); err != nil {
		t.Fatalf("materialize the correction: %v", err)
	}
	if got, want := f.held(t, f.accountID), decimal.RequireFromString("20"); !got.Equal(want) {
		t.Errorf("holding = %s, want %s", got, want)
	}
}

// TestForAccountFindsTheEventsAPurchaseHasJustEarned is the trigger a manual
// write fires: the registry has known about the split all along, and a purchase
// entered today with an old date is the thing that makes it apply here.
func TestForAccountFindsTheEventsAPurchaseHasJustEarned(t *testing.T) {
	f := newFixture(t)
	f.splitEvent(t, "2022-06-06", 1, 20)
	f.buy(t, f.accountID, "2021-05-04", "1", -323_000)

	stats, err := f.materializer.ForAccount(f.ctx, f.spaceID, f.accountID)
	if err != nil {
		t.Fatalf("materialize for the account: %v", err)
	}
	if stats.Added != 1 {
		t.Errorf("stats = %+v, want one row written", stats)
	}
	if got, want := f.held(t, f.accountID), decimal.RequireFromString("20"); !got.Equal(want) {
		t.Errorf("holding = %s, want %s", got, want)
	}
}

// TestTheSweepCoversEveryPaperInTheRegistry: the safety net behind the
// triggers.
func TestTheSweepCoversEveryPaperInTheRegistry(t *testing.T) {
	f := newFixture(t)
	f.buy(t, f.accountID, "2021-05-04", "1", -323_000)
	f.splitEvent(t, "2022-06-06", 1, 20)

	stats, err := f.materializer.All(f.ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if stats.Added != 1 {
		t.Errorf("stats = %+v, want one row written", stats)
	}
	if got, want := f.held(t, f.accountID), decimal.RequireFromString("20"); !got.Equal(want) {
		t.Errorf("holding = %s, want %s", got, want)
	}
}

// TestAKindTheJournalCannotHoldYetIsRecordedAndNotMaterialized states the
// package's own boundary: conversions and spin-offs are storable facts today
// and produce no journal rows, because the journal has no type for one paper
// becoming another. Recording them early is deliberate — nobody can go back and
// ask a registrar what happened in 2023.
func TestAKindTheJournalCannotHoldYetIsRecordedAndNotMaterialized(t *testing.T) {
	f := newFixture(t)
	f.buy(t, f.accountID, "2021-05-04", "1", -323_000)
	if _, err := f.store.Create(f.ctx, corporateaction.Event{
		Kind: corporateaction.KindConversion, ISIN: amazonISIN, ResultISIN: "US0231351068",
		EffectiveOn: date("2024-02-27"), RatioFrom: 1, RatioTo: 1,
		Source: corporateaction.SourceManual, SourceRef: "https://www.moex.com/n67851",
	}); err != nil {
		t.Fatalf("record the conversion: %v", err)
	}

	stats, err := f.materializer.ForISIN(f.ctx, amazonISIN)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if stats.Added != 0 {
		t.Errorf("stats = %+v, want nothing written — the journal has no type for a conversion yet", stats)
	}
	if got, want := f.held(t, f.accountID), decimal.RequireFromString("1"); !got.Equal(want) {
		t.Errorf("holding = %s, want %s", got, want)
	}
}
