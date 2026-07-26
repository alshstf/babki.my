package main

import (
	"context"
	"testing"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/testdb"
)

func TestSeedDemo(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if err := seedDemo(ctx, pool); err != nil {
		t.Fatalf("seedDemo: %v", err)
	}

	// demo user can log in and sees the seeded world
	svc := family.NewService(family.NewStore(pool))
	_, p, err := svc.Login(ctx, "demo", "demo1234")
	if err != nil || p.Role != family.RoleOwner {
		t.Fatalf("login demo: %v %+v", err, p)
	}

	accounts, err := account.NewStore(pool).ListWithBalance(ctx, p.SpaceID)
	if err != nil || len(accounts) != 6 {
		t.Fatalf("accounts = %d, %v; want 6", len(accounts), err)
	}
	for _, a := range accounts {
		if a.Balance == nil {
			t.Errorf("account %q has no balance", a.Name)
		}
	}

	totals, err := account.NewStore(pool).SummaryByCurrency(ctx, p.SpaceID)
	if err != nil || len(totals) != 2 {
		t.Fatalf("totals = %+v, %v; want RUB+USD", totals, err)
	}

	// second run refuses (instance not empty)
	if err := seedDemo(ctx, pool); err == nil {
		t.Fatal("second seedDemo: want error")
	}
}
