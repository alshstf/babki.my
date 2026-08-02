package portfolio_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/portfolio"
)

var (
	sber = uuid.New()
	lkoh = uuid.New()
	ofz  = uuid.New()
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }
func dp(s string) *decimal.Decimal {
	v := d(s)
	return &v
}

func day(n int) time.Time {
	return time.Date(2026, 7, n, 0, 0, 0, 0, time.UTC)
}

// dayp is day as an acquisition date a lot actually knows (portfolio.Lot and
// portfolio.ReleasedLot hold it as a pointer, nil meaning "not knowable" —
// see Lot.AcquiredOn).
func dayp(n int) *time.Time {
	t := day(n)
	return &t
}

// acquired renders an acquisition date for a failure message, naming the
// unknown case rather than printing a stand-in date for it. Every assertion
// below reports through this, so a test that fails because a date went missing
// says so instead of showing 0001-01-01 or a nil pointer.
func acquired(t *time.Time) string {
	if t == nil {
		return "unknown"
	}
	return t.Format("2006-01-02")
}

// sameAcquisition compares two acquisition dates including the unknown case:
// two unknowns match, and an unknown never matches a date. Assertions must go
// through it rather than calling Equal on a pointer, which panics on an
// unknown date and would turn "the lot lost its date" into a crash in the
// test's own reporting code.
func sameAcquisition(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func op(typ portfolio.Type, dayN int, inst *uuid.UUID, qty, price string, amount, fee int64) portfolio.Operation {
	o := portfolio.Operation{
		Type: typ, OccurredOn: day(dayN), AmountMinor: amount,
		Currency: "RUB", FeeMinor: fee, InstrumentID: inst,
	}
	if qty != "" {
		o.Quantity = dp(qty)
	}
	if price != "" {
		o.Price = dp(price)
	}
	return o
}

func TestBuySellFIFO(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeDeposit, 1, nil, "", "", 1_000_000, 0),
		// 10 × 100.00 + fee 10
		op(portfolio.TypeBuy, 2, &sber, "10", "100", -100_000, 10),
		// 10 × 110.00 + fee 11
		op(portfolio.TypeBuy, 3, &sber, "10", "110", -110_000, 11),
		// sell 15 × 120.00, fee 18: released = lot1 fully (100010) + 5/10 of lot2 (floor(110011*0.5)=55005)
		op(portfolio.TypeSell, 4, &sber, "15", "120", 180_000, 18),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	if p == nil {
		t.Fatal("no SBER position")
	}
	if !p.Quantity.Equal(d("5")) {
		t.Errorf("qty = %s, want 5", p.Quantity)
	}
	wantReleased := int64(100_010 + 55_005)
	wantRealized := 180_000 - wantReleased - 18
	if p.RealizedPnLMinor != wantRealized {
		t.Errorf("realized = %d, want %d", p.RealizedPnLMinor, wantRealized)
	}
	// remaining cost = full cost of both lots − released (not a cent of drift)
	if p.CostMinor != (100_010+110_011)-wantReleased {
		t.Errorf("cost = %d", p.CostMinor)
	}
	if p.FeesMinor != 10+11+18 {
		t.Errorf("fees = %d", p.FeesMinor)
	}
}

func TestLotDrainNoRoundingDrift(t *testing.T) {
	// Lot of 3 shares at 100.00 (cost 30000): sells of 1+1+1 — released sums to exactly 30000.
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "3", "100", -30_000, 0),
	}
	// lot cost = 30000; equal thirds of 10000 — no drift, remainder 0
	for i := 0; i < 3; i++ {
		ops = append(ops, op(portfolio.TypeSell, 2+i, &sber, "1", "100", 10_000, 0))
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	if !p.Quantity.IsZero() || p.CostMinor != 0 {
		t.Errorf("qty=%s cost=%d, want 0/0", p.Quantity, p.CostMinor)
	}
	if p.RealizedPnLMinor != 0 {
		t.Errorf("realized = %d, want 0", p.RealizedPnLMinor)
	}
}

func TestDriftRemainderGoesToLastPiece(t *testing.T) {
	// Lot of 3 at 100.01 (cost 10001; fee 0) and three sells of 1 each.
	// Step by step: floor(10001*1/3)=3333 (lot: cost 6668, qty 2);
	// floor(6668*1/2)=3334 (lot: cost 3334, qty 1); the last piece
	// takes the lot's remaining cost 3334. Sum 3333+3334+3334 = 10001 — exact.
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "3", "", -10_001, 0),
		op(portfolio.TypeSell, 2, &sber, "1", "", 4_000, 0),
		op(portfolio.TypeSell, 3, &sber, "1", "", 4_000, 0),
		op(portfolio.TypeSell, 4, &sber, "1", "", 4_000, 0),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	if p.CostMinor != 0 {
		t.Errorf("cost = %d, want 0", p.CostMinor)
	}
	if p.RealizedPnLMinor != 12_000-10_001 {
		t.Errorf("realized = %d, want %d", p.RealizedPnLMinor, 12_000-10_001)
	}
}

