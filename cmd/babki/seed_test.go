package main

import (
	"context"
	"errors"
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

// mustAcquired asserts that a lot (or a piece of a transfer's breakdown) knows
// when it was acquired, and returns that date. Nil is a legitimate value in
// general — a transfer with no recoverable purchase dates produces lots that
// carry none (see portfolio.Lot.AcquiredOn) — but not anywhere in this seed:
// every lot here comes from a real buy or from the TSLA transfer that carries
// those buys' own dates across, and that survival is exactly what the demo
// exists to show. An unknown date here means the seed has stopped demonstrating
// it, so this fails rather than skipping the arithmetic.
func mustAcquired(t *testing.T, on *time.Time, what string) time.Time {
	t.Helper()
	if on == nil {
		t.Fatalf("%s has an unknown acquisition date, want a real one: this seed's dates all come from actual purchases", what)
	}
	return *on
}

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
	if len(tbankPositions) != 6 {
		t.Fatalf("Т-Банк positions = %d, want 6: %+v", len(tbankPositions), tbankPositions)
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
	// TSLA was bought entirely at Т-Банк and then transferred whole to
	// Freedom KZ (see the transfer arithmetic below): the source keeps the
	// position as closed history — zero quantity, zero cost, no lots left —
	// rather than dropping it, mirroring
	// TestPositionInBaseTransferredLotsKeepTheirPurchaseDates's own source
	// check.
	tsla, ok := tbankPositions["TSLA"]
	if !ok {
		t.Fatal("missing Т-Банк position TSLA (closed by the transfer)")
	}
	if tsla.Quantity.String() != "0" || tsla.CostMinor != 0 || len(tsla.Lots) != 0 {
		t.Errorf("Т-Банк TSLA after transferring everything = {qty %s cost %d lots %d}, want {0 0 []}",
			tsla.Quantity.String(), tsla.CostMinor, len(tsla.Lots))
	}

	freedomPositions := positionsByTicker(freedomID)
	if len(freedomPositions) != 4 {
		t.Fatalf("Freedom positions = %d, want 4 (AAPL, MSFT, NVDA, TSLA): %+v", len(freedomPositions), freedomPositions)
	}
	aapl, ok := freedomPositions["AAPL"]
	if !ok {
		t.Fatal("missing Freedom position AAPL")
	}
	if aapl.Quantity.String() != "30" {
		t.Errorf("AAPL quantity = %s, want 30 (10 + 20, two buys)", aapl.Quantity.String())
	}
	if len(aapl.Lots) != 2 {
		t.Errorf("AAPL lots = %d, want 2 — the two buys must stay two lots with two acquisition dates", len(aapl.Lots))
	}

	// TSLA is this seed's demonstration of plan 7a: the position arrives at
	// Freedom KZ entirely by transfer, and the two source lots — bought on
	// different days at different fx rates — must still be two lots here,
	// each keeping the day it was ACTUALLY bought rather than the day it
	// changed brokers. The shape is pinned now; the ruble arithmetic (which
	// needs the fx converter, set up below) is pinned further down, right
	// after rateToday — see the block near the MSFT arithmetic.
	tsla, ok = freedomPositions["TSLA"]
	if !ok {
		t.Fatal("missing Freedom position TSLA")
	}
	if tsla.Quantity.String() != "10" {
		t.Errorf("TSLA quantity = %s, want 10 (5 + 5, transferred whole)", tsla.Quantity.String())
	}
	if tsla.CostMinor != 190_000 {
		t.Errorf("TSLA cost_minor = %d, want 190000 ($1900.00, transfer moves the basis, does not change it)", tsla.CostMinor)
	}
	if len(tsla.Lots) != 2 {
		t.Fatalf("TSLA lots = %d, want 2 — the transfer must carry over both source lots, not collapse them into one", len(tsla.Lots))
	}
	wantLotDates := map[string]bool{"2026-05-13": false, "2026-06-15": false}
	for _, l := range tsla.Lots {
		dateStr := mustAcquired(t, l.AcquiredOn, "a TSLA lot").Format(time.DateOnly)
		if _, known := wantLotDates[dateStr]; !known {
			t.Fatalf("TSLA lot acquired on unexpected date %s, want one of 2026-05-13 or 2026-06-15", dateStr)
		}
		wantLotDates[dateStr] = true
	}
	for dateStr, seen := range wantLotDates {
		if !seen {
			t.Errorf("TSLA lots missing one acquired on %s — the transfer re-dated it instead of carrying it over", dateStr)
		}
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

	// USD/RUB is seeded as a HISTORY, not a single date, and the demo's
	// screens depend on the shape of that history rather than on any one
	// number in it: the journal converts every foreign-currency entry at the
	// rate of its own date. These three cases are exactly what the demo is
	// supposed to show on screen, so they are pinned here — a future seed
	// edit that flattens the history would otherwise silently turn the demo
	// back into "everything at today's rate" with all tests still green.
	day := func(s string) time.Time {
		t.Helper()
		parsed, err := time.Parse(time.DateOnly, s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		return parsed
	}
	// (a) an operation date with a rate of its own converts at that rate.
	rateOnBuy, dateOnBuy, err := converter.Rate(ctx, "USD", "RUB", day("2026-05-20"))
	if err != nil {
		t.Fatalf("Rate(USD -> RUB, 2026-05-20): %v", err)
	}
	if want := decimal.RequireFromString("79.15"); !rateOnBuy.Equal(want) {
		t.Errorf("USD/RUB on 2026-05-20 = %s, want %s", rateOnBuy, want)
	}
	if !dateOnBuy.Equal(day("2026-05-20")) {
		t.Errorf("USD/RUB rate date for 2026-05-20 = %s, want the same day", dateOnBuy.Format(time.DateOnly))
	}
	// (b) a weekend operation date has no rate of its own and falls back to
	// the preceding business day, which the journal then discloses.
	rateOnSat, dateOnSat, err := converter.Rate(ctx, "USD", "RUB", day("2026-07-04"))
	if err != nil {
		t.Fatalf("Rate(USD -> RUB, 2026-07-04): %v", err)
	}
	if want := decimal.RequireFromString("77.90"); !rateOnSat.Equal(want) {
		t.Errorf("USD/RUB on 2026-07-04 = %s, want %s (Friday's rate)", rateOnSat, want)
	}
	if !dateOnSat.Equal(day("2026-07-03")) {
		t.Errorf("USD/RUB rate date for 2026-07-04 = %s, want 2026-07-03", dateOnSat.Format(time.DateOnly))
	}
	// (c) the demo's earliest USD operation predates all seeded rates, so it
	// has none — the honest-degradation path the owner must be able to see.
	if _, _, err := converter.Rate(ctx, "USD", "RUB", day("2026-05-06")); !errors.Is(err, marketdata.ErrNoRate) {
		t.Errorf("Rate(USD -> RUB, 2026-05-06) error = %v, want ErrNoRate", err)
	}
	// (d) that same gap swallows one of AAPL's two lots, so the position as a
	// whole has no ruble figures to publish — the position-level twin of (c),
	// and the reason the demo can show what "no rate for one lot" looks like
	// on a real row instead of only in portfolio's unit tests.
	lotsWithoutRate := 0
	for _, l := range aapl.Lots {
		if _, _, err := converter.Rate(ctx, "USD", "RUB", mustAcquired(t, l.AcquiredOn, "an AAPL lot")); errors.Is(err, marketdata.ErrNoRate) {
			lotsWithoutRate++
		}
	}
	if lotsWithoutRate != 1 {
		t.Errorf("AAPL lots with no fx rate on their acquisition date = %d, want exactly 1 — seeding a rate for the early buy would remove the demo's only position that honestly refuses to convert", lotsWithoutRate)
	}

	// The demo must contain one position whose unrealized profit has a
	// DIFFERENT SIGN in its own currency and in rubles. That is the whole
	// consequence of the owner's decision (2026-07-29) — ruble return carries
	// the currency's own move, position-currency return does not — and
	// without such a position in the seed it cannot be seen on demo data at
	// all, only asserted in portfolio's unit tests.
	//
	// The arithmetic is redone here from the seeded ingredients (lot dates,
	// the rate on each of those dates, today's rate, the quote) rather than
	// borrowed from the handler, so a seed edit that quietly flattens the
	// story — a different quote, a lot moved to another date, a rate nudged —
	// fails here rather than on the owner's screen:
	//
	//	cost    1_000_000 minor USD × 81.40 (the lot's own day) = 81_400_000 ₽
	//	value   1_020_000 minor USD × 78.50 (today)             = 80_070_000 ₽
	//	USD profit = 1_020_000 − 1_000_000 =    +20_000 (a gain)
	//	RUB profit = 80_070_000 − 81_400_000 = −1_330_000 (a loss)
	msft, ok := freedomPositions["MSFT"]
	if !ok {
		t.Fatal("missing Freedom position MSFT")
	}
	if len(msft.Lots) != 1 {
		t.Fatalf("MSFT lots = %d, want 1 — the sign flip is stated for a single-lot position", len(msft.Lots))
	}
	// "Today" is any date past the newest seeded rate, so this resolves to the
	// same 78.50 the running instance uses, without depending on the clock.
	rateToday, dateToday, err := converter.Rate(ctx, "USD", "RUB", day("2099-01-01"))
	if err != nil {
		t.Fatalf("Rate(USD -> RUB, today): %v", err)
	}
	if !dateToday.Equal(on) {
		t.Errorf("newest USD/RUB rate is dated %s, want %s — the sign flip below is struck against the last rate in the table", dateToday.Format(time.DateOnly), on.Format(time.DateOnly))
	}
	rateOnLot, _, err := converter.Rate(ctx, "USD", "RUB", mustAcquired(t, msft.Lots[0].AcquiredOn, "the MSFT lot"))
	if err != nil {
		t.Fatalf("Rate(USD -> RUB, MSFT lot date): %v", err)
	}
	msftQuote, err := marketdata.NewStore(pool).QuoteOn(ctx, msft.InstrumentID, on)
	if err != nil {
		t.Fatalf("QuoteOn MSFT: %v", err)
	}
	// Same expression portfolio.marketValue uses for a share: price × quantity,
	// shifted into minor units.
	marketUSD := msftQuote.Price.Mul(msft.Quantity).Shift(2).Round(0).IntPart()
	costRUB := decimal.NewFromInt(msft.CostMinor).Mul(rateOnLot).Round(0).IntPart()
	marketRUB := decimal.NewFromInt(marketUSD).Mul(rateToday).Round(0).IntPart()
	profitUSD := marketUSD - msft.CostMinor
	profitRUB := marketRUB - costRUB

	if msft.CostMinor != 1_000_000 || marketUSD != 1_020_000 {
		t.Errorf("MSFT cost/value in USD = %d/%d, want 1000000/1020000", msft.CostMinor, marketUSD)
	}
	if costRUB != 81_400_000 || marketRUB != 80_070_000 {
		t.Errorf("MSFT cost/value in RUB = %d/%d, want 81400000 (1000000 × 81.40) / 80070000 (1020000 × 78.50)", costRUB, marketRUB)
	}
	if profitUSD != 20_000 || profitRUB != -1_330_000 {
		t.Errorf("MSFT profit = %d USD / %d RUB, want +20000 / -1330000", profitUSD, profitRUB)
	}
	if profitUSD <= 0 || profitRUB >= 0 {
		t.Errorf("MSFT profit = %d in USD and %d in RUB: the demo must hold one position that is in profit in its own currency and at a loss in rubles at the same moment — that is what a seeded instance has to make visible", profitUSD, profitRUB)
	}
	// The number the pre-plan-6 semantics would have shown, named so a
	// regression to "whole basis at today's rate" is unmistakable here.
	if oldCostRUB := decimal.NewFromInt(msft.CostMinor).Mul(rateToday).Round(0).IntPart(); marketRUB-oldCostRUB <= 0 {
		t.Errorf("basis at today's rate = %d gives a ruble profit of %d: the seed no longer distinguishes the historical basis from the current one, and the demo has nothing left to show", oldCostRUB, marketRUB-oldCostRUB)
	}

	// TSLA's ruble arithmetic, plan 7a's own demonstration: the position
	// arrived at Freedom KZ entirely by transfer, and each of its two lots
	// must be converted at the rate of the day it was ACTUALLY bought, not
	// the day it changed brokers (2026-07-20). Redone from the seeded
	// ingredients the same way MSFT's is above, so a seed edit that quietly
	// re-dates or re-rates a lot fails here instead of only looking slightly
	// off on the owner's screen.
	//
	//	lot 1: 5 @ $180.00 on 2026-05-13 -> 90_000 minor USD, rate 60.00 -> 5_400_000
	//	lot 2: 5 @ $200.00 on 2026-06-15 -> 100_000 minor USD, rate 64.00 -> 6_400_000
	//	correct in_base.cost_minor = 5_400_000 + 6_400_000 = 11_800_000 (118 000,00 ₽)
	//	transfer-date (2026-07-20, rate 78.50) collapse instead:
	//	  190_000 * 78.50 = 14_915_000 (149 150,00 ₽) — 31_150,00 ₽ too much
	var correctBaseCost int64
	for _, l := range tsla.Lots {
		lotOn := mustAcquired(t, l.AcquiredOn, "a TSLA lot")
		rate, _, err := converter.Rate(ctx, "USD", "RUB", lotOn)
		if err != nil {
			t.Fatalf("Rate(USD -> RUB, TSLA lot date %s): %v", lotOn.Format(time.DateOnly), err)
		}
		correctBaseCost += decimal.NewFromInt(l.CostMinor).Mul(rate).Round(0).IntPart()
	}
	if correctBaseCost != 11_800_000 {
		t.Errorf("TSLA in_base.cost_minor (per lot's own rate) = %d, want 11800000 (118 000,00 ₽)", correctBaseCost)
	}
	if collapsedBaseCost := decimal.NewFromInt(tsla.CostMinor).Mul(rateToday).Round(0).IntPart(); collapsedBaseCost <= correctBaseCost {
		t.Errorf("whole-basis-at-transfer-date TSLA cost = %d, want > %d (118 000,00 ₽) — the seed's point is that collapsing to the transfer day OVERVALUES this position",
			collapsedBaseCost, correctBaseCost)
	} else if collapsedBaseCost != 14_915_000 {
		t.Errorf("whole-basis-at-transfer-date TSLA cost = %d, want 14915000 (149 150,00 ₽ = 190000 * 78.50)", collapsedBaseCost)
	}

	// The same arithmetic has to come out of the SOURCE account's journal row,
	// not just the destination's position: the departing leg carries the same
	// breakdown (see operation.Store.attachTransferLots), so the journal at
	// Т-Банк converts it from the same two purchase dates. While it did not,
	// this one row was the last place in the demo still showing the invented
	// 149 150,00 ₽ — the figure README.md and the transfer call above describe
	// as what a collapse to the transfer day would produce.
	tbankJournal, err := opStore.ListForEngine(ctx, p.SpaceID, tbankID)
	if err != nil {
		t.Fatalf("ListForEngine Т-Банк: %v", err)
	}
	// TSLA's leg specifically: the account also sends NVDA away on the same
	// day (see the NVDA block below), and a scan that took whichever
	// transfer_out came last would silently start checking the other one's
	// arithmetic against TSLA's expected figures.
	var outLeg *operation.Operation
	for i := range tbankJournal {
		op := &tbankJournal[i]
		if op.Type == operation.TypeTransferOut && op.InstrumentID != nil && *op.InstrumentID == tsla.InstrumentID {
			outLeg = op
		}
	}
	if outLeg == nil {
		t.Fatal("no TSLA transfer_out in the Т-Банк journal")
	}
	var outLegBaseCost int64
	for _, pc := range outLeg.TransferLots {
		pieceOn := mustAcquired(t, pc.AcquiredOn, "a transfer_out piece")
		rate, _, err := converter.Rate(ctx, "USD", "RUB", pieceOn)
		if err != nil {
			t.Fatalf("Rate(USD -> RUB, transfer_out piece %s): %v", pieceOn.Format(time.DateOnly), err)
		}
		outLegBaseCost += decimal.NewFromInt(pc.CostMinor).Mul(rate).Round(0).IntPart()
	}
	if len(outLeg.TransferLots) != 2 || outLegBaseCost != correctBaseCost {
		t.Errorf("Т-Банк transfer_out carries %d pieces worth %d ₽, want 2 worth %d — one pair of legs may not disagree about the same ten shares",
			len(outLeg.TransferLots), outLegBaseCost, correctBaseCost)
	}

	// NVDA is plan 7c's own demonstration, and the one thing in this seed that
	// only became true with it: a parcel that ARRIVED BY TRANSFER but was
	// BOUGHT EARLIER than the one already sitting in the account leaves the
	// queue first. Moving shares between one's own accounts is not a purchase
	// (НК РФ ст. 214.1 п. 13 releases "первых по времени приобретений";
	// 26 CFR 1.1012-1(c)(1)(i) names "the earliest lot the taxpayer purchased
	// or acquired"), so it cannot decide what is sold first either.
	//
	//	transferred parcel: 10 @ $100.00 bought 2026-05-14 at Т-Банк -> 100_000 minor USD
	//	parcel already there: 10 @ $150.00 bought 2026-06-20 at Freedom -> 150_000
	//	sale: 10 @ $200.00 on 2026-07-22 -> 200_000
	//
	//	by ACQUISITION (what this application now does) the 2026-05-14 parcel goes:
	//	  realized = 200_000 − 100_000 = +100_000 (+$1 000.00)
	//	  left      = the 2026-06-20 parcel, cost 150_000 ($1 500.00)
	//	    in rubles 150_000 × 65.00 (ITS own day) = 9_750_000 (97 500,00 ₽)
	//	by ARRIVAL (what it did before this plan) the parcel already in the
	//	account would have gone instead:
	//	  realized =  200_000 − 150_000 = +50_000 (+$500.00) — half as much
	//	  left      = the transferred parcel, cost 100_000 ($1 000.00)
	//	    in rubles 100_000 × 60.50 = 6_050_000 (60 500,00 ₽) — 37 000,00 ₽ less
	//
	// Every one of those four figures is named below, the wrong ones by value,
	// so a regression to arrival order fails here rather than quietly halving
	// the profit on the owner's screen.
	nvda, ok := freedomPositions["NVDA"]
	if !ok {
		t.Fatal("missing Freedom position NVDA — the seed no longer demonstrates the acquisition-ordered queue")
	}
	if nvda.Quantity.String() != "10" {
		t.Errorf("NVDA quantity = %s, want 10 (10 transferred + 10 bought there − 10 sold)", nvda.Quantity.String())
	}
	switch nvda.CostMinor {
	case 150_000:
		// The parcel bought at Freedom on 2026-06-20 is what is left.
	case 100_000:
		t.Errorf("NVDA cost_minor = 100000: the sale consumed the parcel that was ALREADY in the account and left the transferred one — that is arrival order, the exact behaviour this plan removed")
	default:
		t.Errorf("NVDA cost_minor = %d, want 150000 ($1 500.00)", nvda.CostMinor)
	}
	switch nvda.RealizedPnLMinor {
	case 100_000:
		// 200_000 − 100_000: the earliest acquisition was released.
	case 50_000:
		t.Errorf("NVDA realized P&L = 50000 (+$500.00): the sale was matched against the later, dearer parcel — arrival order again")
	default:
		t.Errorf("NVDA realized P&L = %d, want 100000 (+$1 000.00)", nvda.RealizedPnLMinor)
	}
	if len(nvda.Lots) != 1 {
		t.Fatalf("NVDA lots = %d, want exactly 1 left after the sale", len(nvda.Lots))
	}
	nvdaLotOn := mustAcquired(t, nvda.Lots[0].AcquiredOn, "the surviving NVDA lot")
	if !nvdaLotOn.Equal(day("2026-06-20")) {
		t.Errorf("surviving NVDA lot acquired on %s, want 2026-06-20 — the transferred parcel (2026-05-14) is the one that should have gone",
			nvdaLotOn.Format(time.DateOnly))
	}
	nvdaRate, _, err := converter.Rate(ctx, "USD", "RUB", nvdaLotOn)
	if err != nil {
		t.Fatalf("Rate(USD -> RUB, NVDA lot date): %v", err)
	}
	if got := decimal.NewFromInt(nvda.CostMinor).Mul(nvdaRate).Round(0).IntPart(); got != 9_750_000 {
		t.Errorf("NVDA in_base.cost_minor = %d, want 9750000 (97 500,00 ₽ = 150000 × 65.00); the arrival-order answer is 6050000 (60 500,00 ₽ = 100000 × 60.50)", got)
	}
	// The source account keeps NVDA as closed history, exactly like TSLA.
	if tbankNvda, ok := tbankPositions["NVDA"]; !ok {
		t.Error("missing Т-Банк position NVDA (closed by the transfer)")
	} else if tbankNvda.Quantity.String() != "0" || tbankNvda.CostMinor != 0 {
		t.Errorf("Т-Банк NVDA after transferring everything = {qty %s cost %d}, want {0 0}",
			tbankNvda.Quantity.String(), tbankNvda.CostMinor)
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
