package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
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

	// seeded fx rates let the converter bridge 100 USD into RUB at the
	// seeded rate (78.50): 100 USD = 10000 minor units -> 785000 minor
	// units = 7850.00 RUB.
	converter := marketdata.NewConverter(marketdata.NewStore(pool))
	on := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	got, err := converter.Convert(ctx, 100_00, "USD", "RUB", on)
	if err != nil {
		t.Fatalf("Convert(100 USD -> RUB): %v", err)
	}
	if got != 785_000 {
		t.Errorf("Convert(100 USD -> RUB) = %d, want 785000 (7850.00 RUB)", got)
	}

	// every currency the demo space holds (RUB, USD) now has a seeded rate
	// into the space's base currency (RUB, the default), so GET /summary's
	// total_in_base_minor comes out nonzero with nothing left unconverted —
	// this mirrors handleSummary's own zero-filter + ConvertMany call.
	netByCurrency := make(map[string]int64, len(totals))
	for _, ct := range totals {
		if ct.NetMinor != 0 {
			netByCurrency[ct.Currency] = ct.NetMinor
		}
	}
	converted, missing, ratesOn, err := converter.ConvertMany(ctx, netByCurrency, "RUB", on)
	if err != nil {
		t.Fatalf("ConvertMany: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("ConvertMany missing = %v, want empty", missing)
	}
	if converted == 0 {
		t.Errorf("ConvertMany total = 0, want nonzero")
	}
	// USD is the only non-RUB currency in netByCurrency, seeded with a rate
	// exactly on 2026-07-20 (== on), so that's the oldest (and only) rate
	// used.
	if !ratesOn.Equal(on) {
		t.Errorf("ConvertMany ratesOn = %v, want %v (seeded USD/RUB rate date)", ratesOn, on)
	}

	// SBER has a seeded quote, so its position in Т-Банк carries a market
	// valuation — the same LatestQuotes + marketValue path GET
	// .../positions uses (internal/portfolio/http.go).
	sber, ok := tbankPositions["SBER"]
	if !ok {
		t.Fatal("missing Т-Банк position SBER")
	}
	sberQuote, err := marketdata.NewStore(pool).QuoteOn(ctx, sber.InstrumentID, on)
	if err != nil {
		t.Fatalf("QuoteOn SBER: %v", err)
	}
	if want := decimal.RequireFromString("305.50"); !sberQuote.Price.Equal(want) {
		t.Errorf("SBER quote price = %s, want %s", sberQuote.Price.String(), want.String())
	}

	// second run refuses (instance not empty)
	if err := seedDemo(ctx, pool); err == nil {
		t.Fatal("second seedDemo: want error")
	}
}