func TestOversellRejected(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "10", "100", -100_000, 0),
		op(portfolio.TypeSell, 2, &sber, "11", "100", 110_000, 0),
	}
	_, err := portfolio.Compute(ops)
	if !errors.Is(err, portfolio.ErrOversell) {
		t.Fatalf("err = %v, want ErrOversell", err)
	}
	// Verify instrument ID is included in error message
	if !strings.Contains(err.Error(), sber.String()) {
		t.Errorf("error message missing instrument ID: %v", err)
	}
}

func TestConversionWithInstrumentNoGhost(t *testing.T) {
	// A conversion operation with instrument_id should not create a ghost position
	ops := []portfolio.Operation{
		// Create a conversion op with instrument — it's cash-level and should be ignored,
		// not creating an empty position in the result map
		op(portfolio.TypeConversion, 1, &sber, "", "", 0, 0),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if len(pos) != 0 {
		t.Errorf("positions = %d, want 0 (no ghost position)", len(pos))
	}
}

func TestIncomeAndTaxes(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "10", "100", -100_000, 0),
		op(portfolio.TypeDividend, 5, &sber, "", "", 3_480, 0),
		op(portfolio.TypeTax, 5, &sber, "", "", -452, 0),
		// dividend/tax without instrument — cash-level, ignored
		op(portfolio.TypeInterest, 6, nil, "", "", 1_000, 0),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if pos[sber].IncomeMinor != 3_480-452 {
		t.Errorf("income = %d", pos[sber].IncomeMinor)
	}
	if len(pos) != 1 {
		t.Errorf("positions = %d, want 1", len(pos))
	}
}

func TestAmortizationReducesCost(t *testing.T) {
	// Bond: 10 units at 950.00 (cost 950000). Amortization 250 per unit → 2500.00 total.
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &ofz, "10", "950", -950_000, 0),
		op(portfolio.TypeAmortization, 10, &ofz, "", "", 250_000, 0),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if pos[ofz].CostMinor != 700_000 {
		t.Errorf("cost = %d, want 700000", pos[ofz].CostMinor)
	}
	// amortization beyond remaining cost basis goes to Realized
	ops = append(ops, op(portfolio.TypeAmortization, 11, &ofz, "", "", 800_000, 0))
	pos, err = portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute 2: %v", err)
	}
	if pos[ofz].CostMinor != 0 || pos[ofz].RealizedPnLMinor != 100_000 {
		t.Errorf("cost=%d realized=%d, want 0/100000", pos[ofz].CostMinor, pos[ofz].RealizedPnLMinor)
	}
}

func TestClosedPositionKeptInResult(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &lkoh, "5", "7000", -3_500_000, 0),
		op(portfolio.TypeSell, 2, &lkoh, "5", "7500", 3_750_000, 0),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[lkoh]
	if p == nil || !p.Quantity.IsZero() || p.RealizedPnLMinor != 250_000 {
		t.Fatalf("closed position = %+v", p)
	}
}

// TestCurrencyMismatchRejected pins the per-position currency invariant: the
// first operation fixes the currency, and mixing another one into the same
// position would sum unrelated minor units into a single int64.
func TestCurrencyMismatchRejected(t *testing.T) {
	inCurrency := func(o portfolio.Operation, cur string) portfolio.Operation {
		o.Currency = cur
		return o
	}
	buyRUB := op(portfolio.TypeBuy, 1, &sber, "10", "100", -100_000, 0)

	for name, bad := range map[string]portfolio.Operation{
		"dividend in another currency": inCurrency(op(portfolio.TypeDividend, 2, &sber, "", "", 3_000, 0), "USD"),
		"sell in another currency":     inCurrency(op(portfolio.TypeSell, 3, &sber, "10", "120", 120_000, 0), "EUR"),
		"buy in another currency":      inCurrency(op(portfolio.TypeBuy, 4, &sber, "1", "100", -10_000, 0), "USD"),
		"transfer_in another currency": inCurrency(op(portfolio.TypeTransferIn, 5, &sber, "1", "", 10_000, 0), "USD"),
	} {
		_, err := portfolio.Compute([]portfolio.Operation{buyRUB, bad})
		if !errors.Is(err, portfolio.ErrBadOperation) {
			t.Errorf("%s: err = %v, want ErrBadOperation", name, err)
			continue
		}
		// The message must name both currencies and the instrument so the
		// user can find the offending row.
		for _, want := range []string{"RUB", bad.Currency, sber.String()} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q missing %q", name, err, want)
			}
		}
	}

	// A different instrument may of course carry a different currency.
	usdBuy := inCurrency(op(portfolio.TypeBuy, 2, &lkoh, "1", "100", -10_000, 0), "USD")
	pos, err := portfolio.Compute([]portfolio.Operation{buyRUB, usdBuy})
	if err != nil {
		t.Fatalf("per-instrument currencies: %v", err)
	}
	if pos[sber].Currency != "RUB" || pos[lkoh].Currency != "USD" {
		t.Errorf("currencies = %s/%s, want RUB/USD", pos[sber].Currency, pos[lkoh].Currency)
	}
}

