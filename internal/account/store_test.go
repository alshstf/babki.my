package account_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/testdb"
)

// newStore prepares a migrated DB with one space and returns store, spaceID, ownerID, and context.
func newStore(t *testing.T) (*account.Store, uuid.UUID, uuid.UUID, context.Context) {
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
	return account.NewStore(pool), sp.ID, u.ID, ctx
}

func date(s string) time.Time {
	d, _ := time.Parse("2006-01-02", s)
	return d
}

func TestAccountLifecycleAndBalances(t *testing.T) {
	st, spaceID, _, ctx := newStore(t)

	acc, err := st.Create(ctx, spaceID, nil, "Брокерский Т-Банк", account.TypeBrokerage, "RUB", "Т-Банк")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// balance upsert: two dates + overwrite same date
	for _, c := range []struct {
		d string
		v int64
	}{{"2026-07-01", 100_000_00}, {"2026-07-15", 110_000_00}, {"2026-07-15", 112_000_00}} {
		if err := st.SetBalance(ctx, spaceID, acc.ID, date(c.d), c.v); err != nil {
			t.Fatalf("SetBalance(%s): %v", c.d, err)
		}
	}

	got, err := st.ByID(ctx, spaceID, acc.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if got.Balance == nil || got.Balance.AmountMinor != 112_000_00 ||
		got.Balance.AsOf.Format("2006-01-02") != "2026-07-15" {
		t.Fatalf("latest balance = %+v", got.Balance)
	}

	// update + archive
	newName := "Брокер Т"
	upd, err := st.Update(ctx, spaceID, acc.ID, account.Update{Name: &newName})
	if err != nil || upd.Name != "Брокер Т" {
		t.Fatalf("Update: %v %+v", err, upd)
	}
	if err := st.Archive(ctx, spaceID, acc.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	got, _ = st.ByID(ctx, spaceID, acc.ID)
	if got.Status != account.StatusArchived {
		t.Errorf("status = %s", got.Status)
	}

	// space isolation: foreign space sees nothing
	otherSpace := uuid.New()
	if _, err := st.ByID(ctx, otherSpace, acc.ID); err == nil {
		t.Error("ByID from foreign space: want error")
	}
	if err := st.SetBalance(ctx, otherSpace, acc.ID, date("2026-07-16"), 1); err == nil {
		t.Error("SetBalance from foreign space: want error")
	}
}

func TestSummaryByCurrency(t *testing.T) {
	st, spaceID, _, ctx := newStore(t)

	mk := func(name string, typ account.Type, cur string, bal int64) {
		a, err := st.Create(ctx, spaceID, nil, name, typ, cur, "")
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if err := st.SetBalance(ctx, spaceID, a.ID, date("2026-07-20"), bal); err != nil {
			t.Fatalf("balance %s: %v", name, err)
		}
	}
	mk("Брокер", account.TypeBrokerage, "RUB", 1_500_000_00)
	mk("Текущий", account.TypeChecking, "RUB", 200_000_00)
	mk("Кредитка", account.TypeCreditCard, "RUB", -85_000_00)
	mk("Кэш", account.TypeCash, "EUR", 1_000_00)

	// archived accounts are excluded from summary
	arch, _ := st.Create(ctx, spaceID, nil, "Старый", account.TypeChecking, "RUB", "")
	_ = st.SetBalance(ctx, spaceID, arch.ID, date("2026-07-20"), 999_00)
	_ = st.Archive(ctx, spaceID, arch.ID)

	totals, err := st.SummaryByCurrency(ctx, spaceID)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(totals) != 2 {
		t.Fatalf("totals = %+v, want 2 currencies", totals)
	}
	// sorted by currency: EUR, RUB
	eur, rub := totals[0], totals[1]
	if eur.Currency != "EUR" || eur.AssetsMinor != 1_000_00 || eur.NetMinor != 1_000_00 {
		t.Errorf("EUR = %+v", eur)
	}
	if rub.Currency != "RUB" || rub.AssetsMinor != 1_700_000_00 ||
		rub.LiabilitiesMinor != -85_000_00 || rub.NetMinor != 1_615_000_00 {
		t.Errorf("RUB = %+v", rub)
	}
}

// TestSummarySplitsEveryTypeTheWayIsLiabilitySays walks ALL SEVEN account
// types through the summary in one currency and checks each one landed on the
// side Type.IsLiability puts it on. It exists because that split lives in SQL,
// where the Go method cannot be called: the query takes the list as a
// parameter built by LiabilityTypes, and this is what would notice if the two
// ever stopped agreeing — an account created, listed and edited perfectly, and
// silently summed on the wrong side.
//
// The classification is WRITTEN OUT rather than derived from IsLiability, on
// purpose: a table that asked the method what to expect would move with any
// change to it and could never fail. Written out, it fails twice over —
// LiabilityTypes stops matching the literal pair below, and the account whose
// class changed lands in the wrong total.
//
// Every balance is a different number, so a type counted on the wrong side
// moves both totals by an amount that names it.
func TestSummarySplitsEveryTypeTheWayIsLiabilitySays(t *testing.T) {
	st, spaceID, _, ctx := newStore(t)

	if got, want := account.LiabilityTypes(), []string{"credit_card", "loan"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("LiabilityTypes() = %v, want %v", got, want)
	}

	cases := []struct {
		typ       account.Type
		liability bool
		balance   int64
	}{
		{account.TypeBrokerage, false, 1_000_00},
		{account.TypeChecking, false, 2_000_00},
		{account.TypeSavings, false, 4_000_00},
		{account.TypeDeposit, false, 8_000_00},
		{account.TypeCash, false, 16_000_00},
		{account.TypeCreditCard, true, -32_000_00},
		{account.TypeLoan, true, -64_000_00},
	}
	var wantAssets, wantLiabilities int64
	for _, c := range cases {
		if !c.typ.Valid() {
			t.Fatalf("%q is not a valid account type", c.typ)
		}
		if c.typ.IsLiability() != c.liability {
			t.Fatalf("%q.IsLiability() = %v, want %v", c.typ, c.typ.IsLiability(), c.liability)
		}
		a, err := st.Create(ctx, spaceID, nil, string(c.typ), c.typ, "RUB", "")
		if err != nil {
			t.Fatalf("create %s: %v", c.typ, err)
		}
		if err := st.SetBalance(ctx, spaceID, a.ID, date("2026-07-20"), c.balance); err != nil {
			t.Fatalf("balance %s: %v", c.typ, err)
		}
		if c.liability {
			wantLiabilities += c.balance
		} else {
			wantAssets += c.balance
		}
	}

	totals, err := st.SummaryByCurrency(ctx, spaceID)
	if err != nil {
		t.Fatalf("SummaryByCurrency: %v", err)
	}
	if len(totals) != 1 {
		t.Fatalf("totals = %+v, want one currency", totals)
	}
	got := totals[0]
	if got.AssetsMinor != wantAssets {
		t.Errorf("assets = %d, want %d (every non-liability type, summed)", got.AssetsMinor, wantAssets)
	}
	if got.LiabilitiesMinor != wantLiabilities {
		t.Errorf("liabilities = %d, want %d (credit card + loan, summed)", got.LiabilitiesMinor, wantLiabilities)
	}
	if got.NetMinor != wantAssets+wantLiabilities {
		t.Errorf("net = %d, want %d", got.NetMinor, wantAssets+wantLiabilities)
	}
}

func TestListWithBalanceOrdering(t *testing.T) {
	st, spaceID, _, ctx := newStore(t)

	// Create three accounts with different types
	_, _ = st.Create(ctx, spaceID, nil, "Гамма", account.TypeChecking, "RUB", "")
	alpha, _ := st.Create(ctx, spaceID, nil, "Альфа", account.TypeBrokerage, "RUB", "")
	beta, _ := st.Create(ctx, spaceID, nil, "Бета", account.TypeCash, "RUB", "")

	// Archive Альфа
	if err := st.Archive(ctx, spaceID, alpha.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// Set balance on Бета only
	if err := st.SetBalance(ctx, spaceID, beta.ID, date("2026-07-20"), 50_000_00); err != nil {
		t.Fatalf("SetBalance: %v", err)
	}

	// List with balance
	list, err := st.ListWithBalance(ctx, spaceID)
	if err != nil {
		t.Fatalf("ListWithBalance: %v", err)
	}

	// Expect: active sorted by name (Бета, Гамма), then archived (Альфа)
	if len(list) != 3 {
		t.Fatalf("expected 3 accounts, got %d", len(list))
	}

	// Check ordering and balance
	if list[0].Name != "Бета" || list[0].Status != account.StatusActive {
		t.Errorf("list[0] = %q status=%s", list[0].Name, list[0].Status)
	}
	if list[0].Balance == nil || list[0].Balance.AmountMinor != 50_000_00 {
		t.Errorf("list[0].Balance = %+v, want 50_000_00", list[0].Balance)
	}

	if list[1].Name != "Гамма" || list[1].Status != account.StatusActive {
		t.Errorf("list[1] = %q status=%s", list[1].Name, list[1].Status)
	}
	if list[1].Balance != nil {
		t.Errorf("list[1].Balance = %+v, want nil", list[1].Balance)
	}

	if list[2].Name != "Альфа" || list[2].Status != account.StatusArchived {
		t.Errorf("list[2] = %q status=%s", list[2].Name, list[2].Status)
	}
	if list[2].Balance != nil {
		t.Errorf("list[2].Balance = %+v, want nil", list[2].Balance)
	}
}

func TestUpdateOwnerTriState(t *testing.T) {
	st, spaceID, ownerID, ctx := newStore(t)

	// Create account
	created, _ := st.Create(ctx, spaceID, nil, "Test", account.TypeChecking, "RUB", "")
	accountID := created.ID

	// (a) Update with OwnerUserID unset (nil) leaves owner unchanged
	wb, _ := st.Update(ctx, spaceID, accountID, account.Update{})
	if wb.OwnerUserID != nil {
		t.Errorf("(a) after nil update, OwnerUserID = %v, want nil", wb.OwnerUserID)
	}

	// (b) Set: Update{OwnerUserID: &ptrToOwnerID} → OwnerUserID == ownerID
	upd := account.Update{}
	p := &ownerID
	upd.OwnerUserID = &p
	wb, _ = st.Update(ctx, spaceID, accountID, upd)
	if wb.OwnerUserID == nil || *wb.OwnerUserID != ownerID {
		t.Errorf("(b) after set, OwnerUserID = %v, want %v", wb.OwnerUserID, ownerID)
	}

	// (c) Clear: var null *uuid.UUID; Update{OwnerUserID: &null} → OwnerUserID == nil
	upd2 := account.Update{}
	var null *uuid.UUID
	upd2.OwnerUserID = &null
	wb, _ = st.Update(ctx, spaceID, accountID, upd2)
	if wb.OwnerUserID != nil {
		t.Errorf("(c) after clear, OwnerUserID = %v, want nil", wb.OwnerUserID)
	}
}

func TestDistinctCurrencies(t *testing.T) {
	st, spaceID, _, ctx := newStore(t)

	if _, err := st.Create(ctx, spaceID, nil, "A1", account.TypeChecking, "RUB", ""); err != nil {
		t.Fatalf("create A1: %v", err)
	}
	if _, err := st.Create(ctx, spaceID, nil, "A2", account.TypeChecking, "RUB", ""); err != nil {
		t.Fatalf("create A2: %v", err)
	}
	if _, err := st.Create(ctx, spaceID, nil, "A3", account.TypeCash, "EUR", ""); err != nil {
		t.Fatalf("create A3: %v", err)
	}

	got, err := st.DistinctCurrencies(ctx)
	if err != nil {
		t.Fatalf("DistinctCurrencies: %v", err)
	}
	want := []string{"EUR", "RUB"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DistinctCurrencies = %v, want %v", got, want)
	}
}

func TestDistinctCurrenciesEmpty(t *testing.T) {
	st, _, _, ctx := newStore(t)

	got, err := st.DistinctCurrencies(ctx)
	if err != nil {
		t.Fatalf("DistinctCurrencies on empty table: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("DistinctCurrencies on empty table = %v, want empty, got %v", got, got)
	}
}

func TestUpdateMissingAccountSameSpace(t *testing.T) {
	st, spaceID, _, ctx := newStore(t)

	// Try to update non-existent account in same space
	nonExistent := uuid.New()
	newName := "New"
	_, err := st.Update(ctx, spaceID, nonExistent, account.Update{Name: &newName})
	if err != pgx.ErrNoRows {
		t.Errorf("Update non-existent: want pgx.ErrNoRows, got %v", err)
	}

	// Try to archive non-existent account in same space
	err = st.Archive(ctx, spaceID, nonExistent)
	if err != pgx.ErrNoRows {
		t.Errorf("Archive non-existent: want pgx.ErrNoRows, got %v", err)
	}
}
