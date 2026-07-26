package account_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/testdb"
)

// newStore prepares a migrated DB with one space and returns its id.
func newStore(t *testing.T) (*account.Store, uuid.UUID, context.Context) {
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
	return account.NewStore(pool), sp.ID, ctx
}

func date(s string) time.Time {
	d, _ := time.Parse("2006-01-02", s)
	return d
}

func TestAccountLifecycleAndBalances(t *testing.T) {
	st, spaceID, ctx := newStore(t)

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
	st, spaceID, ctx := newStore(t)

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