// lotSums totals the position's remaining lots. The engine must keep these
// totals exactly equal to the position's own Quantity and CostMinor.
func lotSums(p *portfolio.Position) (decimal.Decimal, int64) {
	qty := decimal.Zero
	var cost int64
	for _, l := range p.Lots {
		qty = qty.Add(l.Quantity)
		cost += l.CostMinor
	}
	return qty, cost
}

func checkLotInvariants(t *testing.T, p *portfolio.Position) {
	t.Helper()
	qty, cost := lotSums(p)
	if !qty.Equal(p.Quantity) {
		t.Errorf("sum of lot quantities = %s, want position quantity %s", qty, p.Quantity)
	}
	if cost != p.CostMinor {
		t.Errorf("sum of lot costs = %d, want position cost %d", cost, p.CostMinor)
	}
	if p.Quantity.IsZero() && len(p.Lots) != 0 {
		t.Errorf("closed position keeps %d lots, want none", len(p.Lots))
	}
}

func TestLotsCarryAcquisitionDates(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 2, &sber, "10", "100", -100_000, 10),
		op(portfolio.TypeBuy, 9, &sber, "5", "110", -55_000, 5),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	want := []portfolio.Lot{
		{Quantity: d("10"), CostMinor: 100_010, AcquiredOn: dayp(2)},
		{Quantity: d("5"), CostMinor: 55_005, AcquiredOn: dayp(9)},
	}
	if len(p.Lots) != len(want) {
		t.Fatalf("lots = %d, want %d", len(p.Lots), len(want))
	}
	for i, w := range want {
		got := p.Lots[i]
		if !got.Quantity.Equal(w.Quantity) || got.CostMinor != w.CostMinor || !sameAcquisition(got.AcquiredOn, w.AcquiredOn) {
			t.Errorf("lot %d = {qty %s cost %d on %s}, want {qty %s cost %d on %s}",
				i, got.Quantity, got.CostMinor, acquired(got.AcquiredOn),
				w.Quantity, w.CostMinor, acquired(w.AcquiredOn))
		}
	}
	checkLotInvariants(t, p)
}

func TestSellingWholeLotDropsIt(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 2, &sber, "10", "100", -100_000, 10),
		op(portfolio.TypeBuy, 9, &sber, "5", "110", -55_000, 5),
		// exactly the first lot
		op(portfolio.TypeSell, 12, &sber, "10", "120", 120_000, 0),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	if len(p.Lots) != 1 {
		t.Fatalf("lots = %d, want 1", len(p.Lots))
	}
	if !sameAcquisition(p.Lots[0].AcquiredOn, dayp(9)) {
		t.Errorf("remaining lot acquired on %s, want %s",
			acquired(p.Lots[0].AcquiredOn), day(9).Format("2006-01-02"))
	}
	if !p.Lots[0].Quantity.Equal(d("5")) || p.Lots[0].CostMinor != 55_005 {
		t.Errorf("remaining lot = {qty %s cost %d}, want {5 55005}", p.Lots[0].Quantity, p.Lots[0].CostMinor)
	}
	checkLotInvariants(t, p)
}

// TestPartialSellKeepsLotDate pins the rule that a partial sale only shrinks
// the lot: what is left over was acquired on the very same day as the part
// that was sold.
func TestPartialSellKeepsLotDate(t *testing.T) {
	ops := []portfolio.Operation{
		// 3 units for 100.01 total — deliberately not divisible by 3
		op(portfolio.TypeBuy, 2, &sber, "3", "", -10_001, 0),
		op(portfolio.TypeSell, 8, &sber, "1", "", 4_000, 0),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	if len(p.Lots) != 1 {
		t.Fatalf("lots = %d, want 1", len(p.Lots))
	}
	l := p.Lots[0]
	if !sameAcquisition(l.AcquiredOn, dayp(2)) {
		t.Errorf("lot acquired on %s, want the buy day %s",
			acquired(l.AcquiredOn), day(2).Format("2006-01-02"))
	}
	if !l.Quantity.Equal(d("2")) {
		t.Errorf("lot qty = %s, want 2", l.Quantity)
	}
	// floor(10001 * 1/3) = 3333 released, so 6668 stays with the lot
	if l.CostMinor != 6_668 {
		t.Errorf("lot cost = %d, want 6668", l.CostMinor)
	}
	checkLotInvariants(t, p)
}

func TestClosedPositionHasNoLots(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &lkoh, "5", "7000", -3_500_000, 0),
		op(portfolio.TypeBuy, 2, &lkoh, "2", "7100", -1_420_000, 0),
		op(portfolio.TypeSell, 3, &lkoh, "7", "7500", 5_250_000, 0),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[lkoh]
	if len(p.Lots) != 0 {
		t.Errorf("closed position lots = %+v, want none", p.Lots)
	}
	checkLotInvariants(t, p)
}

