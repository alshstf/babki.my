package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/importer/tinvest"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/portfolio"
	"babki.my/babki/internal/portfolio/portfoliotest"
)

// mustAcquired asserts a lot knows its purchase date and returns it. The seed
// has exactly one dateless lot, the Intel parcel (see the INTC transfer), which
// has its own opposite assertion; anywhere else a missing date means a
// demonstration broke.
func mustAcquired(t *testing.T, on *time.Time, what string) time.Time {
	t.Helper()
	if on == nil {
		t.Fatalf("%s has an unknown acquisition date, want a real one: this seed's dates all come from actual purchases", what)
	}
	return *on
}

// demoToday is the demo's «today»: the date of the newest seeded USD/RUB rate.
var demoToday = time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)

// demo is a freshly seeded instance and what its checks look things up by.
type demo struct {
	ctx                context.Context
	pool               *pgxpool.Pool
	space              uuid.UUID
	accounts           []account.WithBalance
	totals             []account.CurrencyTotal
	tbankID, freedomID uuid.UUID
	ops                *operation.Store
	catalog            *instrument.Store
	conv               *marketdata.Converter
	// tbank and freedom are the two brokers' positions, by ticker.
	tbank, freedom map[string]*portfolio.Position
}

// seeded seeds a database of its own and signs the demo user in.
func seeded(t *testing.T) demo {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()
	if err := seedDemo(ctx, pool); err != nil {
		t.Fatalf("seedDemo: %v", err)
	}
	_, p, err := family.NewService(family.NewStore(pool)).Login(ctx, "demo", "demo1234")
	if err != nil || p.Role != family.RoleOwner {
		t.Fatalf("login demo: %v %+v", err, p)
	}
	d := demo{
		ctx: ctx, pool: pool, space: p.SpaceID,
		ops: operation.NewStore(pool), catalog: instrument.NewStore(pool),
		conv: marketdata.NewConverter(marketdata.NewStore(pool)),
	}
	if d.accounts, err = account.NewStore(pool).ListWithBalance(ctx, p.SpaceID); err != nil {
		t.Fatalf("accounts: %v", err)
	}
	if d.totals, err = account.NewStore(pool).SummaryByCurrency(ctx, p.SpaceID, nil); err != nil {
		t.Fatalf("totals: %v", err)
	}
	for _, a := range d.accounts {
		switch a.Name {
		case "Брокерский Т-Банк":
			d.tbankID = a.ID
		case "Freedom KZ":
			d.freedomID = a.ID
		}
	}
	if d.tbankID == uuid.Nil || d.freedomID == uuid.Nil {
		t.Fatalf("brokerage accounts not found among seeded accounts")
	}
	d.tbank, d.freedom = d.positionsByTicker(t, d.tbankID), d.positionsByTicker(t, d.freedomID)
	return d
}

// positionsByTicker folds an account's journal, as the positions are a
// projection of it, checked through the stores.
func (d demo) positionsByTicker(t *testing.T, accountID uuid.UUID) map[string]*portfolio.Position {
	t.Helper()
	ops, err := d.ops.ListForEngine(d.ctx, d.space, accountID)
	if err != nil {
		t.Fatalf("ListForEngine: %v", err)
	}
	positions, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	out := make(map[string]*portfolio.Position, len(positions))
	for _, pos := range positions {
		inst, err := d.catalog.ByID(d.ctx, pos.InstrumentID)
		if err != nil {
			t.Fatalf("instrument ByID: %v", err)
		}
		out[inst.Ticker] = pos
	}
	return out
}

// position is one of the account's positions, failing the test without it.
func position(t *testing.T, positions map[string]*portfolio.Position, ticker, why string) *portfolio.Position {
	t.Helper()
	pos, ok := positions[ticker]
	if !ok {
		t.Fatalf("missing position %s — %s", ticker, why)
	}
	return pos
}

// demoDay parses a YYYY-MM-DD date.
func demoDay(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.DateOnly, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return parsed
}

// rateToday is the USD/RUB rate «today» resolves to: any date past the newest
// seeded rate gives the same 78.50 the running instance uses, without depending
// on the clock.
func (d demo) rateToday(t *testing.T) decimal.Decimal {
	t.Helper()
	rate, on, err := d.conv.Rate(d.ctx, "USD", "RUB", demoDay(t, "2099-01-01"))
	if err != nil {
		t.Fatalf("Rate(USD -> RUB, today): %v", err)
	}
	if !on.Equal(demoToday) {
		t.Errorf("newest USD/RUB rate is dated %s, want %s — the figures below are struck against the last rate in the table",
			on.Format(time.DateOnly), demoToday.Format(time.DateOnly))
	}
	return rate
}

