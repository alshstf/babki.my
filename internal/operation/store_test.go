package operation_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/testdb"
)

type fixture struct {
	store      *operation.Store
	spaceID    uuid.UUID
	accountID  uuid.UUID
	account2ID uuid.UUID
	sberID     uuid.UUID
	ctx        context.Context
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
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
	a1, err := acc.Create(ctx, sp.ID, nil, "Брокер", account.TypeBrokerage, "RUB", "")
	if err != nil {
		t.Fatalf("acc1: %v", err)
	}
	a2, err := acc.Create(ctx, sp.ID, nil, "Брокер 2", account.TypeBrokerage, "RUB", "")
	if err != nil {
		t.Fatalf("acc2: %v", err)
	}
	sber, err := instrument.NewStore(pool).Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("sber: %v", err)
	}
	return fixture{
		store: operation.NewStore(pool), spaceID: sp.ID,
		accountID: a1.ID, account2ID: a2.ID, sberID: sber.ID, ctx: ctx,
	}
}

func date(s string) time.Time {
	d, _ := time.Parse("2006-01-02", s)
	return d
}

func dec(s string) *decimal.Decimal {
	d := decimal.RequireFromString(s)
	return &d
}

func TestCreateListDelete(t *testing.T) {
	f := newFixture(t)

	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("305.5"),
		AmountMinor: -305_500, Currency: "RUB", FeeMinor: 92,
	}
	created, err := f.store.Create(f.ctx, f.spaceID, buy)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == uuid.Nil || !created.Quantity.Equal(decimal.RequireFromString("10")) {
		t.Fatalf("created = %+v", created)
	}

	// foreign space rejected
	if _, err := f.store.Create(f.ctx, uuid.New(), buy); err == nil {
		t.Fatal("foreign space Create: want error")
	}

	// list DESC
	dep := operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit,
		OccurredOn: date("2026-07-05"), AmountMinor: 100_000_00, Currency: "RUB",
	}
	if _, err := f.store.Create(f.ctx, f.spaceID, dep); err != nil {
		t.Fatalf("Create dep: %v", err)
	}
	list, err := f.store.ListByAccount(f.ctx, f.spaceID, f.accountID, 10, 0)
	if err != nil || len(list) != 2 || list[0].Type != operation.TypeDeposit {
		t.Fatalf("ListByAccount = %+v, %v", list, err)
	}
	// engine order ASC
	asc, err := f.store.ListForEngine(f.ctx, f.spaceID, f.accountID)
	if err != nil || len(asc) != 2 || asc[0].Type != operation.TypeBuy {
		t.Fatalf("ListForEngine = %+v, %v", asc, err)
	}

	// delete
	if n, err := f.store.Delete(f.ctx, f.spaceID, created.ID); err != nil || n != 1 {
		t.Fatalf("Delete = %d, %v", n, err)
	}
	if list, _ = f.store.ListByAccount(f.ctx, f.spaceID, f.accountID, 10, 0); len(list) != 1 {
		t.Fatalf("after delete = %d", len(list))
	}
}

func TestTransferPairAtomicity(t *testing.T) {
	f := newFixture(t)

	out := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeTransferOut,
		OccurredOn: date("2026-07-10"), Quantity: dec("5"), AmountMinor: 150_000, Currency: "RUB",
	}
	in := operation.Operation{
		AccountID: f.account2ID, InstrumentID: &f.sberID, Type: operation.TypeTransferIn,
		OccurredOn: date("2026-07-10"), Quantity: dec("5"), AmountMinor: 150_000, Currency: "RUB",
	}
	cOut, cIn, err := f.store.CreatePair(f.ctx, f.spaceID, out, in)
	if err != nil {
		t.Fatalf("CreatePair: %v", err)
	}
	if cOut.TransferGroupID == nil || cIn.TransferGroupID == nil ||
		*cOut.TransferGroupID != *cIn.TransferGroupID {
		t.Fatalf("group ids: %+v %+v", cOut.TransferGroupID, cIn.TransferGroupID)
	}

	// deleting one deletes the whole group
	if n, err := f.store.Delete(f.ctx, f.spaceID, cIn.ID); err != nil || n != 2 {
		t.Fatalf("Delete group = %d, %v", n, err)
	}

	// pair with a foreign-space destination is fully rejected (atomicity)
	in.AccountID = uuid.New()
	if _, _, err := f.store.CreatePair(f.ctx, f.spaceID, out, in); err == nil {
		t.Fatal("CreatePair foreign dest: want error")
	}
	if list, _ := f.store.ListByAccount(f.ctx, f.spaceID, f.accountID, 10, 0); len(list) != 0 {
		t.Fatalf("orphan out op left: %d", len(list))
	}
}

func TestExternalIDDedup(t *testing.T) {
	f := newFixture(t)
	ext := "broker-op-1"
	op := operation.Operation{
		AccountID: f.accountID, Type: operation.TypeDeposit,
		OccurredOn: date("2026-07-01"), AmountMinor: 1000, Currency: "RUB",
		Source: "csv", ExternalID: &ext,
	}
	if _, err := f.store.Create(f.ctx, f.spaceID, op); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := f.store.Create(f.ctx, f.spaceID, op); err == nil {
		t.Fatal("duplicate external_id: want error")
	}
}