// TestLotsStayExactOverLongSequence is the discriminating one: through a long
// mix of buys and sells with awkward, non-divisible amounts the lot costs must
// still sum to the position cost to the last minor unit, and the total money
// spent on buys must equal what is still held plus what was released on sells.
// An implementation that loses a minor unit on a partial release fails here.
func TestLotsStayExactOverLongSequence(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "7", "", -100_003, 7),
		op(portfolio.TypeBuy, 2, &sber, "3", "", -33_337, 0),
		op(portfolio.TypeSell, 3, &sber, "5", "", 71_111, 3),
		op(portfolio.TypeBuy, 4, &sber, "11", "", -77_771, 3),
		op(portfolio.TypeSell, 5, &sber, "9", "", 91_119, 0),
		op(portfolio.TypeBuy, 6, &sber, "4", "", -10_007, 1),
		op(portfolio.TypeSell, 7, &sber, "6", "", 41_113, 7),
		op(portfolio.TypeSell, 8, &sber, "2", "", 13_337, 0),
		op(portfolio.TypeBuy, 9, &sber, "5", "", -12_345, 2),
		op(portfolio.TypeSell, 10, &sber, "2", "", 9_991, 1),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]

	var boughtMinor, proceedsMinor, sellFeesMinor int64
	for _, o := range ops {
		switch o.Type {
		case portfolio.TypeBuy:
			boughtMinor += -o.AmountMinor + o.FeeMinor
		case portfolio.TypeSell:
			proceedsMinor += o.AmountMinor
			sellFeesMinor += o.FeeMinor
		}
	}
	// realized = proceeds − released − fees, so released is observable from outside
	releasedMinor := proceedsMinor - sellFeesMinor - p.RealizedPnLMinor
	if boughtMinor != p.CostMinor+releasedMinor {
		t.Errorf("bought %d, but held %d + released %d = %d — %d minor units drifted",
			boughtMinor, p.CostMinor, releasedMinor, p.CostMinor+releasedMinor,
			boughtMinor-p.CostMinor-releasedMinor)
	}
	checkLotInvariants(t, p)

	// 7+3−5+11−9+4−6−2+5−2 = 6 units left: the tail of the day-6 lot and all of day 9.
	if !p.Quantity.Equal(d("6")) {
		t.Fatalf("qty = %s, want 6", p.Quantity)
	}
	wantDays := []int{6, 9}
	if len(p.Lots) != len(wantDays) {
		t.Fatalf("lots = %d, want %d", len(p.Lots), len(wantDays))
	}
	for i, dayN := range wantDays {
		if !p.Lots[i].AcquiredOn.Equal(day(dayN)) {
			t.Errorf("lot %d acquired on %s, want %s", i,
				acquired(p.Lots[i].AcquiredOn), day(dayN).Format("2006-01-02"))
		}
	}
}

// TestLotInvariantsUnderSplitAndAmortization covers the two operations that
// rewrite lots without buying or selling: a split scales quantities, an
// amortization drains cost. Both must leave the totals matching the position.
func TestLotInvariantsUnderSplitAndAmortization(t *testing.T) {
	split := op(portfolio.TypeSplit, 4, &ofz, "", "", 0, 0)
	split.SplitRatio = dp("3")
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &ofz, "7", "", -100_003, 0),
		op(portfolio.TypeBuy, 2, &ofz, "3", "", -33_337, 0),
		split,
		op(portfolio.TypeAmortization, 5, &ofz, "", "", 50_001, 0),
		op(portfolio.TypeSell, 6, &ofz, "25", "", 40_000, 0),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[ofz]
	if !p.Quantity.Equal(d("5")) {
		t.Fatalf("qty = %s, want 5", p.Quantity)
	}
	// the split does not change when the shares were acquired
	if len(p.Lots) != 1 || !sameAcquisition(p.Lots[0].AcquiredOn, dayp(2)) {
		t.Errorf("lots = %+v, want one acquired on %s", p.Lots, day(2).Format("2006-01-02"))
	}
	checkLotInvariants(t, p)
}

