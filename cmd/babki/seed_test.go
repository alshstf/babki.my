package main

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/portfolio"
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

	// seeded instruments and operations produce live positions with realized
	// P&L and income — verified via the stores directly (positions are a
	// pure projection of the journal, computed by portfolio.Compute).
	var tbankID, freedomID uuid.UUID
	for _, a := range accounts {
		switch a.Name {
		case "Брокерский Т-Банк":
			tbankID = a.ID
		case "Freedom KZ":
			freedomID = a.ID
		}
	}
	if tbankID == uuid.Nil || freedomID == uuid.Nil {
		t.Fatalf("brokerage accounts not found among seeded accounts")
	}

	opStore := operation.NewStore(pool)
	instStore := instrument.NewStore(pool)

	positionsByTicker := func(accountID uuid.UUID) map[string]*portfolio.Position {
		ops, err := opStore.ListForEngine(ctx, p.SpaceID, accountID)
		if err != nil {
			t.Fatalf("ListForEngine: %v", err)
		}
		positions, err := portfolio.Compute(ops)
		if err != nil {
			t.Fatalf("Compute: %v", err)
		}
		out := make(map[string]*portfolio.Position, len(positions))
		for _, pos := range positions {
			inst, err := instStore.ByID(ctx, pos.InstrumentID)
			if err != nil {
				t.Fatalf("instrument ByID: %v", err)
			}
			out[inst.Ticker] = pos
		}
		return out
	}

	tbankPositions := positionsByTicker(tbankID)
	if len(tbankPositions) != 4 {
		t.Fatalf("Т-Банк positions = %d, want 4: %+v", len(tbankPositions), tbankPositions)
	}
	wantQty := map[string]string{"SBER": "300", "OFZ26238": "100", "FXUS": "30", "LKOH": "15"}
	for ticker, qty := range wantQty {
		pos, ok := tbankPositions[ticker]
		if !ok {
			t.Fatalf("missing Т-Банк position %s", ticker)
		}
		if pos.Quantity.String() != qty {
			t.Errorf("Т-Банк %s quantity = %s, want %s", ticker, pos.Quantity.String(), qty)
		}
	}
	if lkoh := tbankPositions["LKOH"]; lkoh.RealizedPnLMinor <= 0 {
		t.Errorf("LKOH realized P&L = %d, want > 0", lkoh.RealizedPnLMinor)
	}

	freedomPositions := positionsByTicker(freedomID)
	if len(freedomPositions) != 1 {
		t.Fatalf("Freedom positions = %d, want 1: %+v", len(freedomPositions), freedomPositions)
	}
	if aapl, ok := freedomPositions["AAPL"]; !ok {
		t.Fatal("missing Freedom position AAPL")
	} else if aapl.Quantity.String() != "20" {
		t.Errorf("AAPL quantity = %s, want 20", aapl.Quantity.String())
	}

	// second run refuses (instance not empty)
	if err := seedDemo(ctx, pool); err == nil {
		t.Fatal("second seedDemo: want error")
	}
}