// realizedInBase rebuilds in_base.realized_pnl_minor from each disposal's
// parcels: proceeds and fee at the disposal day's rate, each parcel at its own
// purchase day's (НК РФ ст. 210 п. 5), summed as decimals and rounded once, as
// portfolio's realizedTerms and sumInBase do. A position with no disposals is
// zero and asks for no rate, so AAPL's gap does not block the total.
func (d demo) realizedInBase(t *testing.T, pos *portfolio.Position, what string) int64 {
	t.Helper()
	total := decimal.Zero
	term := func(minor int64, on time.Time) {
		rate, _, err := d.conv.Rate(d.ctx, pos.Currency, "RUB", on)
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

// The demo user signs in and owns six accounts, each with a balance, in
// roubles and dollars; a second seeding of a non-empty instance is refused.
func TestSeedDemoSignsInToSixAccountsInTwoCurrencies(t *testing.T) {
	t.Parallel()
	d := seeded(t)
	if len(d.accounts) != 6 {
		t.Fatalf("accounts = %d; want 6", len(d.accounts))
	}
	for _, a := range d.accounts {
		if a.Balance == nil {
			t.Errorf("account %q has no balance", a.Name)
		}
	}
	if len(d.totals) != 2 {
		t.Fatalf("totals = %+v; want RUB+USD", d.totals)
	}
	if err := seedDemo(d.ctx, d.pool); err == nil {
		t.Fatal("second seedDemo: want error")
	}
}

func TestSeedDemoBondIsAskedOfTheExchangeByItsOwnID(t *testing.T) {
	t.Parallel()
	d := seeded(t)
	ctx := d.ctx
	instStore := d.catalog

	// The quotes job sends ListTradable's tickers to MOEX verbatim, which answers
	// by SECID, so a non-SECID ticker is never priced (#35). The literal is the
	// exchange's id for ОФЗ 26238 (iss.moex.com, 2026-08-08: bonds/TQOB, ISIN
	// RU000A1038V6); no test may call the exchange, so only this catches it.
	tradable, err := instStore.ListTradable(ctx)
	if err != nil {
		t.Fatalf("ListTradable: %v", err)
	}
	askable := make([]string, 0, len(tradable))
	for _, inst := range tradable {
		askable = append(askable, inst.Ticker)
	}
	if !slices.Contains(askable, "SU26238RMFS4") {
		t.Errorf("the tickers the quotes job would ask for are %v, and MOEX's own id for the demo's bond, %q, is not among them",
			askable, "SU26238RMFS4")
	}
}

func TestSeedDemoBrokersHoldWhatTheirJournalsSay(t *testing.T) {
	t.Parallel()
	d := seeded(t)
	tbankPositions := d.tbank
	if len(tbankPositions) != 8 {
		t.Fatalf("Т-Банк positions = %d, want 8 (SBER, LKOH, SU26238RMFS4, FXUS + TSLA, NVDA, INTC and AMZN left closed by their transfers): %+v",
			len(tbankPositions), tbankPositions)
	}
	wantQty := map[string]string{"SBER": "300", "SU26238RMFS4": "100", "FXUS": "30", "LKOH": "15"}
	for ticker, qty := range wantQty {
		pos, ok := tbankPositions[ticker]
		if !ok {
			t.Fatalf("missing Т-Банк position %s", ticker)
		}
		if pos.Quantity.String() != qty {
			t.Errorf("Т-Банк %s quantity = %s, want %s", ticker, pos.Quantity.String(), qty)
		}
	}
	if lkoh := tbankPositions["LKOH"]; portfoliotest.Realized(t, lkoh) <= 0 {
		t.Errorf("LKOH realized P&L = %d, want > 0", portfoliotest.Realized(t, lkoh))
	}
	// TSLA left Т-Банк whole: the source keeps it as closed history (zero
	// quantity, cost and lots), as
	// TestPositionInBaseTransferredLotsKeepTheirPurchaseDates checks.
	tsla, ok := tbankPositions["TSLA"]
	if !ok {
		t.Fatal("missing Т-Банк position TSLA (closed by the transfer)")
	}
	if tsla.Quantity.String() != "0" || tsla.CostMinor != 0 || len(tsla.Lots) != 0 {
		t.Errorf("Т-Банк TSLA after transferring everything = {qty %s cost %d lots %d}, want {0 0 []}",
			tsla.Quantity.String(), tsla.CostMinor, len(tsla.Lots))
	}

	freedomPositions := d.freedom
	if len(freedomPositions) != 9 {
		t.Fatalf("Freedom positions = %d, want 9 (AAPL, GOOGL, MSFT, NVDA, TSLA, KAZ32EUR, WEWKQ, INTC, AMZN): %+v", len(freedomPositions), freedomPositions)
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

	// TSLA arrived by transfer and keeps two lots with their purchase days;
	// the rouble arithmetic is checked further down, after rateToday.
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
}

func TestSeedDemoDollarRatesHaveTheirShape(t *testing.T) {
	t.Parallel()
	d := seeded(t)
	aapl := position(t, d.freedom, "AAPL", "Freedom holds Apple")
	ctx := d.ctx
	pool := d.pool
	day := func(s string) time.Time { return demoDay(t, s) }

	// 100 USD = 10000 minor -> 785000 minor = 7850.00 RUB at 78.50.
	converter := marketdata.NewConverter(marketdata.NewStore(pool))
	on := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	got, err := converter.Convert(ctx, 100_00, "USD", "RUB", on)
	if err != nil {
		t.Fatalf("Convert(100 USD -> RUB): %v", err)
	}
	if got != 785_000 {
		t.Errorf("Convert(100 USD -> RUB) = %d, want 785000 (7850.00 RUB)", got)
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
	// (d) the same gap takes one of AAPL's lots, so the position has no
	// rouble figures.
	lotsWithoutRate := 0
	for _, l := range aapl.Lots {
		if _, _, err := converter.Rate(ctx, "USD", "RUB", mustAcquired(t, l.AcquiredOn, "an AAPL lot")); errors.Is(err, marketdata.ErrNoRate) {
			lotsWithoutRate++
		}
	}
	if lotsWithoutRate != 1 {
		t.Errorf("AAPL lots with no fx rate on their acquisition date = %d, want exactly 1 — seeding a rate for the early buy would remove the demo's only position that honestly refuses to convert", lotsWithoutRate)
	}
}

func TestSeedDemoAmazonShowsAMissingAndAHolidayRate(t *testing.T) {
	t.Parallel()
	d := seeded(t)
	ctx := d.ctx
	converter := d.conv
	freedomPositions := d.freedom
	day := func(s string) time.Time { return demoDay(t, s) }

	// AMZN's two lot dates carry the journal's last two sentences:
	//
	// 	2026-05-11 — before the history, no rate at all: its transfer is the only
	// 	             row naming a missing purchase-day rate (no_rate_lot_date),
	// 	             while its own day has a rate that may not be used (#79).
	// 	2026-06-12 — Russia Day: the nearest rate is 2026-06-11's 81.00, the only
	// 	             transfer where dated_on and rate_on differ (#80).
	//
	// Both are named by value, so moving either fails.
	amzn, ok := freedomPositions["AMZN"]
	if !ok {
		t.Fatal("missing Freedom position AMZN — the seed no longer shows a missing rate for a purchase date")
	}
	if amzn.Quantity.String() != "20" || amzn.CostMinor != 380_000 {
		t.Errorf("AMZN = {qty %s cost %d}, want {20 380000} ($1 800,00 + $2 000,00, two parcels moved one at a time)",
			amzn.Quantity.String(), amzn.CostMinor)
	}
	if len(amzn.Lots) != 2 {
		t.Fatalf("AMZN lots = %d, want 2 — the two transfers must arrive as two lots with two purchase dates", len(amzn.Lots))
	}
	amznLotOn := map[string]int64{}
	for _, l := range amzn.Lots {
		amznLotOn[mustAcquired(t, l.AcquiredOn, "an AMZN lot").Format(time.DateOnly)] = l.CostMinor
	}
	if cost, ok := amznLotOn["2026-05-11"]; !ok || cost != 180_000 {
		t.Errorf("AMZN lots = %v, want one acquired 2026-05-11 costing 180000", amznLotOn)
	}
	if cost, ok := amznLotOn["2026-06-12"]; !ok || cost != 200_000 {
		t.Errorf("AMZN lots = %v, want one acquired 2026-06-12 costing 200000", amznLotOn)
	}
	if _, _, err := converter.Rate(ctx, "USD", "RUB", day("2026-05-11")); !errors.Is(err, marketdata.ErrNoRate) {
		t.Errorf("Rate(USD -> RUB, 2026-05-11) error = %v, want ErrNoRate — the AMZN parcel bought that day is what makes a transfer say the missing rate is a PURCHASE day's", err)
	}
	rateOnHoliday, dateOnHoliday, err := converter.Rate(ctx, "USD", "RUB", day("2026-06-12"))
	if err != nil {
		t.Fatalf("Rate(USD -> RUB, 2026-06-12): %v", err)
	}
	if want := decimal.RequireFromString("81.00"); !rateOnHoliday.Equal(want) {
		t.Errorf("USD/RUB on 2026-06-12 = %s, want %s (2026-06-11's, the last working day before Russia Day)", rateOnHoliday, want)
	}
	if !dateOnHoliday.Equal(day("2026-06-11")) {
		t.Errorf("USD/RUB rate date for 2026-06-12 = %s, want 2026-06-11 — equal dates would collapse the demo's only transfer where dated_on and rate_on differ",
			dateOnHoliday.Format(time.DateOnly))
	}
	if got := decimal.NewFromInt(200_000).Mul(rateOnHoliday).Round(0).IntPart(); got != 16_200_000 {
		t.Errorf("AMZN's datable parcel in rubles = %d, want 16200000 (162 000,00 ₽ = 200000 × 81.00) — the figure the transfer row and the buy row four lines above it must agree on", got)
	}
}

func TestSeedDemoJournalRunsPastOnePage(t *testing.T) {
	t.Parallel()
	d := seeded(t)
	ctx := d.ctx
	opStore := d.ops
	tbankID := d.tbankID
	day := func(s string) time.Time { return demoDay(t, s) }

	// Т-Банк's journal must run past one 50-row page (JOURNAL_PAGE_SIZE, also
	// defaultListLimit), so "show more" appears on the stand (#86); the server's
	// has_more is what is asserted.
	firstPage, hasMore, err := opStore.ListByAccount(ctx, d.space, tbankID, 50, 0, operation.JournalFilter{})
	if err != nil {
		t.Fatalf("ListByAccount(Т-Банк, 50, 0): %v", err)
	}
	if len(firstPage) != 50 || !hasMore {
		t.Errorf("Т-Банк journal page one = %d rows, has_more = %v; want a full 50 and true — the demo must be able to show the «Показать еще» button",
			len(firstPage), hasMore)
	}
	rest, restHasMore, err := opStore.ListByAccount(ctx, d.space, tbankID, 50, 50, operation.JournalFilter{})
	if err != nil {
		t.Fatalf("ListByAccount(Т-Банк, 50, 50): %v", err)
	}
	if len(rest) == 0 || restHasMore {
		t.Errorf("Т-Банк journal page two = %d rows, has_more = %v; want a non-empty last page and false — the button must also be able to go away",
			len(rest), restHasMore)
	}
	// The padding rows stay inert: base currency and no instrument, so
	// nothing converts or moves.
	for _, o := range append(firstPage, rest...) {
		if !o.OccurredOn.Before(day("2026-05-05")) {
			continue
		}
		if o.Currency != "RUB" || o.InstrumentID != nil {
			t.Errorf("the row dated %s that lengthens the journal is %s/instrument=%v, want RUB and none — added length must convert nothing and value nothing",
				o.OccurredOn.Format(time.DateOnly), o.Currency, o.InstrumentID)
		}
	}
}

func TestSeedDemoMicrosoftGainsInDollarsAndLosesInRoubles(t *testing.T) {
	t.Parallel()
	d := seeded(t)
	ctx := d.ctx
	pool := d.pool
	converter := d.conv
	freedomPositions := d.freedom
	on := demoToday
	rateToday := d.rateToday(t)

	// One open position whose unrealized profit has opposite signs in dollars
	// and roubles (owner's decision 2026-07-29). Redone from the seeded ingredients so
	// a seed edit that flattens it fails:
	//
	// 	cost    1_000_000 minor USD × 81.40 (the lot's own day) = 81_400_000 ₽
	// 	value   1_020_000 minor USD × 78.50 (today)             = 80_070_000 ₽
	// 	USD profit = 1_020_000 − 1_000_000 =    +20_000 (a gain)
	// 	RUB profit = 80_070_000 − 81_400_000 = −1_330_000 (a loss)
	msft, ok := freedomPositions["MSFT"]
	if !ok {
		t.Fatal("missing Freedom position MSFT")
	}
	if len(msft.Lots) != 1 {
		t.Fatalf("MSFT lots = %d, want 1 — the sign flip is stated for a single-lot position", len(msft.Lots))
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
}

func TestSeedDemoTeslaKeepsItsPurchaseDayRatesAcrossTheTransfer(t *testing.T) {
	t.Parallel()
	d := seeded(t)
	tsla := position(t, d.freedom, "TSLA", "Freedom holds Tesla by transfer")
	ctx := d.ctx
	opStore := d.ops
	converter := d.conv
	tbankID := d.tbankID
	rateToday := d.rateToday(t)

	// TSLA's lots converted at their purchase days, not the transfer day:
	//
	// 	lot 1: 5 @ $180.00 on 2026-05-13 -> 90_000 minor USD, rate 60.00 -> 5_400_000
	// 	lot 2: 5 @ $200.00 on 2026-06-15 -> 100_000 minor USD, rate 64.00 -> 6_400_000
	// 	correct in_base.cost_minor = 5_400_000 + 6_400_000 = 11_800_000 (118 000,00 ₽)
	// 	collapsed to 2026-07-20 (78.50): 190_000 * 78.50 = 14_915_000 (149 150,00 ₽)
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

	// The source account's journal row converts the same breakdown (see
	// operation.Store.attachTransferLots), so it shows 118 000,00 ₽ too.
	tbankJournal, err := opStore.ListForEngine(ctx, d.space, tbankID)
	if err != nil {
		t.Fatalf("ListForEngine Т-Банк: %v", err)
	}
	// TSLA's leg specifically: NVDA leaves the same day.
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
}

func TestSeedDemoNvidiaLeavesTheEarliestParcelFirst(t *testing.T) {
	t.Parallel()
	d := seeded(t)
	ctx := d.ctx
	converter := d.conv
	tbankPositions := d.tbank
	freedomPositions := d.freedom
	day := func(s string) time.Time { return demoDay(t, s) }

	// NVDA: a parcel that arrived by transfer but was bought earlier leaves first
	// (НК РФ ст. 214.1 п. 13; 26 CFR 1.1012-1(c)(1)(i)).
	//
	// 	transferred parcel: 10 @ $100.00 bought 2026-05-14 at Т-Банк -> 100_000 minor USD
	// 	parcel already there: 10 @ $150.00 bought 2026-06-20 at Freedom -> 150_000
	// 	sale: 10 @ $200.00 on 2026-07-22 -> 200_000
	//
	// 	by purchase day:
	// 	  realized = 200_000 − 100_000 = +100_000 (+$1 000.00)
	// 	  left      = the 2026-06-20 parcel, cost 150_000 ($1 500.00)
	// 	    in rubles 150_000 × 65.00 = 9_750_000 (97 500,00 ₽)
	// 	by arrival (wrong):
	// 	  realized =  200_000 − 150_000 = +50_000 (+$500.00)
	// 	  left      = the transferred parcel, cost 100_000 ($1 000.00)
	// 	    in rubles 100_000 × 60.50 = 6_050_000 (60 500,00 ₽)
	//
	// The wrong figures are named by value.
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
	switch portfoliotest.Realized(t, nvda) {
	case 100_000:
		// 200_000 − 100_000: the earliest acquisition was released.
	case 50_000:
		t.Errorf("NVDA realized P&L = 50000 (+$500.00): the sale was matched against the later, dearer parcel — arrival order again")
	default:
		t.Errorf("NVDA realized P&L = %d, want 100000 (+$1 000.00)", portfoliotest.Realized(t, nvda))
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
	// NVDA's settled result differs from a single-rate conversion without
	// flipping sign:
	//
	// 	proceeds 200_000 on 2026-07-22 -> nearest earlier rate, 2026-07-20's 78.50
	// 	  -> 15_700_000
	// 	basis    100_000 bought 2026-05-14, rate 60.50 -> 6_050_000
	// 	  in RUB: 15_700_000 − 6_050_000 = +9_650_000 (+96 500,00 ₽)
	nvdaBase := d.realizedInBase(t, nvda, "NVDA")
	if nvdaBase != 9_650_000 {
		t.Errorf("NVDA realized P&L in RUB = %d, want 9650000 (15 700 000 − 6 050 000 = +96 500,00 ₽)", nvdaBase)
	}
}

func TestSeedDemoAlphabetsSettledResultFlipsSignInRoubles(t *testing.T) {
	t.Parallel()
	d := seeded(t)
	ctx := d.ctx
	converter := d.conv
	freedomPositions := d.freedom
	day := func(s string) time.Time { return demoDay(t, s) }

	// Alphabet: the closed deal whose settled result has opposite signs.
	//
	// 	buy  50 @ $200.00 on 2026-06-10 -> 1_000_000 minor USD, rate 81.40 -> 81_400_000
	// 	sell 50 @ $210.00 on 2026-06-20 -> 1_050_000 minor USD, rate 65.00 -> 68_250_000
	// 	  in USD: 1_050_000 −  1_000_000 =     +50_000 (+$500.00, a gain)
	// 	  in RUB: 68_250_000 − 81_400_000 = −13_150_000 (−131 500,00 ₽, a loss)
	//
	// Any single seeded rate gives +30 000,00 to +40 700,00 ₽, a profit.
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
	if portfoliotest.Realized(t, googl) != 50_000 {
		t.Errorf("GOOGL realized P&L = %d, want 50000 (+$500.00 = 1050000 − 1000000)", portfoliotest.Realized(t, googl))
	}
	googlBase := d.realizedInBase(t, googl, "GOOGL")
	if googlBase != -13_150_000 {
		t.Errorf("GOOGL realized P&L in RUB = %d, want -13150000 (68 250 000 − 81 400 000 = −131 500,00 ₽)", googlBase)
	}
	if portfoliotest.Realized(t, googl) <= 0 || googlBase >= 0 {
		t.Errorf("GOOGL settled result = %d in USD and %d in RUB: the demo must contain one CLOSED deal that is a profit in the position's currency and a loss in rubles — without it plan 7b's consequence cannot be seen on demo data at all",
			portfoliotest.Realized(t, googl), googlBase)
	}
	// The answer a single-rate conversion of the dollar result would give,
	// named by value so a seed edit that makes the two agree is unmistakable.
	rateOnSale, _, err := converter.Rate(ctx, "USD", "RUB", day("2026-06-20"))
	if err != nil {
		t.Fatalf("Rate(USD -> RUB, 2026-06-20): %v", err)
	}
	if flat := decimal.NewFromInt(portfoliotest.Realized(t, googl)).Mul(rateOnSale).Round(0).IntPart(); flat != 3_250_000 || flat <= 0 {
		t.Errorf("GOOGL result converted at the sale day's rate alone = %d, want 3250000 (+32 500,00 ₽, a PROFIT) — the point of this deal is that no single rate reproduces −131 500,00 ₽", flat)
	}
}

func TestSeedDemoFreedomsRealizedTotalHasOppositeSigns(t *testing.T) {
	t.Parallel()
	d := seeded(t)
	freedomPositions := d.freedom

	// The account's «Зафиксировано» line, summed as portfolio.realizedTotals
	// does, with opposite signs across the display toggle:
	//
	// 	NVDA     +100_000 USD    +9_650_000 ₽
	// 	GOOGL     +50_000 USD   −13_150_000 ₽
	// 	AAPL, MSFT, TSLA, KAZ32EUR, WEWKQ, INTC: no disposals, zero in both,
	// 	  and no rate asked for
	// 	total    +150_000 USD    −3_500_000 ₽
	// 	        (+$1 500.00)     (−35 000,00 ₽)
	var accountUSD, accountRUB int64
	for ticker, pos := range freedomPositions {
		accountUSD += portfoliotest.Realized(t, pos)
		accountRUB += d.realizedInBase(t, pos, ticker)
	}
	if accountUSD != 150_000 || accountRUB != -3_500_000 {
		t.Errorf("Freedom KZ realized total = %d USD / %d RUB, want 150000 (+$1 500.00) / -3500000 (−35 000,00 ₽)", accountUSD, accountRUB)
	}
	if accountUSD <= 0 || accountRUB >= 0 {
		t.Errorf("Freedom KZ realized total = %d USD / %d RUB: the account's own «Зафиксировано» line must come out with opposite signs in the two display modes — that is what makes the plan's consequence visible without opening a single position",
			accountUSD, accountRUB)
	}
}

func TestSeedDemoIntelHasNoPurchaseDate(t *testing.T) {
	t.Parallel()
	d := seeded(t)
	aapl := position(t, d.freedom, "AAPL", "Freedom holds Apple")
	tbankPositions := d.tbank
	freedomPositions := d.freedom

	// Two different "no rouble figures" sentences on one screen (#66):
	//
	// 	Apple — one lot's purchase date has no rate; the backfill closes it
	// 	        (pinned above by lotsWithoutRate).
	// 	Intel — no purchase date at all: a hand-typed basis; nothing closes it.
	//
	// Intel's lot is the only dateless one, and Apple's are all dated; without the
	// second check both could read the same sentence.
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
}

func TestSeedDemoEurobondIsValuedInAThirdCurrency(t *testing.T) {
	t.Parallel()
	d := seeded(t)
	ctx := d.ctx
	pool := d.pool
	instStore := d.catalog
	converter := d.conv
	freedomPositions := d.freedom
	on := demoToday
	rateToday := d.rateToday(t)

	// The third-currency valuation (#39): a euro face, a dollar position, a rouble
	// space. Redone from the seeded face, quote and rates as portfolio.marketValue
	// and Handler.positionInBase do. The face and position currencies differing is
	// also the trade dialog's refusal case (#77, faceGapOf in
	// web/src/routes/accounts/trade-dialog.tsx); the OFZ is the agreeing case.
	//
	// 	valuation  100_000 (€1 000,00 face) × 98.00 % × 5 =    490_000 minor EUR
	// 	  in $     490_000 × (92.30 ÷ 78.50)              =    576_140 ($5 761,40)
	// 	  in ₽     490_000 × 92.30, from the euros        = 45_227_000 (452 270,00 ₽)
	// 	  in ₽     576_140 × 78.50, chained via the dollar = 45_226_990 (ten kopecks short)
	// 	cost       575_000 minor USD × 65.00 (its lot's own day) = 37_375_000
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
	// EUR/USD is bridged through the rouble (marketdata.resolveRate); asked
	// of the converter so it is the handler's rate.
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
}

func TestSeedDemoWeWorkIsQuotedBelowACent(t *testing.T) {
	t.Parallel()
	d := seeded(t)
	ctx := d.ctx
	pool := d.pool
	instStore := d.catalog
	freedomPositions := d.freedom
	on := demoToday

	// The sub-cent quote (#30), pinned by its property: below a hundredth,
	// above zero, on a share (other types have no price line).
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
}

func TestSeedDemoTotalConvertsEveryCurrency(t *testing.T) {
	t.Parallel()
	d := seeded(t)
	ctx := d.ctx
	pool := d.pool
	converter := d.conv
	tbankPositions := d.tbank
	totals := d.totals
	on := demoToday

	// Every held currency has a rate into RUB, so GET /summary's total has
	// nothing unconverted; mirrors handleSummary.
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
	// USD's only rate used is 2026-07-20's.
	if !ratesOn.Equal(on) {
		t.Errorf("ConvertMany ratesOn = %v, want %v (seeded USD/RUB rate date)", ratesOn, on)
	}

	// SBER has a quote, so its position is valued, as GET .../positions
	// does.
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
}

func TestSeedDemoBondPriceIsTheTradeDialogs(t *testing.T) {
	t.Parallel()
	d := seeded(t)
	ctx := d.ctx
	pool := d.pool
	instStore := d.catalog
	tbankPositions := d.tbank
	tbankID := d.tbankID

	// The OFZ row is what the trade dialog produces from 95,00 % (#77):
	//
	// 	1 000,00 ₽ face × 95 % = 950,00 ₽ a bond × 100 = 95 000,00 ₽ recorded
	//
	// The price is recomputed from the seeded face value, so moving one without the
	// other fails. The face currency equals the instrument's, the dialog's condition
	// for converting (faceGapOf); the eurobond is the other case.
	ofz, ok := tbankPositions["SU26238RMFS4"]
	if !ok {
		t.Fatal("missing Т-Банк position SU26238RMFS4 — the seed no longer holds the bond whose price the trade dialog is demonstrated on")
	}
	ofzInst, err := instStore.ByID(ctx, ofz.InstrumentID)
	if err != nil {
		t.Fatalf("instrument ByID SU26238RMFS4: %v", err)
	}
	if ofzInst.FaceValueMinor == nil || ofzInst.FaceCurrency == nil || *ofzInst.FaceCurrency != ofzInst.Currency {
		t.Fatalf("OFZ face = %v %v against instrument currency %s, want a face value denominated in the instrument's own currency: without that equality the trade dialog refuses the conversion and this row demonstrates nothing",
			ofzInst.FaceValueMinor, ofzInst.FaceCurrency, ofzInst.Currency)
	}
	// bondPriceFromPercent's two shifts (web/src/lib/money.ts): minor to
	// major, percent to fraction.
	wantOFZPrice := decimal.NewFromInt(*ofzInst.FaceValueMinor).Shift(-2).
		Mul(decimal.RequireFromString("95")).Shift(-2)
	tbankOps, err := operation.NewStore(pool).ListForEngine(ctx, d.space, tbankID)
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
			t.Errorf("OFZ buy has price %v and quantity %v, want both recorded", op.Price, op.Quantity)
			continue
		}
		if !op.Price.Equal(wantOFZPrice) {
			t.Errorf("OFZ buy price = %s, want %s (95 %% of the seeded face value, in %s): the operation and the comment above it have to say the same thing",
				op.Price.String(), wantOFZPrice.String(), ofzInst.Currency)
			continue
		}
		// Quantity × price, which is exactly what the dialog's «Итого» shows
		// and what it sends as amount_minor.
		if want := op.Price.Mul(*op.Quantity).Shift(2).Round(0).IntPart(); -op.AmountMinor != want {
			t.Errorf("OFZ buy amount = %d, want %d (quantity × price)", -op.AmountMinor, want)
		}
	}
	if ofzBuys != 1 {
		t.Errorf("OFZ buys = %d, want exactly 1 — the arithmetic above is stated for a single purchase", ofzBuys)
	}
}

func TestSeedDemoQuotesAreDatedBySessions(t *testing.T) {
	t.Parallel()
	d := seeded(t)
	ctx := d.ctx
	pool := d.pool
	tbankPositions := d.tbank
	freedomPositions := d.freedom
	on := demoToday

	// Quote dates are sessions (#90), shown as «Цена на …». They do not all
	// share one date (that would read as a page stamp), and each is a weekday no
	// later than the demo's today (the worker refuses future dates). Read through
	// LatestQuotes, as the positions handler does.
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
}

// The demo connection is consistent with its own data and cannot reach the
// broker. Counters are compared against the mirror, not against seed.go's
// numbers.
func TestSeedTinvestDemoIsSelfConsistentAndCannotReachTheBroker(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	if err := seedDemo(ctx, pool); err != nil {
		t.Fatalf("seedDemo: %v", err)
	}
	svc := family.NewService(family.NewStore(pool))
	_, p, err := svc.Login(ctx, "demo", "demo1234")
	if err != nil {
		t.Fatalf("login demo: %v", err)
	}
	store := tinvest.NewStore(pool)

	conns, err := store.ListConnections(ctx, p.SpaceID)
	if err != nil || len(conns) != 1 {
		t.Fatalf("connections = %d, %v; want 1", len(conns), err)
	}
	conn := conns[0]
	if conn.Status != tinvest.StatusDisabled {
		t.Errorf("status = %q, want %q: an active demo connection would be picked up by the "+
			"hourly dispatcher and would go to the network from the stand", conn.Status, tinvest.StatusDisabled)
	}
	if conn.TokenLast4 != "0000" {
		t.Errorf("token_last4 = %q, want 0000", conn.TokenLast4)
	}
	// The property behind "disabled", checked through the very read the
	// scheduler makes rather than through the word on the row.
	active, err := store.ListActiveConnections(ctx)
	if err != nil {
		t.Fatalf("list active connections: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("the scheduler would sync %d seeded connections, want 0", len(active))
	}

	links, err := store.LinksByConnection(ctx, conn.ID)
	if err != nil || len(links) != 1 {
		t.Fatalf("links = %d, %v; want 1", len(links), err)
	}
	accounts, err := account.NewStore(pool).ListWithBalance(ctx, p.SpaceID)
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	var linkedName string
	for _, a := range accounts {
		if a.ID == links[0].AccountID {
			linkedName = a.Name
		}
	}
	if linkedName != "Брокерский Т-Банк" {
		t.Errorf("the demo link feeds account %q, want «Брокерский Т-Банк»", linkedName)
	}

	rows, err := store.MirrorRowsByLink(ctx, links[0].ID)
	if err != nil {
		t.Fatalf("mirror rows: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("mirror rows = %d, want 3", len(rows))
	}
	gone := 0
	for _, r := range rows {
		if r.UnparsedReason == "" {
			t.Errorf("mirror row %s (%s) has no unparsed reason, so it claims to have become a "+
				"journal entry — and the seed writes none", r.ID, r.BrokerOperationID)
		}
		if r.DisappearedAt != nil {
			gone++
		}
	}
	if gone != 1 {
		t.Errorf("mirror rows the broker stopped returning = %d, want exactly 1: the demo exists "+
			"to show that such a row is marked and kept rather than deleted", gone)
	}

	unparsed, hasMore, err := store.UnparsedByConnection(ctx, conn.ID, 10, 0)
	if err != nil {
		t.Fatalf("unparsed: %v", err)
	}
	if len(unparsed) != 3 || hasMore {
		t.Fatalf("unparsed = %d (has_more=%v), want 3 and false — including the one that "+
			"disappeared", len(unparsed), hasMore)
	}

	runs, _, err := store.RunsByConnection(ctx, conn.ID, 10, 0)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}
	// Runs are picked by trigger, and the log's order is checked on its own,
	// so a reversed log fails with the right message.
	byTrigger := map[tinvest.SyncTrigger]tinvest.SyncRun{}
	for _, r := range runs {
		byTrigger[r.Trigger] = r
	}
	first, ok := byTrigger[tinvest.TriggerInitial]
	if !ok {
		t.Fatalf("no initial run in the log, only %+v", runs)
	}
	second, ok := byTrigger[tinvest.TriggerSchedule]
	if !ok {
		t.Fatalf("no scheduled run in the log, only %+v", runs)
	}
	for _, r := range runs {
		if r.Status != tinvest.RunOK {
			t.Errorf("run %s finished %q, want ok", r.ID, r.Status)
		}
		if r.UnparsedCount != len(unparsed) {
			t.Errorf("run %s says %d unparsed, but the connection holds %d",
				r.ID, r.UnparsedCount, len(unparsed))
		}
	}

	// Stated instants an hour apart, not the transaction's single now(), or
	// the order would depend on random uuids. Written out, not read from the
	// mirror.
	wantFirst := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	wantSecond := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	if !first.StartedAt.Equal(wantFirst) || !second.StartedAt.Equal(wantSecond) {
		t.Errorf("the runs started at %s and %s, want %s and %s",
			first.StartedAt.UTC(), second.StartedAt.UTC(), wantFirst, wantSecond)
	}
	for _, r := range runs {
		if r.FinishedAt == nil || !r.FinishedAt.Equal(r.StartedAt) {
			t.Errorf("run %q started at %s and finished at %v, want the same stated instant: a "+
				"seeded run has no duration anybody measured", r.Trigger, r.StartedAt, r.FinishedAt)
		}
	}
	if runs[0].Trigger != tinvest.TriggerSchedule {
		t.Errorf("the log hands over the %q run first, want the later (%q) one — it is read "+
			"newest first, and that order is what the demo's run table shows",
			runs[0].Trigger, tinvest.TriggerSchedule)
	}

	if first.ReadCount != 3 || first.AddedCount != 3 || first.DisappearedCount != 0 {
		t.Errorf("the first run = %+v, want the initial import reading and adding all three",
			[]int{first.ReadCount, first.AddedCount, first.DisappearedCount})
	}
	if second.ReadCount != 2 || second.AddedCount != 0 || second.DisappearedCount != gone {
		t.Errorf("the second run read=%d added=%d disappeared=%d, want 2/0/%d — the broker "+
			"returned one operation fewer and nothing new", second.ReadCount, second.AddedCount,
			second.DisappearedCount, gone)
	}
	if second.ReconcileStatus != tinvest.ReconcileMismatched || second.ReconciledAt == nil {
		t.Errorf("the second run's check = %q at %v, want a mismatched verdict with a date",
			second.ReconcileStatus, second.ReconciledAt)
	}
	var mismatches []tinvest.ReconcileMismatch
	if err := json.Unmarshal(second.ReconcileMismatches, &mismatches); err != nil {
		t.Fatalf("decode the seeded differences: %v", err)
	}
	if len(mismatches) != 1 || mismatches[0].Kind != tinvest.MismatchUnsupported {
		t.Errorf("differences = %+v, want exactly one of kind %q", mismatches, tinvest.MismatchUnsupported)
	}
	if first.ReconcileStatus != tinvest.ReconcileNotChecked || first.ReconciledAt != nil {
		t.Errorf("the first run's check = %q at %v, want not_checked with no date: nothing "+
			"reconciled it, and «nobody looked» is not «everything agreed»",
			first.ReconcileStatus, first.ReconciledAt)
	}
}

// A seed failing after the users are written leaves the instance seedable.
// A pre-existing "SBER" collides with the catalogue's first instrument (migration
// 0011's index), a real error; the obstacle is removed and the second run must
// pass the emptiness guard.
func TestASeedThatFailsPartWayLeavesTheInstanceSeedableAgain(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	blocker, err := instrument.NewStore(pool).Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Чужой Сбербанк", Ticker: "SBER", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("create the blocking instrument: %v", err)
	}

	if err := seedDemo(ctx, pool); err == nil {
		t.Fatal("the seed succeeded although the ticker SBER was already taken; " +
			"this test proves nothing unless that collision stops it")
	}

	// Nothing of the seed survived: no users, so the instance still reports it
	// needs setting up, and no accounts either.
	svc := family.NewService(family.NewStore(pool))
	needed, err := svc.SetupNeeded(ctx)
	if err != nil {
		t.Fatalf("SetupNeeded after the failed seed: %v", err)
	}
	if !needed {
		t.Error("the failed seed left users behind: the instance now claims to be set up, " +
			"and no seed will ever run on it again")
	}
	var accounts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM accounts`).Scan(&accounts); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if accounts != 0 {
		t.Errorf("the failed seed left %d accounts behind, want none", accounts)
	}

	// Remove the obstacle and seed for real.
	if _, err := pool.Exec(ctx, `DELETE FROM instruments WHERE id = $1`, blocker.ID); err != nil {
		t.Fatalf("delete the blocking instrument: %v", err)
	}
	if err := seedDemo(ctx, pool); err != nil {
		t.Fatalf("the second seed failed: %v", err)
	}
	if _, p, err := svc.Login(ctx, "demo", "demo1234"); err != nil || p.Role != family.RoleOwner {
		t.Fatalf("login demo after the second seed: %v %+v", err, p)
	}
}

// The demo family's card has two months of spending, each under a category and
// with whom it was; the current account keeps one spending unfiled.
func TestSeedDemoFamilySpendsUnderCategories(t *testing.T) {
	d := seeded(t)
	byName := map[string]uuid.UUID{}
	for _, a := range d.accounts {
		byName[a.Name] = a.ID
	}
	card, err := d.ops.ListForEngine(d.ctx, d.space, byName["Кредитка Альфа"])
	if err != nil {
		t.Fatal(err)
	}
	if len(card) == 0 {
		t.Fatal("the card has no spending")
	}
	for _, op := range card {
		if op.Type != operation.TypeWithdrawal || op.CategoryID == nil || op.Counterparty == "" {
			t.Errorf("a card row is not a filed spending: %s %v %q", op.Type, op.CategoryID, op.Counterparty)
		}
	}
	current, _, err := d.ops.ListByAccount(d.ctx, d.space, byName["Текущий Сбер"], 100, 0, operation.JournalFilter{Uncategorized: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 || current[0].AmountMinor != -15_000_00 {
		t.Errorf("unfiled on the current account: %+v, want the one transfer to Иван", current)
	}
}