// TestTransferInWithoutBreakdownHasNoAcquisitionDate is the behavioural change
// this file exists to pin. A transfer_in with no stored FIFO breakdown
// (Operation.TransferLots) carries only a cost snapshot — its basis was typed
// in by hand, or it was recorded before breakdowns were kept — and no
// acquisition dates come with such a number.
//
// The engine used to date that lot on the transfer itself and call it "the
// best available answer". It is not an answer to the question the field asks.
// The field says when the shares were BOUGHT; the transfer day is when they
// changed brokers, which is a fact about paperwork. Written into the same slot
// as a real purchase date, in the same format, it became a fact: the ruble
// basis converted it at that day's fx rate and published the product, and
// nothing downstream could tell it from a date somebody actually recorded.
//
// So the lot now knows nothing about its date, and says so. The transfer day
// is asserted by name below, because it is the one wrong value this code has
// ever produced and the one a regression would produce again.
func TestTransferInWithoutBreakdownHasNoAcquisitionDate(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeTransferIn, 5, &sber, "4", "", 40_000, 0),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	if len(p.Lots) != 1 {
		t.Fatalf("lots = %d, want exactly 1 (one carried number, one lot)", len(p.Lots))
	}
	if sameAcquisition(p.Lots[0].AcquiredOn, dayp(5)) {
		t.Fatalf("transferred lot acquired on %s — that is the transfer's own date, the day the shares changed brokers; nobody recorded it as a purchase date and the engine must not either",
			acquired(p.Lots[0].AcquiredOn))
	}
	if p.Lots[0].AcquiredOn != nil {
		t.Errorf("transferred lot acquired on %s, want unknown: a transfer with no breakdown has no purchase dates behind it, and any date here is invented",
			acquired(p.Lots[0].AcquiredOn))
	}
	// The rest of the lot is untouched: only the date is unknown, the money and
	// the shares are as real as any other lot's.
	if !p.Lots[0].Quantity.Equal(d("4")) || p.Lots[0].CostMinor != 40_000 {
		t.Errorf("lot = {qty %s cost %d}, want {4 40000}", p.Lots[0].Quantity, p.Lots[0].CostMinor)
	}
	checkLotInvariants(t, p)
}

// TestUndatedLotBehavesLikeAnyOtherLot pins that "no date" is a missing date
// and nothing more: the lot is a full member of the FIFO queue. It is released
// in its turn, a partial release splits its cost the usual way and leaves the
// remainder still undated, and a split rescales it. A representation that
// treated the absence as a special kind of lot — dropped, floated to the end,
// merged into a neighbour — would change what the position is worth, which no
// missing date should ever do.
func TestUndatedLotBehavesLikeAnyOtherLot(t *testing.T) {
	split := op(portfolio.TypeSplit, 8, &sber, "", "", 0, 0)
	split.SplitRatio = dp("2")
	ops := []portfolio.Operation{
		// 3 units for 100.01 total, undated — deliberately not divisible by 3
		op(portfolio.TypeTransferIn, 5, &sber, "3", "", 10_001, 0),
		op(portfolio.TypeBuy, 6, &sber, "2", "", -20_000, 0),
		op(portfolio.TypeSell, 7, &sber, "1", "", 4_000, 0),
		split,
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	if len(p.Lots) != 2 {
		t.Fatalf("lots = %+v, want 2 (the undated remainder, then the buy)", p.Lots)
	}
	// The sale took its share of the UNDATED lot, first in the queue, not of
	// the dated buy behind it: floor(10001 * 1/3) = 3333 released, 6668 left.
	if p.Lots[0].AcquiredOn != nil {
		t.Errorf("first lot acquired on %s, want unknown — a release must not date what it leaves behind", acquired(p.Lots[0].AcquiredOn))
	}
	if p.Lots[0].CostMinor != 6_668 {
		t.Errorf("undated lot cost = %d, want 6668 (10001 − floor(10001/3))", p.Lots[0].CostMinor)
	}
	if p.RealizedPnLMinor != 4_000-3_333 {
		t.Errorf("realized = %d, want %d — the sale must consume the undated lot at ITS cost, not the buy's",
			p.RealizedPnLMinor, 4_000-3_333)
	}
	// 2 units left of the undated lot and 2 bought, both doubled by the split.
	if !sameAcquisition(p.Lots[1].AcquiredOn, dayp(6)) {
		t.Errorf("second lot acquired on %s, want %s — the dated lot keeps its date", acquired(p.Lots[1].AcquiredOn), acquired(dayp(6)))
	}
	if !p.Lots[0].Quantity.Equal(d("4")) || !p.Lots[1].Quantity.Equal(d("4")) {
		t.Errorf("quantities after the split = %s and %s, want 4 and 4 — a split rescales an undated lot like any other",
			p.Lots[0].Quantity, p.Lots[1].Quantity)
	}
	checkLotInvariants(t, p)
}

// TestBuyIsAlwaysDated is the other half of the rule: absence is reserved for
// what genuinely cannot be known. A purchase is recorded with the day it
// happened, so its lot always carries that day and a nil here would mean the
// engine had stopped distinguishing "not knowable" from "not bothered".
func TestBuyIsAlwaysDated(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 3, &sber, "10", "100", -100_000, 10),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	l := pos[sber].Lots[0]
	if l.AcquiredOn == nil {
		t.Fatalf("a buy produced a lot with an unknown acquisition date; the operation's own date %s is that date", day(3).Format("2006-01-02"))
	}
	if !sameAcquisition(l.AcquiredOn, dayp(3)) {
		t.Errorf("lot acquired on %s, want the buy's own day %s", acquired(l.AcquiredOn), acquired(dayp(3)))
	}
}

