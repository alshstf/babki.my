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
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/portfolio"
)

// mustAcquired asserts that a lot (or a piece of a transfer's breakdown) knows
// when it was acquired, and returns that date. Nil is a legitimate value in
// general — a transfer with no recoverable purchase dates produces lots that
// carry none (see portfolio.Lot.AcquiredOn) — and the seed contains EXACTLY ONE
// such lot on purpose: the Intel parcel that arrives at Freedom KZ with a
// hand-typed basis, which is the demo's permanent-gap row (see the INTC
// transfer). Nowhere else: every other lot comes from a real buy or from a
// transfer that carries those buys' own dates across, and that survival is
// exactly what the demo exists to show. This is therefore called only on the
// lots that must be dated — never on Intel's, which has an assertion of its own
// stating the opposite — and an unknown date at any of those call sites means
// the seed has stopped demonstrating something, so it fails rather than
// skipping the arithmetic.
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
	if len(tbankPositions) != 7 {
		t.Fatalf("Т-Банк positions = %d, want 7 (SBER, LKOH, OFZ26238, FXUS + TSLA, NVDA and INTC left closed by their transfers): %+v",
			len(tbankPositions), tbankPositions)
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
	if len(freedomPositions) != 8 {
		t.Fatalf("Freedom positions = %d, want 8 (AAPL, GOOGL, MSFT, NVDA, TSLA, KAZ32EUR, WEWKQ, INTC): %+v", len(freedomPositions), freedomPositions)
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

	// realizedInBase redoes, from the engine's own record of WHAT EACH DISPOSAL
	// WAS MADE OF, the very sum the server publishes as
	// in_base.realized_pnl_minor: the proceeds and the fee at the rate of the
	// day the disposal happened, every released parcel of basis at the rate of
	// the day THAT parcel was bought (НК РФ ст. 210 п. 5), summed as decimals
	// and rounded once for the position — exactly portfolio's realizedTerms +
	// sumInBase, rebuilt here from the seeded ingredients rather than borrowed,
	// so a seed edit that quietly flattens the story fails here instead of on
	// the owner's screen.
	//
	// A position that has never disposed of anything contributes an exact zero
	// and asks the rate table for nothing at all — which is why AAPL, whose
	// earliest lot has no rate, is no obstacle to the account total below.
	realizedInBase := func(pos *portfolio.Position, what string) int64 {
		total := decimal.Zero
		term := func(minor int64, on time.Time) {
			rate, _, err := converter.Rate(ctx, pos.Currency, "RUB", on)
			if err != nil {
				t.Fatalf("Rate(%s -> RUB, %s term on %s): %v", pos.Currency, what, on.Format(time.DateOnly), err)
			}
			total = total.Add(decimal.NewFromInt(minor).Mul(rate))
		}
		for _, e := range pos.Realizations {
			term(e.ProceedsMinor, e.OccurredOn)
			term(-e.FeeMinor, e.OccurredOn)
			for _, rel := range e.Released {
				term(-rel.CostMinor, mustAcquired(t, rel.AcquiredOn, what+" released parcel"))
			}
		}
		return total.Round(0).IntPart()
	}

	// Alphabet is plan 7b's own demonstration, and the demo's only CLOSED deal
	// whose SETTLED result has a different sign in its own currency and in
	// rubles. MSFT above makes the same point about a holding still open, where
	// the figure keeps moving; this one is over and will never move again, which
	// is the whole difference «Зафиксировано» exists to state.
	//
	//	buy  50 @ $200.00 on 2026-06-10 -> 1_000_000 minor USD, rate 81.40 -> 81_400_000
	//	sell 50 @ $210.00 on 2026-06-20 -> 1_050_000 minor USD, rate 65.00 -> 68_250_000
	//	  in USD: 1_050_000 −  1_000_000 =     +50_000 (+$500.00, a gain)
	//	  in RUB: 68_250_000 − 81_400_000 = −13_150_000 (−131 500,00 ₽, a loss)
	//
	// Converting the dollar result at ANY single rate in this table lands
	// between +30 000,00 ₽ (60.00, the lowest seeded) and +40 700,00 ₽ (81.40,
	// the highest) — a profit, for a deal that lost 131 500,00 ₽. That gap is
	// the demonstration.
	googl, ok := freedomPositions["GOOGL"]
	if !ok {
		t.Fatal("missing Freedom position GOOGL — the seed no longer demonstrates a settled result that flips sign in rubles")
	}
	if googl.Quantity.String() != "0" || googl.CostMinor != 0 || len(googl.Lots) != 0 {
		t.Errorf("GOOGL after selling the whole parcel = {qty %s cost %d lots %d}, want {0 0 []} — an open remainder would add basis to an account whose balance is already close to what it holds",
			googl.Quantity.String(), googl.CostMinor, len(googl.Lots))
	}
	if len(googl.Realizations) != 1 {
		t.Fatalf("GOOGL realizations = %d, want exactly 1 (the single sale)", len(googl.Realizations))
	}
	if googl.RealizedPnLMinor != 50_000 {
		t.Errorf("GOOGL realized P&L = %d, want 50000 (+$500.00 = 1050000 − 1000000)", googl.RealizedPnLMinor)
	}
	googlBase := realizedInBase(googl, "GOOGL")
	if googlBase != -13_150_000 {
		t.Errorf("GOOGL realized P&L in RUB = %d, want -13150000 (68 250 000 − 81 400 000 = −131 500,00 ₽)", googlBase)
	}
	if googl.RealizedPnLMinor <= 0 || googlBase >= 0 {
		t.Errorf("GOOGL settled result = %d in USD and %d in RUB: the demo must contain one CLOSED deal that is a profit in the position's currency and a loss in rubles — without it plan 7b's consequence cannot be seen on demo data at all",
			googl.RealizedPnLMinor, googlBase)
	}
	// The answer a single-rate conversion of the dollar result would give,
	// named by value so a seed edit that makes the two agree is unmistakable.
	rateOnSale, _, err := converter.Rate(ctx, "USD", "RUB", day("2026-06-20"))
	if err != nil {
		t.Fatalf("Rate(USD -> RUB, 2026-06-20): %v", err)
	}
	if flat := decimal.NewFromInt(googl.RealizedPnLMinor).Mul(rateOnSale).Round(0).IntPart(); flat != 3_250_000 || flat <= 0 {
		t.Errorf("GOOGL result converted at the sale day's rate alone = %d, want 3250000 (+32 500,00 ₽, a PROFIT) — the point of this deal is that no single rate reproduces −131 500,00 ₽", flat)
	}

	// NVDA's own settled result, the other half of the account's line: a
	// disposal whose ruble figure differs from any single-rate conversion
	// WITHOUT flipping sign, so the demo shows both shapes.
	//
	//	proceeds 200_000 on 2026-07-22 -> the rate table publishes nothing that
	//	  day, so the ordinary nearest-earlier rule gives 2026-07-20's 78.50
	//	  -> 15_700_000
	//	basis    100_000 bought 2026-05-14, rate 60.50 -> 6_050_000
	//	  in RUB: 15_700_000 − 6_050_000 = +9_650_000 (+96 500,00 ₽)
	nvdaBase := realizedInBase(nvda, "NVDA")
	if nvdaBase != 9_650_000 {
		t.Errorf("NVDA realized P&L in RUB = %d, want 9650000 (15 700 000 − 6 050 000 = +96 500,00 ₽)", nvdaBase)
	}

	// And the account's «Зафиксировано» line itself — the sum of the rounded
	// per-position figures, exactly as portfolio.realizedTotals adds them.
	// It disagrees with itself in SIGN across the display-currency toggle, one
	// click apart, on real demo data:
	//
	//	NVDA     +100_000 USD    +9_650_000 ₽
	//	GOOGL     +50_000 USD   −13_150_000 ₽
	//	AAPL, MSFT, TSLA, KAZ32EUR, WEWKQ, INTC: no disposals, exactly zero in
	//	  both — and, crucially, no rate asked for either, which is why AAPL's
	//	  rate-less lot and INTC's dateless one cannot stop this total
	//	total    +150_000 USD    −3_500_000 ₽
	//	        (+$1 500.00)     (−35 000,00 ₽)
	var accountUSD, accountRUB int64
	for ticker, pos := range freedomPositions {
		accountUSD += pos.RealizedPnLMinor
		accountRUB += realizedInBase(pos, ticker)
	}
	if accountUSD != 150_000 || accountRUB != -3_500_000 {
		t.Errorf("Freedom KZ realized total = %d USD / %d RUB, want 150000 (+$1 500.00) / -3500000 (−35 000,00 ₽)", accountUSD, accountRUB)
	}
	if accountUSD <= 0 || accountRUB >= 0 {
		t.Errorf("Freedom KZ realized total = %d USD / %d RUB: the account's own «Зафиксировано» line must come out with opposite signs in the two display modes — that is what makes the plan's consequence visible without opening a single position",
			accountUSD, accountRUB)
	}

	// THE DEMO'S DATELESS PARCEL, and the reason this seed can put two
	// DIFFERENT "no ruble figures here" sentences on one screen (#66):
	//
	//	Apple — one lot's purchase date has no fx rate. The backfill job fills
	//	        that date from cbr.ru and the row converts on its own. Pinned
	//	        above, by lotsWithoutRate.
	//	Intel — the parcel has no purchase date at all, because its basis was
	//	        typed in by hand and nothing was released behind it. No job can
	//	        close that.
	//
	// This is the ONE lot in the whole seed that legitimately has no date (see
	// mustAcquired), and both halves are asserted: that Intel's has none, and
	// that Apple's all have one. Without the second, a seed edit that made
	// Apple dateless too would leave the screen showing one sentence twice with
	// every test still green — and the pair is the entire point.
	intc, ok := freedomPositions["INTC"]
	if !ok {
		t.Fatal("missing Freedom position INTC — the seed no longer shows a parcel whose purchase date was never recorded")
	}
	if len(intc.Lots) != 1 {
		t.Fatalf("INTC lots = %d, want 1 (the whole parcel arrives as a single hand-priced lot)", len(intc.Lots))
	}
	if intc.Lots[0].AcquiredOn != nil {
		t.Errorf("INTC lot acquired on %s, want NO date at all: a basis given by hand has no purchase behind it, and dating it on the day the shares changed brokers is exactly the invention portfolio.Lot.AcquiredOn refuses",
			intc.Lots[0].AcquiredOn.Format(time.DateOnly))
	}
	if intc.CostMinor != 300_000 {
		t.Errorf("INTC cost_minor = %d, want 300000 ($3 000,00 — the owner's own figure, which the journal may not contradict)", intc.CostMinor)
	}
	for _, l := range aapl.Lots {
		if l.AcquiredOn == nil {
			t.Fatal("an AAPL lot has no acquisition date: the demo's two unconvertible rows would then share ONE cause, and the pair of different sentences this seed exists to show collapses into a single one")
		}
	}
	// The source account keeps Intel as closed history, exactly like TSLA and NVDA.
	if tbankIntc, ok := tbankPositions["INTC"]; !ok {
		t.Error("missing Т-Банк position INTC (closed by the transfer)")
	} else if tbankIntc.Quantity.String() != "0" || tbankIntc.CostMinor != 0 {
		t.Errorf("Т-Банк INTC after transferring everything = {qty %s cost %d}, want {0 0}",
			tbankIntc.Quantity.String(), tbankIntc.CostMinor)
	}

	// THE DEMO'S THIRD-CURRENCY VALUATION (#39). A bond with a euro face value,
	// held in a dollar position, inside a ruble space: three distinct
	// currencies on one row, which is the only shape in which valuing it twice
	// over can go wrong. Redone here from the seeded ingredients — face value,
	// face currency, quote, and the two fx rows the bridge is built from —
	// exactly as portfolio.marketValue and Handler.positionInBase strike it, so
	// a seed edit that quietly brings any two of the three currencies back into
	// agreement fails here instead of leaving the path untested on screen.
	//
	// The face-vs-position disagreement asserted below now carries a second
	// demonstration as well: it is the one case the trade dialog refuses to
	// convert a percentage of face into money, having no fx rate to do it with,
	// so it names the mismatch instead (#77 — faceGapOf in
	// web/src/routes/accounts/trade-dialog.tsx). The OFZ further down is the
	// same dialog with the two currencies agreeing and the link intact.
	//
	//	valuation  100_000 (€1 000,00 face) × 98.00 % × 5 =    490_000 minor EUR
	//	  in $     490_000 × (92.30 ÷ 78.50)              =    576_140 ($5 761,40)
	//	  in ₽     490_000 × 92.30, from the EUROS        = 45_227_000 (452 270,00 ₽)
	//	  in ₽     576_140 × 78.50, chained via the dollar = 45_226_990 — ten
	//	           kopecks short, the fraction the intermediate cent threw away
	//	cost       575_000 minor USD × 65.00 (its lot's own day) = 37_375_000
	bond, ok := freedomPositions["KAZ32EUR"]
	if !ok {
		t.Fatal("missing Freedom position KAZ32EUR — the seed no longer holds anything valued in a third currency")
	}
	bondInst, err := instStore.ByID(ctx, bond.InstrumentID)
	if err != nil {
		t.Fatalf("instrument ByID KAZ32EUR: %v", err)
	}
	if bondInst.FaceValueMinor == nil || bondInst.FaceCurrency == nil {
		t.Fatalf("KAZ32EUR face value/currency = %v/%v, want both set — without them marketValue publishes no valuation at all",
			bondInst.FaceValueMinor, bondInst.FaceCurrency)
	}
	if *bondInst.FaceCurrency == bond.Currency || bond.Currency == "RUB" || *bondInst.FaceCurrency == "RUB" {
		t.Fatalf("KAZ32EUR face currency %s, position currency %s, base RUB: the three must all differ, or this row stops exercising the case it exists for",
			*bondInst.FaceCurrency, bond.Currency)
	}
	bondQuote, err := marketdata.NewStore(pool).QuoteOn(ctx, bond.InstrumentID, on)
	if err != nil {
		t.Fatalf("QuoteOn KAZ32EUR: %v", err)
	}
	// Same expression portfolio.marketValue uses for a bond: face × percent ×
	// quantity, rounded once, denominated in the FACE currency.
	rawEUR := decimal.NewFromInt(*bondInst.FaceValueMinor).Mul(bondQuote.Price).Shift(-2).Mul(bond.Quantity).Round(0).IntPart()
	if rawEUR != 490_000 {
		t.Errorf("KAZ32EUR raw valuation = %d minor %s, want 490000 (€4 900,00)", rawEUR, *bondInst.FaceCurrency)
	}
	// No EUR/USD pair is seeded: the converter bridges it through the ruble out
	// of the two rows that are (see marketdata.resolveRate). Asking for it here
	// rather than computing it is what makes this the same rate the handler got.
	eurToPosition, _, err := converter.Rate(ctx, *bondInst.FaceCurrency, bond.Currency, on)
	if err != nil {
		t.Fatalf("Rate(%s -> %s, today): %v — the bridge the eurobond's row depends on is gone", *bondInst.FaceCurrency, bond.Currency, err)
	}
	eurToBase, _, err := converter.Rate(ctx, *bondInst.FaceCurrency, "RUB", on)
	if err != nil {
		t.Fatalf("Rate(%s -> RUB, today): %v", *bondInst.FaceCurrency, err)
	}
	valuationUSD := decimal.NewFromInt(rawEUR).Mul(eurToPosition).Round(0).IntPart()
	valuationRUB := decimal.NewFromInt(rawEUR).Mul(eurToBase).Round(0).IntPart()
	chainedRUB := decimal.NewFromInt(valuationUSD).Mul(rateToday).Round(0).IntPart()
	if bond.CostMinor != 575_000 || valuationUSD != 576_140 {
		t.Errorf("KAZ32EUR cost/value in USD = %d/%d, want 575000/576140", bond.CostMinor, valuationUSD)
	}
	if valuationRUB != 45_227_000 {
		t.Errorf("KAZ32EUR valuation in RUB = %d, want 45227000 (452 270,00 ₽ = 490000 × 92.30, struck from the euros)", valuationRUB)
	}
	if chainedRUB != 45_226_990 || chainedRUB == valuationRUB {
		t.Errorf("KAZ32EUR valuation chained through the dollar = %d, want 45226990 and DIFFERENT from %d: if the two agree, this row no longer shows what converting once buys",
			chainedRUB, valuationRUB)
	}
	if len(bond.Lots) != 1 {
		t.Fatalf("KAZ32EUR lots = %d, want 1", len(bond.Lots))
	}
	bondLotOn := mustAcquired(t, bond.Lots[0].AcquiredOn, "the KAZ32EUR lot")
	bondLotRate, _, err := converter.Rate(ctx, bond.Currency, "RUB", bondLotOn)
	if err != nil {
		t.Fatalf("Rate(USD -> RUB, KAZ32EUR lot date): %v", err)
	}
	if got := decimal.NewFromInt(bond.CostMinor).Mul(bondLotRate).Round(0).IntPart(); got != 37_375_000 {
		t.Errorf("KAZ32EUR cost in RUB = %d, want 37375000 (373 750,00 ₽ = 575000 × 65.00, that lot's own day)", got)
	}

	// THE DEMO'S SUB-CENT QUOTE (#30). Two fraction digits print it as «0,00» —
	// neither the price nor zero — so the screen renders it by significant
	// digits instead. Pinned by the PROPERTY that makes it the demo's example,
	// not just by its value: below a hundredth, above nothing, and on a share,
	// because marketValue has no valuation model for crypto/currency/metal/
	// custom and a row of those types carries no price line to render at all.
	wework, ok := freedomPositions["WEWKQ"]
	if !ok {
		t.Fatal("missing Freedom position WEWKQ — the seed no longer holds anything quoted below a hundredth")
	}
	if weworkInst, err := instStore.ByID(ctx, wework.InstrumentID); err != nil {
		t.Fatalf("instrument ByID WEWKQ: %v", err)
	} else if weworkInst.Type != instrument.TypeShare && weworkInst.Type != instrument.TypeETF {
		t.Errorf("WEWKQ type = %s, want share or etf: any other type has no valuation and therefore no price line, and the sub-cent rendering would be invisible", weworkInst.Type)
	}
	weworkQuote, err := marketdata.NewStore(pool).QuoteOn(ctx, wework.InstrumentID, on)
	if err != nil {
		t.Fatalf("QuoteOn WEWKQ: %v", err)
	}
	hundredth := decimal.RequireFromString("0.01")
	if !weworkQuote.Price.IsPositive() || !weworkQuote.Price.LessThan(hundredth) {
		t.Errorf("WEWKQ quote = %s, want a positive price below %s — at or above it the ordinary two-digit rendering is honest and the demo shows nothing",
			weworkQuote.Price, hundredth)
	}
	if !weworkQuote.Price.Round(2).IsZero() {
		t.Errorf("WEWKQ quote = %s rounds to %s at two digits: the point of this row is that the old rendering printed a ZERO for a price that is not zero",
			weworkQuote.Price, weworkQuote.Price.Round(2))
	}
	if got := weworkQuote.Price.Mul(wework.Quantity).Shift(2).Round(0).IntPart(); got != 1_250 {
		t.Errorf("WEWKQ valuation = %d, want 1250 ($12,50 = 5000 × 0.0025) — a real, nonzero holding priced at a fraction of a cent", got)
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

	// THE DEMO'S BOND PRICE (#77). A broker quotes a bond as a percentage of
	// its face value; this application records money per bond; and the trade
	// dialog now shows both fields with each deriving the other. What is pinned
	// here is that the seed's OFZ row is the row that dialog produces out of a
	// round 95,00 %, so the stand shows the feature agreeing with the data
	// rather than merely sitting beside it:
	//
	//	1 000,00 ₽ face × 95 % = 950,00 ₽ a bond × 100 = 95 000,00 ₽ recorded
	//
	// The expected price is recomputed from the seeded FACE VALUE rather than
	// compared against a literal, so moving the face value without moving the
	// price — the exact edit that would turn the comment on that operation into
	// a false claim — fails here.
	//
	// The face value's currency being the instrument's own is asserted too, and
	// it is not decoration: that equality is the entire condition under which
	// the dialog is willing to convert at all (faceGapOf in
	// web/src/routes/accounts/trade-dialog.tsx). The eurobond above is the
	// demo's other side of the same coin, where the two currencies differ and
	// the dialog names the mismatch instead of producing a number.
	ofz, ok := tbankPositions["OFZ26238"]
	if !ok {
		t.Fatal("missing Т-Банк position OFZ26238 — the seed no longer holds the bond whose price the trade dialog is demonstrated on")
	}
	ofzInst, err := instStore.ByID(ctx, ofz.InstrumentID)
	if err != nil {
		t.Fatalf("instrument ByID OFZ26238: %v", err)
	}
	if ofzInst.FaceValueMinor == nil || ofzInst.FaceCurrency == nil || *ofzInst.FaceCurrency != ofzInst.Currency {
		t.Fatalf("OFZ26238 face = %v %v against instrument currency %s, want a face value denominated in the instrument's own currency: without that equality the trade dialog refuses the conversion and this row demonstrates nothing",
			ofzInst.FaceValueMinor, ofzInst.FaceCurrency, ofzInst.Currency)
	}
	// The same two decimal shifts bondPriceFromPercent performs (see
	// web/src/lib/money.ts): minor units into major ones, and percent into a
	// fraction. Two divisions by a hundred, deliberately not folded into one.
	wantOFZPrice := decimal.NewFromInt(*ofzInst.FaceValueMinor).Shift(-2).
		Mul(decimal.RequireFromString("95")).Shift(-2)
	tbankOps, err := operation.NewStore(pool).ListForEngine(ctx, p.SpaceID, tbankID)
	if err != nil {
		t.Fatalf("ListForEngine Т-Банк: %v", err)
	}
	ofzBuys := 0
	for _, op := range tbankOps {
		if op.Type != operation.TypeBuy || op.InstrumentID == nil || *op.InstrumentID != ofz.InstrumentID {
			continue
		}
		ofzBuys++
		if op.Price == nil || op.Quantity == nil {
			t.Errorf("OFZ26238 buy has price %v and quantity %v, want both recorded", op.Price, op.Quantity)
			continue
		}
		if !op.Price.Equal(wantOFZPrice) {
			t.Errorf("OFZ26238 buy price = %s, want %s (95 %% of the seeded face value, in %s): the operation and the comment above it have to say the same thing",
				op.Price.String(), wantOFZPrice.String(), ofzInst.Currency)
			continue
		}
		// Quantity × price, which is exactly what the dialog's «Итого» shows
		// and what it sends as amount_minor.
		if want := op.Price.Mul(*op.Quantity).Shift(2).Round(0).IntPart(); -op.AmountMinor != want {
			t.Errorf("OFZ26238 buy amount = %d, want %d (quantity × price)", -op.AmountMinor, want)
		}
	}
	if ofzBuys != 1 {
		t.Errorf("OFZ26238 buys = %d, want exactly 1 — the arithmetic above is stated for a single purchase", ofzBuys)
	}

	// THE DEMO'S QUOTE DATES (#90). A quote's date is the trading SESSION its
	// price belongs to — not the day anything was fetched, and not the day the
	// seed ran — and the positions screen prints it as «Цена на …». Two
	// properties keep that legible on the stand, and neither follows from the
	// other:
	//
	//   - the quotes do NOT all share one date. One date across every row is
	//     indistinguishable from a stamp the screen puts on the whole page,
	//     which is precisely the reading this field stopped deserving;
	//   - every date is a weekday, and none is later than the demo's own today.
	//     A Saturday is not a session any exchange held, and a date in the
	//     future is one the quotes worker refuses to store outright (see
	//     jobs.go) — a seed writing either would be showing a state the running
	//     system cannot reach.
	//
	// Read through LatestQuotes, the same call GET .../positions makes, so what
	// is checked is what a row would actually be captioned with.
	instrumentIDs := make([]uuid.UUID, 0, len(tbankPositions)+len(freedomPositions))
	for _, pos := range tbankPositions {
		instrumentIDs = append(instrumentIDs, pos.InstrumentID)
	}
	for _, pos := range freedomPositions {
		instrumentIDs = append(instrumentIDs, pos.InstrumentID)
	}
	latestQuotes, err := marketdata.NewStore(pool).LatestQuotes(ctx, instrumentIDs)
	if err != nil {
		t.Fatalf("LatestQuotes: %v", err)
	}
	sessions := make(map[string]bool, len(latestQuotes))
	for id, q := range latestQuotes {
		sessions[q.On.Format(time.DateOnly)] = true
		if wd := q.On.Weekday(); wd == time.Saturday || wd == time.Sunday {
			t.Errorf("quote for instrument %s is dated %s, a %s: a seeded quote date has to be a day an exchange could have held a session",
				id, q.On.Format(time.DateOnly), wd)
		}
		if q.On.After(on) {
			t.Errorf("quote for instrument %s is dated %s, later than the demo's own today (%s)",
				id, q.On.Format(time.DateOnly), on.Format(time.DateOnly))
		}
	}
	if len(sessions) < 2 {
		t.Errorf("seeded quotes span %d distinct session dates (%v), want at least 2 — with one date on every row the «Цена на …» caption cannot be told apart from a page-wide stamp",
			len(sessions), sessions)
	}

	// second run refuses (instance not empty)
	if err := seedDemo(ctx, pool); err == nil {
		t.Fatal("second seedDemo: want error")
	}
}
