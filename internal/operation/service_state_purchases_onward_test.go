package operation_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/portfolio"
)

// onward is a fixture for #227: ten shares that arrived on f.accountID with no
// price, and helpers to move them on and to read where they ended up.
type onward struct {
	fixture
	svc     *operation.Service
	arrival operation.Operation
}

func newOnward(t *testing.T) onward {
	t.Helper()
	f := newFixture(t)
	o := onward{fixture: f, svc: operation.NewService(f.store)}
	var err error
	o.arrival, err = o.svc.CreateArrival(f.ctx, f.spaceID, operation.ArrivalParams{
		AccountID: f.accountID, InstrumentID: f.sberID, OccurredOn: date("2026-06-15"),
		Quantity: decimal.NewFromInt(10), Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("arrival: %v", err)
	}
	return o
}

func (o onward) move(t *testing.T, from, to uuid.UUID, qty int64, on string, cost *int64) (out, in operation.Operation) {
	t.Helper()
	out, in, err := o.svc.CreateTransfer(o.ctx, o.spaceID, operation.TransferParams{
		FromAccountID: from, ToAccountID: to, InstrumentID: o.sberID,
		Quantity: decimal.NewFromInt(qty), OccurredOn: date(on), CostMinorOverride: cost,
	})
	if err != nil {
		t.Fatalf("move %d on %s: %v", qty, on, err)
	}
	return out, in
}

// state states one purchase behind all ten: 100 ₽ a share, bought on day.
func (o onward) state(day string) error {
	price := decimal.NewFromInt(100)
	acquired := date(day)
	_, err := o.svc.StatePurchases(o.ctx, o.spaceID, o.arrival.ID, []operation.StatedPurchase{
		{Quantity: decimal.NewFromInt(10), Price: &price, AcquiredOn: &acquired},
	})
	return err
}

func (o onward) position(t *testing.T, accountID uuid.UUID) *portfolio.Position {
	t.Helper()
	journal, err := o.store.ListForEngine(o.ctx, o.spaceID, accountID)
	if err != nil {
		t.Fatalf("ListForEngine: %v", err)
	}
	positions, err := portfolio.Compute(journal)
	if err != nil {
		t.Fatalf("account %s does not replay: %v", accountID, err)
	}
	return positions[o.sberID]
}

func (o onward) amount(t *testing.T, id uuid.UUID) int64 {
	t.Helper()
	op, err := o.store.ByID(o.ctx, o.spaceID, id)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	return op.AmountMinor
}

func lotDays(p *portfolio.Position) []string {
	var days []string
	for _, l := range p.Lots {
		day := "-"
		if l.AcquiredOn != nil {
			day = l.AcquiredOn.Format(time.DateOnly)
		}
		days = append(days, day)
	}
	return days
}

// Shares moved three times — A to B, part of them B to C, part of those C to
// D — reach D with the price and day stated on A, and every move carries the
// restated basis on both legs: each onward move is released from its account
// as the move before it left that account, so the moves are released in the
// order they happened, whichever account each one left.
func TestStatedPurchasesFollowTheSharesThroughEveryLaterMove(t *testing.T) {
	o := newOnward(t)
	c, d := o.newAccount(t), o.newAccount(t)
	ab, ba := o.move(t, o.accountID, o.account2ID, 6, "2026-07-01", nil)
	bc, cb := o.move(t, o.account2ID, c, 4, "2026-07-02", nil)
	cd, dc := o.move(t, c, d, 2, "2026-07-03", nil)

	if err := o.state("2021-03-02"); err != nil {
		t.Fatalf("state purchases: %v", err)
	}
	for name, want := range map[string]struct {
		account uuid.UUID
		cost    int64
	}{"A": {o.accountID, 40_000}, "B": {o.account2ID, 20_000}, "C": {c, 20_000}, "D": {d, 20_000}} {
		p := o.position(t, want.account)
		if p == nil || p.CostMinor != want.cost {
			t.Errorf("account %s: %+v, want a cost of %d", name, p, want.cost)
			continue
		}
		for _, day := range lotDays(p) {
			if day != "2021-03-02" {
				t.Errorf("account %s holds a lot dated %s, want the stated 2021-03-02", name, day)
			}
		}
	}
	for id, want := range map[uuid.UUID]int64{
		ab.ID: 60_000, ba.ID: 60_000, bc.ID: 40_000, cb.ID: 40_000, cd.ID: 20_000, dc.ID: 20_000,
	} {
		if got := o.amount(t, id); got != want {
			t.Errorf("transfer leg %s amount = %d, want %d", id, got, want)
		}
	}
}

// A move that folds before the arrival — made the same day, entered first —
// cannot have carried its shares, so the account it went to is not one the
// statement touches: archiving that account since does not stop it.
func TestAnAccountOnlyAnEarlierMoveReachedDoesNotStopTheStatement(t *testing.T) {
	f := newFixture(t)
	o := onward{fixture: f, svc: operation.NewService(f.store)}
	if _, err := o.svc.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-06-01"), Quantity: dec("5"), Price: dec("50"),
		AmountMinor: -25_000, Currency: "RUB",
	}); err != nil {
		t.Fatalf("buy: %v", err)
	}
	o.move(t, f.accountID, f.account2ID, 5, "2026-06-15", nil)
	var err error
	o.arrival, err = o.svc.CreateArrival(f.ctx, f.spaceID, operation.ArrivalParams{
		AccountID: f.accountID, InstrumentID: f.sberID, OccurredOn: date("2026-06-15"),
		Quantity: decimal.NewFromInt(10), Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("arrival: %v", err)
	}
	if err := f.accStore.Archive(f.ctx, f.spaceID, f.account2ID); err != nil {
		t.Fatalf("archive: %v", err)
	}

	if err := o.state("2021-03-02"); err != nil {
		t.Errorf("state purchases = %v, want it written — the archived account holds none of the arrived shares", err)
	}
}