// TestReleasedLotsSingleLot pins the simple case: a release that fits
// entirely inside the oldest lot yields exactly one piece, carrying that
// lot's own acquisition date.
func TestReleasedLotsSingleLot(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 2, &sber, "10", "100", -100_000, 10),
	}
	lots, err := portfolio.ReleasedLots(ops, sber, d("4"))
	if err != nil {
		t.Fatalf("ReleasedLots: %v", err)
	}
	if len(lots) != 1 {
		t.Fatalf("pieces = %d, want 1", len(lots))
	}
	l := lots[0]
	if !l.Quantity.Equal(d("4")) {
		t.Errorf("qty = %s, want 4", l.Quantity)
	}
	if !sameAcquisition(l.AcquiredOn, dayp(2)) {
		t.Errorf("acquired = %s, want %s", acquired(l.AcquiredOn), day(2).Format("2006-01-02"))
	}
	// floor(100010 * 4/10) = 40004
	if l.CostMinor != 40_004 {
		t.Errorf("cost = %d, want 40004", l.CostMinor)
	}
}

// TestReleasedLotsCrossesTwoLots pins the multi-lot case: a release larger
// than the oldest lot must yield one piece per lot it touches, in FIFO
// order, each with its own cost and acquisition date.
func TestReleasedLotsCrossesTwoLots(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 2, &sber, "10", "100", -100_000, 10),
		op(portfolio.TypeBuy, 9, &sber, "5", "110", -55_000, 5),
	}
	lots, err := portfolio.ReleasedLots(ops, sber, d("15"))
	if err != nil {
		t.Fatalf("ReleasedLots: %v", err)
	}
	if len(lots) != 2 {
		t.Fatalf("pieces = %d, want 2", len(lots))
	}
	if !lots[0].Quantity.Equal(d("10")) || lots[0].CostMinor != 100_010 || !sameAcquisition(lots[0].AcquiredOn, dayp(2)) {
		t.Errorf("piece 0 = %+v, want {qty 10 cost 100010 on %s}", lots[0], day(2).Format("2006-01-02"))
	}
	if !lots[1].Quantity.Equal(d("5")) || lots[1].CostMinor != 55_005 || !sameAcquisition(lots[1].AcquiredOn, dayp(9)) {
		t.Errorf("piece 1 = %+v, want {qty 5 cost 55005 on %s}", lots[1], day(9).Format("2006-01-02"))
	}
}

// TestReleasedLotsPartialLot pins the partial-release rule: the piece takes
// a floored share of the lot's cost and inherits the lot's own acquisition
// date, exactly like the internal releaseFIFO behavior already pinned by
// TestPartialSellKeepsLotDate.
func TestReleasedLotsPartialLot(t *testing.T) {
	ops := []portfolio.Operation{
		// 3 units for 100.01 total — deliberately not divisible by 3
		op(portfolio.TypeBuy, 2, &sber, "3", "", -10_001, 0),
	}
	lots, err := portfolio.ReleasedLots(ops, sber, d("1"))
	if err != nil {
		t.Fatalf("ReleasedLots: %v", err)
	}
	if len(lots) != 1 {
		t.Fatalf("pieces = %d, want 1", len(lots))
	}
	l := lots[0]
	if !l.Quantity.Equal(d("1")) {
		t.Errorf("qty = %s, want 1", l.Quantity)
	}
	if !sameAcquisition(l.AcquiredOn, dayp(2)) {
		t.Errorf("acquired = %s, want the buy day %s", acquired(l.AcquiredOn), day(2).Format("2006-01-02"))
	}
	// floor(10001 * 1/3) = 3333
	if l.CostMinor != 3_333 {
		t.Errorf("cost = %d, want 3333", l.CostMinor)
	}
}

// TestReleasedLotsSumMatchesReleasedCost is the discriminating test: across a
// long, awkward mix of buys and sells (leftover lots with non-divisible
// costs) the sum of the pieces ReleasedLots returns must equal, to the last
// minor unit, what ReleasedCost returns for the very same release. An
// implementation that computes the pieces separately from the total — and
// drifts by even one minor unit on a partial piece — fails here. It also
// checks the pieces' quantities sum back to the requested release quantity.
func TestReleasedLotsSumMatchesReleasedCost(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "7", "", -100_003, 7),
		op(portfolio.TypeBuy, 2, &sber, "3", "", -33_337, 0),
		op(portfolio.TypeSell, 3, &sber, "5", "", 71_111, 3),
		op(portfolio.TypeBuy, 4, &sber, "11", "", -77_771, 3),
		op(portfolio.TypeSell, 5, &sber, "9", "", 91_119, 0),
		op(portfolio.TypeBuy, 6, &sber, "4", "", -10_007, 1),
		op(portfolio.TypeSell, 7, &sber, "6", "", 41_113, 7),
		op(portfolio.TypeSell, 8, &sber, "2", "", 13_337, 0),
		op(portfolio.TypeBuy, 9, &sber, "5", "", -12_345, 2),
		op(portfolio.TypeSell, 10, &sber, "2", "", 9_991, 1),
	}
	// 6 units remain after this sequence (see TestLotsStayExactOverLongSequence):
	// a 1-unit tail of the day-6 lot plus all 5 units of the day-9 lot. Exercise
	// release sizes that stay inside the first lot, cross the boundary with a
	// clean fraction, cross it with an awkward fraction, and drain everything.
	for _, qty := range []string{"1", "3", "4.5", "6"} {
		wantCost, err := portfolio.ReleasedCost(ops, sber, d(qty))
		if err != nil {
			t.Fatalf("ReleasedCost(%s): %v", qty, err)
		}
		pieces, err := portfolio.ReleasedLots(ops, sber, d(qty))
		if err != nil {
			t.Fatalf("ReleasedLots(%s): %v", qty, err)
		}
		var gotCost int64
		gotQty := decimal.Zero
		for _, l := range pieces {
			gotCost += l.CostMinor
			gotQty = gotQty.Add(l.Quantity)
		}
		if gotCost != wantCost {
			t.Errorf("qty %s: sum of piece costs = %d, want %d (ReleasedCost)", qty, gotCost, wantCost)
		}
		if !gotQty.Equal(d(qty)) {
			t.Errorf("qty %s: sum of piece quantities = %s, want %s", qty, gotQty, qty)
		}
	}
}

// TestReleasedLotsOversellRejected pins that ReleasedLots fails exactly like
// the plain-cost ReleasedCost/releaseFIFO path when asked to release more
// than is held.
func TestReleasedLotsOversellRejected(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 2, &sber, "10", "100", -100_000, 10),
	}
	_, err := portfolio.ReleasedLots(ops, sber, d("11"))
	if !errors.Is(err, portfolio.ErrOversell) {
		t.Fatalf("err = %v, want ErrOversell", err)
	}
}

func TestBadOperations(t *testing.T) {
	for name, bad := range map[string]portfolio.Operation{
		"buy without qty":      op(portfolio.TypeBuy, 1, &sber, "", "100", -1000, 0),
		"buy negative qty":     op(portfolio.TypeBuy, 1, &sber, "-1", "100", -1000, 0),
		"sell without inst":    op(portfolio.TypeSell, 1, nil, "1", "100", 1000, 0),
		"buy positive amount":  op(portfolio.TypeBuy, 1, &sber, "1", "100", 1000, 0),
		"sell negative amount": op(portfolio.TypeSell, 1, &sber, "1", "100", -1000, 0),
	} {
		if _, err := portfolio.Compute([]portfolio.Operation{bad}); !errors.Is(err, portfolio.ErrBadOperation) {
			t.Errorf("%s: err = %v, want ErrBadOperation", name, err)
		}
	}
}