// A move whose basis the owner gave by hand carries no breakdown: nothing was
// released for it, and nothing is released again — the owner's figure stays.
func TestAMoveWithABasisGivenByHandKeepsIt(t *testing.T) {
	o := newOnward(t)
	byHand := int64(12_345)
	_, in := o.move(t, o.accountID, o.account2ID, 4, "2026-07-01", &byHand)

	if err := o.state("2021-03-02"); err != nil {
		t.Fatalf("state purchases: %v", err)
	}
	if got := o.amount(t, in.ID); got != byHand {
		t.Errorf("the arriving leg's basis = %d, want the owner's %d", got, byHand)
	}
	if p := o.position(t, o.account2ID); p == nil || p.CostMinor != byHand {
		t.Errorf("B: %+v, want a cost of %d", p, byHand)
	}
}

// A move made before the shares arrived cannot have carried them: it stays
// exactly as it was released, and the account it went to is not touched —
// archived since, it does not stop the statement.
func TestAMoveBeforeTheArrivalIsLeftAlone(t *testing.T) {
	f := newFixture(t)
	o := onward{fixture: f, svc: operation.NewService(f.store)}
	if _, err := o.svc.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-06-01"), Quantity: dec("5"), Price: dec("50"),
		AmountMinor: -25_000, Currency: "RUB",
	}); err != nil {
		t.Fatalf("buy: %v", err)
	}
	_, earlier := o.move(t, f.accountID, f.account2ID, 5, "2026-06-10", nil)
	var err error
	o.arrival, err = o.svc.CreateArrival(f.ctx, f.spaceID, operation.ArrivalParams{
		AccountID: f.accountID, InstrumentID: f.sberID, OccurredOn: date("2026-06-15"),
		Quantity: decimal.NewFromInt(10), Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("arrival: %v", err)
	}

	if err := f.accStore.Archive(f.ctx, f.spaceID, f.account2ID); err != nil {
		t.Fatalf("archive: %v", err)
	}

	if err := o.state("2021-03-02"); err != nil {
		t.Fatalf("state purchases: %v", err)
	}
	if got := o.amount(t, earlier.ID); got != 25_000 {
		t.Errorf("the earlier move's basis = %d, want the 25000 it was released at", got)
	}
	if days := lotDays(o.position(t, f.account2ID)); len(days) != 1 || days[0] != "2026-06-01" {
		t.Errorf("B's lots are dated %v, want the one bought on 2026-06-01", days)
	}
}