// TestSplitKeepsQuantitiesTheJournalCanRecord is the root fix of the whole
// "sell everything and break the account forever" family, at the level where
// the unrecordable number was born.
//
// A split is the only thing the engine does that can produce a quantity the
// journal cannot hold: it multiplies. 0.35 shares by a 1:3 reverse split
// (0.3333333333, the natural way anyone records one) is 0.116666666655 —
// eleven decimal places for a lot that arrived with two, in a ledger that keeps
// ten. A position holding that number is a position nobody can close: "sell all
// of it" names a quantity the sell row cannot store, so what is checked and
// what is written are two different quantities, and the write path rounds to
// NEAREST, which is up here.
//
// Every quantity below must therefore be expressible in the journal, and none
// of them may exceed the exact product — a ledger may lose a ten-billionth of a
// share to arithmetic it cannot express, but must never invent one.
func TestSplitKeepsQuantitiesTheJournalCanRecord(t *testing.T) {
	split := op(portfolio.TypeSplit, 3, &sber, "", "", 0, 0)
	split.SplitRatio = dp("0.3333333333")
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "0.35", "100", -3_500, 0),
		op(portfolio.TypeBuy, 2, &sber, "0.35", "200", -7_000, 0),
		split,
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]

	// 0.7 × 0.3333333333 = 0.23333333331 exactly; the journal can hold
	// 0.2333333333 of that and the last ten-billionth of a share is lost, not
	// rounded up into existence.
	if want := d("0.2333333333"); !p.Quantity.Equal(want) {
		t.Errorf("position quantity = %s, want %s", p.Quantity, want)
	}
	if exact := d("0.7").Mul(d("0.3333333333")); p.Quantity.GreaterThan(exact) {
		t.Errorf("position quantity = %s, more than the exact product %s — shares were invented", p.Quantity, exact)
	}
	if p.Quantity.Exponent() < -portfolio.QuantityScale {
		t.Errorf("position quantity = %s, finer than the %d decimal places the journal records — this is the number that cannot be sold",
			p.Quantity, portfolio.QuantityScale)
	}
	for i, l := range p.Lots {
		if l.Quantity.Exponent() < -portfolio.QuantityScale {
			t.Errorf("lot %d quantity = %s, finer than the %d decimal places the journal records",
				i, l.Quantity, portfolio.QuantityScale)
		}
	}
	// The lost ten-billionth comes off ONE lot, not each of them: the running
	// total is what gets truncated, so the pieces still add up to the position
	// exactly rather than approximately (checkLotInvariants), and the split
	// does not silently re-date anything.
	want := []portfolio.Lot{
		{Quantity: d("0.1166666666"), CostMinor: 3_500, AcquiredOn: dayp(1)},
		{Quantity: d("0.1166666667"), CostMinor: 7_000, AcquiredOn: dayp(2)},
	}
	if len(p.Lots) != len(want) {
		t.Fatalf("lots = %+v, want %d", p.Lots, len(want))
	}
	for i, w := range want {
		if !p.Lots[i].Quantity.Equal(w.Quantity) || p.Lots[i].CostMinor != w.CostMinor ||
			!sameAcquisition(p.Lots[i].AcquiredOn, w.AcquiredOn) {
			t.Errorf("lot %d = %s/%d/%s, want %s/%d/%s", i,
				p.Lots[i].Quantity, p.Lots[i].CostMinor, acquired(p.Lots[i].AcquiredOn),
				w.Quantity, w.CostMinor, acquired(w.AcquiredOn))
		}
	}
	checkLotInvariants(t, p)

	// And the whole position can now be released in one entry the journal can
	// actually record — the thing that was impossible before.
	sold := make([]portfolio.Operation, 0, len(ops)+1)
	sold = append(sold, ops...)
	sold = append(sold, op(portfolio.TypeSell, 4, &sber, p.Quantity.String(), "", 10_000, 0))
	after, err := portfolio.Compute(sold)
	if err != nil {
		t.Fatalf("selling the whole position: %v", err)
	}
	if !after[sber].Quantity.IsZero() {
		t.Errorf("quantity after selling everything = %s, want 0 — no unsellable dust may be left",
			after[sber].Quantity)
	}
}

// TestSplitThatRoundsALotAwayKeepsItsCost pins the edge the rule above creates:
// a reverse split deep enough that a lot's entire holding rounds away.
//
// The shares are gone — that is what the ledger can express and no rounding
// rule can conjure them back — but the money spent on them is not, and neither
// is the day it was spent, which is what values it in another currency. The lot
// stays, holding no shares and all of its cost, rather than having that cost
// swept onto some other lot's date.
func TestSplitThatRoundsALotAwayKeepsItsCost(t *testing.T) {
	split := op(portfolio.TypeSplit, 2, &sber, "", "", 0, 0)
	split.SplitRatio = dp("0.0000000001")
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "0.4", "10", -400, 0),
		split,
		op(portfolio.TypeBuy, 3, &sber, "5", "100", -50_000, 0),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	if want := d("5"); !p.Quantity.Equal(want) {
		t.Errorf("position quantity = %s, want %s (0.4 × 1e-10 is below anything the journal can name)", p.Quantity, want)
	}
	if p.CostMinor != 50_400 {
		t.Errorf("position cost = %d, want 50400 — a split is not a disposal, so no money may go missing", p.CostMinor)
	}
	if len(p.Lots) != 2 {
		t.Fatalf("lots = %+v, want 2 (the shareless one still holds its 400)", p.Lots)
	}
	if !p.Lots[0].Quantity.IsZero() || p.Lots[0].CostMinor != 400 || !sameAcquisition(p.Lots[0].AcquiredOn, dayp(1)) {
		t.Errorf("shareless lot = %s/%d/%s, want 0/400/%s", p.Lots[0].Quantity, p.Lots[0].CostMinor,
			acquired(p.Lots[0].AcquiredOn), day(1).Format("2006-01-02"))
	}
	checkLotInvariants(t, p)
}