// A later move released the shares at the FIFO front as it stood then: the
// ten that arrived with no purchase day, which the queue puts ahead of the five
// bought here on 2026-06-01. Stating that the arrived shares were bought on
// 2026-06-05 puts the five bought here at the front instead — the queue is
// ordered by the day shares were bought — and the move is released again from
// the front as it now stands, the same rule a move made today would follow.
func TestAMoveIsReleasedAgainFromTheFrontAsItNowStands(t *testing.T) {
	f := newFixture(t)
	o := onward{fixture: f, svc: operation.NewService(f.store)}
	if _, err := o.svc.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-06-01"), Quantity: dec("5"), Price: dec("50"),
		AmountMinor: -25_000, Currency: "RUB",
	}); err != nil {
		t.Fatalf("buy: %v", err)
	}
	var err error
	o.arrival, err = o.svc.CreateArrival(f.ctx, f.spaceID, operation.ArrivalParams{
		AccountID: f.accountID, InstrumentID: f.sberID, OccurredOn: date("2026-06-15"),
		Quantity: decimal.NewFromInt(10), Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("arrival: %v", err)
	}
	_, in := o.move(t, f.accountID, f.account2ID, 5, "2026-07-01", nil)
	if got := o.amount(t, in.ID); got != 0 {
		t.Fatalf("the move took %d at first, want 0 — five of the undated arrival at the front", got)
	}

	if err := o.state("2026-06-05"); err != nil {
		t.Fatalf("state purchases: %v", err)
	}
	if got := o.amount(t, in.ID); got != 25_000 {
		t.Errorf("the move's basis = %d, want 25000 — the five bought on 2026-06-01, now at the front", got)
	}
	if days := lotDays(o.position(t, f.account2ID)); len(days) != 1 || days[0] != "2026-06-01" {
		t.Errorf("B's lots are dated %v, want the one bought on 2026-06-01", days)
	}
	if p := o.position(t, f.accountID); p == nil || p.CostMinor != 100_000 {
		t.Errorf("A: %+v, want the ten arrived left at 100 ₽, 100000", p)
	}
}

// A move an importer recorded is not released again; when the restated
// history cannot take it, the statement is refused with the engine's reason.
func TestAMoveAnImporterRecordedIsNotRewritten(t *testing.T) {
	o := newOnward(t)
	group := uuid.New()
	qty := dec("4")
	outID, inID := "move-out", "move-in"
	applied, refused, err := o.svc.ApplyImportDelta(o.ctx, o.spaceID, operation.ImportDelta{Add: []operation.Operation{
		{
			AccountID: o.accountID, InstrumentID: &o.sberID, Type: operation.TypeTransferOut,
			OccurredOn: date("2026-07-01"), Quantity: qty, Currency: "RUB",
			TransferGroupID: &group, Source: "tinvest", ExternalID: &outID,
		},
		{
			AccountID: o.account2ID, InstrumentID: &o.sberID, Type: operation.TypeTransferIn,
			OccurredOn: date("2026-07-01"), Quantity: qty, Currency: "RUB",
			TransferGroupID: &group, Source: "tinvest", ExternalID: &inID,
		},
	}})
	if err != nil || len(applied) != 2 || len(refused) != 0 {
		t.Fatalf("import the move: applied %d, refused %+v, err %v", len(applied), refused, err)
	}
	before := map[uuid.UUID]int64{}
	for _, op := range applied {
		before[op.ID] = op.AmountMinor
	}

	err = o.state("2021-03-02")
	if !errors.Is(err, operation.ErrInconsistent) {
		t.Fatalf("state purchases = %v, want the engine's refusal", err)
	}
	for id, amount := range before {
		if got := o.amount(t, id); got != amount {
			t.Errorf("imported leg %s amount = %d, want it untouched at %d", id, got, amount)
		}
	}
	if p := o.position(t, o.accountID); p == nil || p.CostMinor != 0 {
		t.Errorf("A: %+v, want the arrival untouched", p)
	}
}
