package portfolio_test

import (
	"errors"
	"fmt"
	"reflect"
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
	if realizedOf(t, p) != wantRealized {
		t.Errorf("realized = %d, want %d", realizedOf(t, p), wantRealized)
	}
	// remaining cost = full cost of both lots − released (not a cent of drift)
	if p.CostMinor != (100_010+110_011)-wantReleased {
		t.Errorf("cost = %d", p.CostMinor)
	}
	if p.FeesMinorIn(p.Currency) != 10+11+18 {
		t.Errorf("fees = %d", p.FeesMinorIn(p.Currency))
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
	if realizedOf(t, p) != 0 {
		t.Errorf("realized = %d, want 0", realizedOf(t, p))
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
	if realizedOf(t, p) != 12_000-10_001 {
		t.Errorf("realized = %d, want %d", realizedOf(t, p), 12_000-10_001)
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
	if got := pos[sber].IncomeMinorIn("RUB"); got != 3_480-452 {
		t.Errorf("income in RUB = %d", got)
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
	if pos[ofz].CostMinor != 0 || realizedOf(t, pos[ofz]) != 100_000 {
		t.Errorf("cost=%d realized=%d, want 0/100000", pos[ofz].CostMinor, realizedOf(t, pos[ofz]))
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
	if p == nil || !p.Quantity.IsZero() || realizedOf(t, p) != 250_000 {
		t.Fatalf("closed position = %+v", p)
	}
}

// TestCurrencyMismatchRejected pins the per-position currency invariant: an
// entry that puts money into a figure holding ONE currency has to repeat that
// currency, because mixing another one in would sum unrelated minor units into
// a single int64.
//
// THE EXEMPTIONS ARE ELSEWHERE ON PURPOSE and are as much a rule as these
// refusals — income (engine_income_currency_test.go), a commission, a sale, and
// any entry that moves no money at all (engine_position_currency_test.go, which
// carries the whole enum). What belongs in THIS table is every entry that does
// reach such a figure, so an exemption widened by one case fails here.
func TestCurrencyMismatchRejected(t *testing.T) {
	inCurrency := func(o portfolio.Operation, cur string) portfolio.Operation {
		o.Currency = cur
		return o
	}
	buyRUB := op(portfolio.TypeBuy, 1, &sber, "10", "100", -100_000, 0)

	for name, bad := range map[string]portfolio.Operation{
		"buy in another currency":          inCurrency(op(portfolio.TypeBuy, 4, &sber, "1", "100", -10_000, 0), "USD"),
		"transfer_in another currency":     inCurrency(op(portfolio.TypeTransferIn, 5, &sber, "1", "", 10_000, 0), "USD"),
		"transfer_out in another currency": inCurrency(op(portfolio.TypeTransferOut, 5, &sber, "1", "", 5_000, 0), "USD"),
		"amortization in another currency": inCurrency(op(portfolio.TypeAmortization, 5, &sber, "", "", 1_000, 0), "USD"),
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
	releasedMinor := proceedsMinor - sellFeesMinor - realizedOf(t, p)
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
		if !sameAcquisition(p.Lots[i].AcquiredOn, dayp(dayN)) {
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

// TestUndatedLotBehavesLikeAnyOtherLot pins that "no date" costs the lot
// nothing but the date: it is a full member of the FIFO queue, released in its
// turn, a partial release splits its cost the usual way and leaves the
// remainder still undated, and a split rescales it. A representation that
// treated the absence as a lesser kind of lot — dropped, merged into a
// neighbour, held back from a release — would change what the position is
// worth, which no missing date should ever do.
//
// Its PLACE in the queue is the one thing an unknown date does decide, and it
// decides it in the direction this fixture already had: the undated lot leads
// (see TestUndatedLotLeavesTheQueueFirst). Here it entered first as well, so
// nothing below distinguishes the two reasons — that is deliberate, this test
// is about the arithmetic and not about the ordering.
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
	if realizedOf(t, p) != 4_000-3_333 {
		t.Errorf("realized = %d, want %d — the sale must consume the undated lot at ITS cost, not the buy's",
			realizedOf(t, p), 4_000-3_333)
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

// TestTransferredLotBoughtEarlierIsSoldFirst is the case this change exists
// for, and the one the old engine got wrong.
//
// The queue is built from the day each lot was ACQUIRED, not from the order the
// journal happened to mention it. Every jurisdiction the owner files in says so
// in the same words: НК РФ ст. 214.1 п. 13 releases "по стоимости первых по
// времени ПРИОБРЕТЕНИЙ" — the word "зачисление" does not appear in the norm —
// and 26 CFR 1.1012-1(c)(1)(i) names "the earliest lot the taxpayer purchased
// or acquired". Moving shares between one's own accounts is nowhere a sale and
// nowhere resets that day.
//
// Here the account holds 10 shares bought on day 20 for $3 000,00 when a
// transfer arrives on day 25 carrying 10 shares bought on day 2 for $1 000,00 —
// older than anything on this account. Ten shares are then sold for $4 000,00.
//
//	arrival order (what the engine used to do): the day-20 lot is released
//	  realized 400 000 − 300 000 = 100 000; what stays is the day-2 parcel, 100 000
//	acquisition order (the law): the day-2 parcel is released
//	  realized 400 000 − 100 000 = 300 000; what stays is the day-20 lot, 300 000
//
// Three times the realized profit and three times the remaining basis, and the
// remainder is now dated on a different day — which is what picks the fx rate
// that turns it into rubles (see Handler.positionInBase). At 60,00 ₽/$ on day 2
// and 90,00 ₽/$ on day 20 the ruble basis left on the books moves from
// 60 000,00 ₽ to 270 000,00 ₽, four and a half times, on the same shares and
// the same money.
func TestTransferredLotBoughtEarlierIsSoldFirst(t *testing.T) {
	inUSD := func(o portfolio.Operation) portfolio.Operation {
		o.Currency = "USD"
		return o
	}
	ops := []portfolio.Operation{
		inUSD(op(portfolio.TypeBuy, 20, &sber, "10", "300", -300_000, 0)),
		inUSD(transferIn(25, "10", 100_000, piece("10", 100_000, 2))),
		inUSD(op(portfolio.TypeSell, 26, &sber, "10", "400", 400_000, 0)),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]

	if realizedOf(t, p) == 400_000-300_000 {
		t.Fatalf("realized = %d — the day-%d purchase was released because the journal mentions it first; the parcel bought on day %d is the earlier ACQUISITION and the queue is built from that",
			realizedOf(t, p), 20, 2)
	}
	if realizedOf(t, p) != 400_000-100_000 {
		t.Errorf("realized = %d, want %d (400000 − the day-2 parcel's cost 100000)",
			realizedOf(t, p), 400_000-100_000)
	}
	if !p.Quantity.Equal(d("10")) {
		t.Fatalf("qty = %s, want 10", p.Quantity)
	}
	if len(p.Lots) != 1 {
		t.Fatalf("lots = %+v, want 1 (the day-20 purchase, which nothing older was ahead of)", p.Lots)
	}
	left := p.Lots[0]
	if !sameAcquisition(left.AcquiredOn, dayp(20)) || left.CostMinor != 300_000 {
		t.Fatalf("remaining lot = {cost %d on %s}, want {300000 on %s}: the transferred parcel was the older one and has gone",
			left.CostMinor, acquired(left.AcquiredOn), day(20).Format("2006-01-02"))
	}
	checkLotInvariants(t, p)

	// And the remainder in rubles, which is the number that reaches a tax
	// return. The engine knows nothing about rates — it only says which lot is
	// left and when it was bought, and that day is what selects the rate. The
	// table below is the arithmetic Handler.positionInBase performs:
	//   before: $1 000,00 (100000) × 60,00 =  60 000,00 ₽ (6000000)
	//   now:    $3 000,00 (300000) × 90,00 = 270 000,00 ₽ (27000000)
	rubPerUSD := map[int]decimal.Decimal{2: d("60"), 20: d("90")}
	rate, ok := rubPerUSD[left.AcquiredOn.Day()]
	if !ok {
		t.Fatalf("remaining lot dated %s, which is neither of the two days this fixture uses", acquired(left.AcquiredOn))
	}
	inRubles := decimal.NewFromInt(left.CostMinor).Mul(rate)
	if inRubles.Equal(d("6000000")) {
		t.Fatalf("ruble basis left on the books = %s — the day-2 parcel at the day-2 rate; that parcel was sold", inRubles)
	}
	if want := d("27000000"); !inRubles.Equal(want) {
		t.Errorf("ruble basis left on the books = %s, want %s", inRubles, want)
	}
}

// TestAmortizationDrainsTheOlderTransferredLotFirst pins that amortization
// follows the queue's ACQUISITION order exactly like releaseFIFO does — a gap
// left by the change that reordered the queue (see addLot). drainLotsCost
// reduces cost basis front-to-back, and whichever lot loses that cost is the
// one later multiplied by ITS OWN historical fx rate when a position is
// valued in rubles (see Handler.positionInBase): draining the wrong lot does
// not round a number, it moves the whole amortized amount onto the wrong
// day's rate.
//
// All three existing amortization tests — TestAmortizationReducesCost,
// TestLotInvariantsUnderSplitAndAmortization, and the split/amortization case
// inside TestUndatedLotBehavesLikeAnyOtherLot — build their fixtures out of
// purchases in chronological order, where arrival order and acquisition order
// agree, so none of them can tell the two rules apart. This one can: the
// account holds a purchase from day 20 ($3 000,00) when a transfer on day 25
// brings shares bought on day 2 ($1 000,00) — older than anything already on
// the account — and an amortization of $400,00 then lands.
//
//	arrival order (what addLot used to do): the day-20 lot is drained,
//	  because the journal mentions the buy before the transfer
//	acquisition order (the rule this queue now keeps): the day-2 lot is
//	  drained, because it is the older ACQUISITION
//
// At 60,00 ₽/$ on day 2 and 90,00 ₽/$ on day 20, valuing what is left after
// the correct drain gives 600,00×60 + 3 000,00×90 = 306 000,00 ₽; draining the
// day-20 lot instead would give 2 600,00×90 + 1 000,00×60 = 294 000,00 ₽ — the
// $400,00 amortization landing on the wrong day's rate is a 12 000,00 ₽
// difference on the books, not a rounding error.
func TestAmortizationDrainsTheOlderTransferredLotFirst(t *testing.T) {
	inUSD := func(o portfolio.Operation) portfolio.Operation {
		o.Currency = "USD"
		return o
	}
	ops := []portfolio.Operation{
		inUSD(op(portfolio.TypeBuy, 20, &sber, "10", "300", -300_000, 0)),
		inUSD(transferIn(25, "10", 100_000, piece("10", 100_000, 2))),
		inUSD(op(portfolio.TypeAmortization, 30, &sber, "", "", 40_000, 0)),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	if len(p.Lots) != 2 {
		t.Fatalf("lots = %+v, want 2", p.Lots)
	}
	if p.Lots[0].CostMinor == 260_000 {
		t.Fatalf("lot 0 cost = %d — the day-20 lot was drained because the journal mentions its purchase before the transfer; amortization must follow the ACQUISITION order like releaseFIFO does",
			p.Lots[0].CostMinor)
	}
	if !sameAcquisition(p.Lots[0].AcquiredOn, dayp(2)) || p.Lots[0].CostMinor != 60_000 {
		t.Errorf("lot 0 = {cost %d on %s}, want {60000 on %s} — the older, transferred parcel is drained first",
			p.Lots[0].CostMinor, acquired(p.Lots[0].AcquiredOn), day(2).Format("2006-01-02"))
	}
	if !sameAcquisition(p.Lots[1].AcquiredOn, dayp(20)) || p.Lots[1].CostMinor != 300_000 {
		t.Errorf("lot 1 = {cost %d on %s}, want {300000 on %s} — untouched, the amortization never reached it",
			p.Lots[1].CostMinor, acquired(p.Lots[1].AcquiredOn), day(20).Format("2006-01-02"))
	}
	if p.CostMinor != 360_000 {
		t.Errorf("cost = %d, want 360000 (400000 total − 40000 amortized)", p.CostMinor)
	}
	checkLotInvariants(t, p)

	// The ruble base, which is the number Handler.positionInBase publishes:
	// each lot valued at its own day's rate.
	rubPerUSD := map[int]decimal.Decimal{2: d("60"), 20: d("90")}
	inRubles := decimal.Zero
	for _, l := range p.Lots {
		rate, ok := rubPerUSD[l.AcquiredOn.Day()]
		if !ok {
			t.Fatalf("lot dated %s, which is neither of the two days this fixture uses", acquired(l.AcquiredOn))
		}
		inRubles = inRubles.Add(decimal.NewFromInt(l.CostMinor).Mul(rate))
	}
	if inRubles.Equal(d("29400000")) {
		t.Fatalf("ruble base = %s — that is what draining the day-20 lot gives; the day-2 lot was supposed to be drained instead", inRubles)
	}
	if want := d("30600000"); !inRubles.Equal(want) {
		t.Errorf("ruble base = %s, want %s", inRubles, want)
	}
}

// TestAmortizationSkipsAnEmptyUndatedLot pins the safeguard in drainLotsCost
// that treats a lot with nothing left to give (CostMinor == 0) as producing NO
// piece at all — see drainLotsCost's own doc for why an empty piece would
// misreport a lot as having taken part in an event it took no part in.
//
// The safeguard is not cosmetic. A zero-basis parcel that arrived by transfer
// with no breakdown is undated (see TestUndatedLotLeavesTheQueueFirst) AND
// sits at the very head of the queue, ahead of every dated lot. Without the
// guard, an amortization that has to walk past such a lot to reach real cost
// would emit an empty piece carrying that lot's own (missing) acquisition
// date — and realizedTerms (see http.go) bails out the instant ANY released
// piece has no date, even though an expense of exactly zero needs no fx rate
// to be valued at all. The position's ruble realized figure, and the account
// total riding on it, would go silently and permanently null — not because
// anything is actually unknown, but because an empty piece pretended to be
// one.
func TestAmortizationSkipsAnEmptyUndatedLot(t *testing.T) {
	ops := []portfolio.Operation{
		// Zero-basis parcel, no breakdown: undated, gives up nothing, and
		// stands ahead of the dated purchase below.
		op(portfolio.TypeTransferIn, 1, &sber, "5", "", 0, 0),
		// A real, dated purchase.
		op(portfolio.TypeBuy, 20, &sber, "10", "300", -300_000, 0),
		// Amortization that has to walk past the empty lot to find real cost.
		op(portfolio.TypeAmortization, 30, &sber, "", "", 40_000, 0),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	if len(p.Realizations) != 1 {
		t.Fatalf("realizations = %d, want 1", len(p.Realizations))
	}
	released := p.Realizations[0].Released
	if len(released) != 1 {
		t.Fatalf("released pieces = %+v, want exactly 1 — the empty undated lot must not appear in the amortization's breakdown", released)
	}
	r := released[0]
	if r.AcquiredOn == nil {
		t.Fatalf("released piece has no acquisition date — the empty undated lot leaked into the breakdown, which is exactly what silences the ruble realized figure (see realizedTerms in http.go)")
	}
	if !sameAcquisition(r.AcquiredOn, dayp(20)) || r.CostMinor != 40_000 {
		t.Errorf("released piece = {cost %d on %s}, want {cost 40000 on %s}",
			r.CostMinor, acquired(r.AcquiredOn), day(20).Format("2006-01-02"))
	}
	if len(p.Lots) != 2 {
		t.Fatalf("lots = %+v, want 2", p.Lots)
	}
	if p.Lots[0].CostMinor != 0 || p.Lots[0].AcquiredOn != nil {
		t.Errorf("undated lot = %+v, want untouched at {cost 0, no date}", p.Lots[0])
	}
	checkLotInvariants(t, p)
}

// TestUndatedLotLeavesTheQueueFirst pins the second half of the ordering rule:
// a lot that does not know when it was acquired goes out BEFORE every dated
// one, however old the dated ones are.
//
// This is not an invention of ours. 26 CFR 1.6045A-1(b)(10) settles exactly
// this situation for transferred securities whose acquisition date did not come
// with them: such lots are treated as sold first, ahead of every lot with a
// known date. It is also the only answer that does not require making a date
// up: any other placement in the queue has to compare the unknown against real
// days, which means silently choosing a day for it.
//
// Here the account holds a purchase from day 1 — earlier than anything else in
// this file — and then receives a parcel with no breakdown at all, whose basis
// was typed in by hand and whose purchase dates were never recorded.
func TestUndatedLotLeavesTheQueueFirst(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "10", "10", -10_000, 0),
		op(portfolio.TypeTransferIn, 5, &sber, "10", "", 90_000, 0),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	if len(p.Lots) != 2 {
		t.Fatalf("lots = %+v, want 2", p.Lots)
	}
	if p.Lots[0].AcquiredOn != nil {
		t.Fatalf("head of the queue is dated %s; the lot with no date at all must stand ahead of every dated one, including a purchase from day %d",
			acquired(p.Lots[0].AcquiredOn), 1)
	}
	if p.Lots[0].CostMinor != 90_000 {
		t.Errorf("head lot cost = %d, want 90000 (the undated parcel)", p.Lots[0].CostMinor)
	}
	if !sameAcquisition(p.Lots[1].AcquiredOn, dayp(1)) || p.Lots[1].CostMinor != 10_000 {
		t.Errorf("second lot = {cost %d on %s}, want {10000 on %s}",
			p.Lots[1].CostMinor, acquired(p.Lots[1].AcquiredOn), day(1).Format("2006-01-02"))
	}
	checkLotInvariants(t, p)

	// The queue is not decoration: a sale of ten units takes the undated
	// parcel whole and leaves the old purchase untouched. Nine times the
	// realized profit separates the two answers.
	sold := append(append([]portfolio.Operation{}, ops...),
		op(portfolio.TypeSell, 6, &sber, "10", "", 100_000, 0))
	after, err := portfolio.Compute(sold)
	if err != nil {
		t.Fatalf("Compute after the sale: %v", err)
	}
	q := after[sber]
	if realizedOf(t, q) == 100_000-10_000 {
		t.Fatalf("realized = %d — the day-1 purchase was released; a lot whose acquisition is unknown leaves first", realizedOf(t, q))
	}
	if realizedOf(t, q) != 100_000-90_000 {
		t.Errorf("realized = %d, want %d (100000 − the undated parcel's 90000)",
			realizedOf(t, q), 100_000-90_000)
	}
	// A welcome consequence: selling drains the unknown out of the account
	// first, so what is left can be valued again (see Handler.positionInBase,
	// which publishes nothing at all while one lot has no date).
	if len(q.Lots) != 1 || !sameAcquisition(q.Lots[0].AcquiredOn, dayp(1)) {
		t.Errorf("lots after the sale = %+v, want only the day-%d purchase", q.Lots, 1)
	}
	checkLotInvariants(t, q)
}

// TestQueueOrderIsStableUnderTies pins the tie-break, which matters more than
// it looks: these numbers go into a tax return, so two runs must agree, and
// "two runs" includes two builds of the engine that resolve ties differently.
// The law names no rule finer than the day — НК РФ ст. 214.1 п. 13 and 26 CFR
// 1.1012-1(c)(1)(i) both stop at "first by time of acquisition" — so the
// tie-break is ours to choose, and the choice is the order the lots entered the
// account, which is the journal's own order: it is total, it is recorded rather
// than derived, and it is what the queue already did before dates entered the
// picture.
//
// The fixture is built to catch the failure that a three-lot example cannot.
// Two purchases open the account, on day 2 and day 4; then seven transfers
// arrive, each carrying three pieces at once — one with no date, one bought on
// day 2, one bought on day 4 — so lots keep being inserted into the MIDDLE of
// the queue, alternating between three groups. Sorting such an arrangement with
// Go's sort.Slice, which is not stable, visibly permutes the ties; a stable
// order does not. Every cost below is distinct, so any permutation is named in
// the failure.
func TestQueueOrderIsStableUnderTies(t *testing.T) {
	const parcels = 7
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 2, &sber, "1", "", -1_000, 0),
		op(portfolio.TypeBuy, 4, &sber, "1", "", -2_000, 0),
	}
	// wantUndated/wantEarly/wantLate accumulate the costs in the order the lots
	// entered the account; the queue must be the three lists concatenated.
	wantUndated := []int64{}
	wantEarly := []int64{1_000}
	wantLate := []int64{2_000}
	for i := 0; i < parcels; i++ {
		undated := int64(100_000 + i)
		early := int64(200_000 + i)
		late := int64(300_000 + i)
		ops = append(ops, transferIn(10+i, "3", undated+early+late,
			portfolio.ReleasedLot{Quantity: d("1"), CostMinor: undated},
			piece("1", early, 2),
			piece("1", late, 4)))
		wantUndated = append(wantUndated, undated)
		wantEarly = append(wantEarly, early)
		wantLate = append(wantLate, late)
	}

	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]

	type want struct {
		cost int64
		on   *time.Time
	}
	queue := make([]want, 0, len(p.Lots))
	for _, c := range wantUndated {
		queue = append(queue, want{c, nil})
	}
	for _, c := range wantEarly {
		queue = append(queue, want{c, dayp(2)})
	}
	for _, c := range wantLate {
		queue = append(queue, want{c, dayp(4)})
	}
	if len(p.Lots) != len(queue) {
		t.Fatalf("lots = %d, want %d", len(p.Lots), len(queue))
	}
	for i, w := range queue {
		got := p.Lots[i]
		if got.CostMinor != w.cost || !sameAcquisition(got.AcquiredOn, w.on) {
			t.Errorf("lot %d = {cost %d on %s}, want {cost %d on %s} — ties must keep the order the lots entered the account",
				i, got.CostMinor, acquired(got.AcquiredOn), w.cost, acquired(w.on))
		}
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

// TestSplitLeavesTheDateCostMapUnchangedOverAReorderedQueue pins, in code,
// what a reviewer confirmed by hand while reading the acquisition-ordering
// change: applySplit rewrites quantities only (see its doc comment), so which
// lot sits at which INDEX in the queue cannot move a single unit of cost from
// one acquisition date to another. The cost each date owns before a split is
// exactly what it owns after, no matter what reordered the queue meant the
// index-to-date mapping now was.
//
// The fixture is the reordering case this whole change exists for: a purchase
// from day 20 already on the account, then a transfer on day 25 carrying
// shares bought on day 2 — older, so it leads the queue (see addLot) instead
// of trailing behind the purchase the journal happened to mention first. A
// 1:2 reverse split then runs over that reordered queue.
func TestSplitLeavesTheDateCostMapUnchangedOverAReorderedQueue(t *testing.T) {
	split := op(portfolio.TypeSplit, 26, &sber, "", "", 0, 0)
	split.SplitRatio = dp("0.5")
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 20, &sber, "10", "300", -300_000, 0),
		transferIn(25, "10", 100_000, piece("10", 100_000, 2)),
		split,
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	if len(p.Lots) != 2 {
		t.Fatalf("lots = %+v, want 2", p.Lots)
	}
	// The queue itself is untouched by the split: the older, transferred
	// parcel still leads.
	if !sameAcquisition(p.Lots[0].AcquiredOn, dayp(2)) || !sameAcquisition(p.Lots[1].AcquiredOn, dayp(20)) {
		t.Fatalf("lots dated %s then %s, want %s then %s — a split must not reorder the queue",
			acquired(p.Lots[0].AcquiredOn), acquired(p.Lots[1].AcquiredOn), day(2).Format("2006-01-02"), day(20).Format("2006-01-02"))
	}
	// Quantities are halved by the 1:2 reverse split; costs are the split's
	// business to leave alone entirely.
	if !p.Lots[0].Quantity.Equal(d("5")) || !p.Lots[1].Quantity.Equal(d("5")) {
		t.Errorf("quantities after the split = %s and %s, want 5 and 5", p.Lots[0].Quantity, p.Lots[1].Quantity)
	}
	dateCost := map[string]int64{}
	for _, l := range p.Lots {
		dateCost[acquired(l.AcquiredOn)] = l.CostMinor
	}
	want := map[string]int64{day(2).Format("2006-01-02"): 100_000, day(20).Format("2006-01-02"): 300_000}
	for on, cost := range want {
		if dateCost[on] != cost {
			t.Errorf("cost dated %s = %d, want %d — a split must not move cost from one acquisition date to another",
				on, dateCost[on], cost)
		}
	}
	if p.CostMinor != 400_000 {
		t.Errorf("cost = %d, want 400000 — unchanged by the split", p.CostMinor)
	}
	checkLotInvariants(t, p)
}

// --- What each realized result was made of -------------------------------

// releasedText renders a realization's released pieces for a failure message,
// naming the unknown acquisition date rather than printing a stand-in for it
// (see acquired).
func releasedText(pieces []portfolio.ReleasedLot) string {
	parts := make([]string, 0, len(pieces))
	for _, pc := range pieces {
		parts = append(parts, fmt.Sprintf("%s units/%d minor acquired %s", pc.Quantity, pc.CostMinor, acquired(pc.AcquiredOn)))
	}
	return "[" + strings.Join(parts, "; ") + "]"
}

// checkReleased compares a realization's pieces against what they must be,
// reporting the whole breakdown on any mismatch so a failure says which piece
// moved rather than only that something did.
func checkReleased(t *testing.T, got, want []portfolio.ReleasedLot) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("released %d pieces %s, want %d %s", len(got), releasedText(got), len(want), releasedText(want))
	}
	for i := range want {
		if !got[i].Quantity.Equal(want[i].Quantity) || got[i].CostMinor != want[i].CostMinor ||
			!sameAcquisition(got[i].AcquiredOn, want[i].AcquiredOn) {
			t.Errorf("released piece %d = %s, want %s", i, releasedText(got[i:i+1]), releasedText(want[i:i+1]))
		}
	}
}

// checkRealizationsSumToTotal is the invariant every realization test ends
// with: the events must account for the running total to the last minor unit.
// A total larger than the events means something was realized without saying
// what it was made of — a figure the ruble layer cannot convert and cannot even
// see it is missing; a total smaller means an event was recorded twice or
// carries basis the position never gave up.
func checkRealizationsSumToTotal(t *testing.T, p *portfolio.Position) {
	t.Helper()
	var sum int64
	for _, r := range p.Realizations {
		sum += r.PnLMinor()
	}
	if sum != realizedOf(t, p) {
		t.Errorf("realizations sum to %d, but the position realized %d (off by %d) — every minor unit of the total must be accounted for by an event",
			sum, realizedOf(t, p), sum-realizedOf(t, p))
		for i, r := range p.Realizations {
			t.Logf("  event %d on %s: proceeds %d, fee %d, released %s → %d",
				i, r.OccurredOn.Format("2006-01-02"), r.ProceedsMinor, r.FeeMinor, releasedText(r.Released), r.PnLMinor())
		}
	}
}

// TestSaleRecordsWhatItWasMadeOf is the change this section exists for.
//
// A single accumulated number in the position's currency cannot be turned into
// rubles: the proceeds belong to the day of the sale and each parcel of basis
// belongs to the day THAT parcel was bought (НК РФ ст. 210 п. 5), so the fold
// has to keep the parts rather than only their difference. Here one sale
// crosses a lot boundary — 15 shares out of a lot of 10 bought on day 2 and a
// lot of 10 bought on day 3 — and the event must carry both pieces with their
// own dates, not one piece dated on the sale.
func TestSaleRecordsWhatItWasMadeOf(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 2, &sber, "10", "100", -100_000, 10),
		op(portfolio.TypeBuy, 3, &sber, "10", "110", -110_000, 11),
		op(portfolio.TypeSell, 4, &sber, "15", "120", 180_000, 18),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	if len(p.Realizations) != 1 {
		t.Fatalf("realizations = %d, want 1 (one sale, one event)", len(p.Realizations))
	}
	r := p.Realizations[0]
	if !r.OccurredOn.Equal(day(4)) {
		t.Errorf("event dated %s, want %s — the proceeds belong to the day of the sale",
			r.OccurredOn.Format("2006-01-02"), day(4).Format("2006-01-02"))
	}
	if r.ProceedsMinor != 180_000 || r.FeeMinor != 18 {
		t.Errorf("proceeds/fee = %d/%d, want 180000/18", r.ProceedsMinor, r.FeeMinor)
	}
	// The whole day-2 lot (100 000 + 10 fee capitalized) and half of the day-3
	// lot (floor(110 011 / 2) = 55 005), each keeping its own purchase day.
	checkReleased(t, r.Released, []portfolio.ReleasedLot{
		{Quantity: d("10"), CostMinor: 100_010, AcquiredOn: dayp(2)},
		{Quantity: d("5"), CostMinor: 55_005, AcquiredOn: dayp(3)},
	})
	if want := int64(180_000 - 18 - 155_015); r.PnLMinor() != want {
		t.Errorf("event result = %d, want %d", r.PnLMinor(), want)
	}
	checkRealizationsSumToTotal(t, p)
}

// TestEachDisposalGetsItsOwnRealization pins that the events are a series and
// not a running summary: three sales of the same lot on three days produce
// three records, in journal order, each carrying its own day and its own
// proceeds. Merging them would lose the dates the ruble conversion is struck
// at, which is the whole reason the breakdown is kept.
func TestEachDisposalGetsItsOwnRealization(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "9", "100", -90_000, 0),
		op(portfolio.TypeSell, 2, &sber, "3", "110", 33_000, 5),
		op(portfolio.TypeSell, 5, &sber, "3", "120", 36_000, 6),
		op(portfolio.TypeSell, 9, &sber, "3", "90", 27_000, 7),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	if len(p.Realizations) != 3 {
		t.Fatalf("realizations = %d, want 3 (three sales, three events)", len(p.Realizations))
	}
	wantDay := []int{2, 5, 9}
	wantProceeds := []int64{33_000, 36_000, 27_000}
	wantFee := []int64{5, 6, 7}
	for i, r := range p.Realizations {
		if !r.OccurredOn.Equal(day(wantDay[i])) {
			t.Errorf("event %d dated %s, want %s", i, r.OccurredOn.Format("2006-01-02"), day(wantDay[i]).Format("2006-01-02"))
		}
		if r.ProceedsMinor != wantProceeds[i] || r.FeeMinor != wantFee[i] {
			t.Errorf("event %d proceeds/fee = %d/%d, want %d/%d", i, r.ProceedsMinor, r.FeeMinor, wantProceeds[i], wantFee[i])
		}
		// Each third of a 9-share lot costing 90 000 releases exactly 30 000,
		// and every piece keeps the one day the shares were bought.
		checkReleased(t, r.Released, []portfolio.ReleasedLot{{Quantity: d("3"), CostMinor: 30_000, AcquiredOn: dayp(1)}})
	}
	checkRealizationsSumToTotal(t, p)
}

// TestTransferOutRecordsNoRealization pins the decision, rather than leaving it
// to a comment: the departing leg of a transfer produces NO event.
//
// Moving shares between the family's own accounts is a disposal in none of the
// seven jurisdictions this series was researched against, and the leg has no
// proceeds to record — its AmountMinor is the basis that travelled, not money
// received. It also adds nothing to RealizedPnL and never has, so leaving
// it out costs the events-sum-to-the-total invariant nothing.
//
// Both shapes are covered, because they take different branches: a transfer
// with a stored breakdown gives up the lots it recorded, one without gives up a
// fresh slice of the queue.
func TestTransferOutRecordsNoRealization(t *testing.T) {
	for name, ops := range map[string][]portfolio.Operation{
		"with a recorded breakdown": {
			op(portfolio.TypeBuy, 1, &sber, "10", "100", -100_000, 0),
			transferOut(5, "4", 40_000, piece("4", 40_000, 1)),
		},
		"without one": {
			op(portfolio.TypeBuy, 1, &sber, "10", "100", -100_000, 0),
			op(portfolio.TypeTransferOut, 5, &sber, "4", "", 40_000, 0),
		},
	} {
		t.Run(name, func(t *testing.T) {
			pos, err := portfolio.Compute(ops)
			if err != nil {
				t.Fatalf("Compute: %v", err)
			}
			p := pos[sber]
			if len(p.Realizations) != 0 {
				t.Errorf("realizations = %d %+v, want 0 — a transfer between one's own accounts realizes nothing",
					len(p.Realizations), p.Realizations)
			}
			if realizedOf(t, p) != 0 {
				t.Errorf("realized = %d, want 0", realizedOf(t, p))
			}
			checkRealizationsSumToTotal(t, p)
		})
	}
}

// TestAmortizationRecordsARealization covers the OTHER place the running total
// grows, and the reason it must produce an event even when it adds nothing to
// that total.
//
// A return of principal retires cost basis, and in the position's own currency
// only the excess over what is left of that basis is a result. In rubles the
// covered part is not neutral either: the principal comes back at the rate of
// the day it was paid, the basis it retires was struck at the rates of the days
// those lots were bought, and that difference is as much of a taxable result as
// any sale's. So the covered amortization below has a result of zero here and
// still has to say what it was made of.
func TestAmortizationRecordsARealization(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &ofz, "10", "950", -950_000, 0),
		op(portfolio.TypeAmortization, 10, &ofz, "", "", 250_000, 0),
		op(portfolio.TypeAmortization, 20, &ofz, "", "", 800_000, 0),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[ofz]
	if len(p.Realizations) != 2 {
		t.Fatalf("realizations = %d, want 2 (two amortizations, two events)", len(p.Realizations))
	}
	// Fully covered by the basis: nothing realized in this currency, and the
	// 250 000 retired belongs to the day the bond was bought. The pieces carry
	// NO quantity — an amortization returns principal without moving a share.
	first := p.Realizations[0]
	if !first.OccurredOn.Equal(day(10)) || first.ProceedsMinor != 250_000 || first.FeeMinor != 0 {
		t.Errorf("first event = %s/%d/%d, want %s/250000/0",
			first.OccurredOn.Format("2006-01-02"), first.ProceedsMinor, first.FeeMinor, day(10).Format("2006-01-02"))
	}
	checkReleased(t, first.Released, []portfolio.ReleasedLot{{Quantity: d("0"), CostMinor: 250_000, AcquiredOn: dayp(1)}})
	if first.PnLMinor() != 0 {
		t.Errorf("first event result = %d, want 0", first.PnLMinor())
	}
	// Beyond what the basis can cover: 700 000 left retires, the remaining
	// 100 000 is realized.
	second := p.Realizations[1]
	if !second.OccurredOn.Equal(day(20)) || second.ProceedsMinor != 800_000 {
		t.Errorf("second event = %s/%d, want %s/800000",
			second.OccurredOn.Format("2006-01-02"), second.ProceedsMinor, day(20).Format("2006-01-02"))
	}
	checkReleased(t, second.Released, []portfolio.ReleasedLot{{Quantity: d("0"), CostMinor: 700_000, AcquiredOn: dayp(1)}})
	if second.PnLMinor() != 100_000 {
		t.Errorf("second event result = %d, want 100000", second.PnLMinor())
	}
	if realizedOf(t, p) != 100_000 {
		t.Errorf("realized = %d, want 100000 — unchanged by recording what it was made of", realizedOf(t, p))
	}
	checkRealizationsSumToTotal(t, p)
}

// TestRealizationMayNotKnowWhenItsBasisWasAcquired pins that an event whose
// basis has no purchase date behind it is LEGITIMATE and recorded as it is.
//
// A transfer with no stored breakdown creates a lot that does not know when it
// was bought (see Lot.AcquiredOn), such a lot leads the release queue, and
// selling it produces an event whose expense side cannot be converted into
// rubles at all. That is a fact about the journal, not damage: the engine must
// carry the absence through instead of substituting the transfer day or the
// sale day, and what to publish for such an event is the caller's decision.
func TestRealizationMayNotKnowWhenItsBasisWasAcquired(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeTransferIn, 1, &sber, "10", "", 100_000, 0),
		op(portfolio.TypeBuy, 2, &sber, "10", "150", -150_000, 0),
		op(portfolio.TypeSell, 3, &sber, "15", "200", 300_000, 20),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	if len(p.Realizations) != 1 {
		t.Fatalf("realizations = %d, want 1", len(p.Realizations))
	}
	// The undated parcel leads the queue and goes whole; the rest comes out of
	// the day-2 purchase, floor(150 000 × 5 / 10) = 75 000.
	checkReleased(t, p.Realizations[0].Released, []portfolio.ReleasedLot{
		{Quantity: d("10"), CostMinor: 100_000, AcquiredOn: nil},
		{Quantity: d("5"), CostMinor: 75_000, AcquiredOn: dayp(2)},
	})
	if want := int64(300_000 - 20 - 175_000); p.Realizations[0].PnLMinor() != want {
		t.Errorf("event result = %d, want %d", p.Realizations[0].PnLMinor(), want)
	}
	checkRealizationsSumToTotal(t, p)
}

// TestRealizationsSumToRealizedPnL is the discriminating one.
//
// The invariant is not "the events roughly explain the total" but "the events
// ARE the total": a ruble figure is built by converting each event and adding
// them up, so a single minor unit realized outside an event is a unit that
// silently never reaches rubles, and one counted twice is money the family
// never made. It has to hold through everything the journal can do, not through
// sales alone — splits rewrite the quantities the pieces are proportioned from,
// transfers move lots in and out and reorder the queue by acquisition date, an
// undated parcel leads that queue, and amortization drains basis without moving
// a share.
//
// So the sequence below mixes all of them, with awkward, non-divisible amounts
// so that every floor division has a remainder to lose. The transfer out
// resolves its own breakdown from the journal exactly as the write path does
// (portfolio.ReleasedLots), rather than being written by hand into agreement.
//
// Both totals are pinned as well as their equality. Equality alone would
// survive a change that broke the events and the total the same way; the
// numbers below were derived by hand from the sequence:
//
//	day  3 sell:  12 345 − 30 011 −  11 =  −17 677
//	day  7 sell: 130 007 − 36 680 −  13 =   93 314
//	day  9 amrt:   5 000 −  5 000       =        0
//	day 11 sell:  60 001 − 46 119 −   9 =   13 873
//	day 13 sell:  40 000 − 25 000 −   7 =   14 993
//	day 14 amrt: 900 000 − 155 010      =  744 990
//	day 15 sell: 200 000 −      0 −   3 =  199 997
//	                                      ─────────
//	                                      1 049 490
//
// Seven events for five sales and two amortizations — and none for the
// transfers, which is the count that pins the decision that a transfer out
// realizes nothing.
func TestRealizationsSumToRealizedPnL(t *testing.T) {
	var ops []portfolio.Operation
	add := func(o portfolio.Operation) { ops = append(ops, o) }
	split := func(dayN int, ratio string) portfolio.Operation {
		o := op(portfolio.TypeSplit, dayN, &sber, "", "", 0, 0)
		o.SplitRatio = dp(ratio)
		return o
	}
	// moveOut is a departing leg whose breakdown is resolved against the
	// journal so far, the way the transfer service resolves it.
	moveOut := func(dayN int, qty string) portfolio.Operation {
		pieces, err := portfolio.ReleasedLots(ops, sber, d(qty))
		if err != nil {
			t.Fatalf("resolving the parcel leaving on day %d: %v", dayN, err)
		}
		return transferOut(dayN, qty, portfolio.LotsCost(pieces), pieces...)
	}

	add(op(portfolio.TypeBuy, 1, &sber, "10", "", -100_030, 7))
	add(op(portfolio.TypeBuy, 2, &sber, "7", "", -23_331, 3))
	add(op(portfolio.TypeSell, 3, &sber, "3", "", 12_345, 11))
	add(split(4, "3"))
	add(op(portfolio.TypeBuy, 5, &sber, "5", "", -77_777, 5))
	// A transfer in carrying shares older than some of what is already held, so
	// the queue is reordered by acquisition rather than by arrival.
	add(transferIn(6, "9", 90_009, piece("4", 40_004, 1), piece("5", 50_005, 4)))
	add(op(portfolio.TypeSell, 7, &sber, "11", "", 130_007, 13))
	add(moveOut(8, "7"))
	add(op(portfolio.TypeAmortization, 9, &sber, "", "", 5_000, 0))
	add(split(10, "0.5"))
	add(op(portfolio.TypeSell, 11, &sber, "4", "", 60_001, 9))
	// A transfer whose basis was typed in by hand: the lot it creates knows no
	// purchase date, leads the queue, and is sold into on day 13.
	add(op(portfolio.TypeTransferIn, 12, &sber, "6", "", 30_000, 0))
	add(op(portfolio.TypeSell, 13, &sber, "5", "", 40_000, 7))
	// More principal returned than the basis can cover: the excess is realized.
	add(op(portfolio.TypeAmortization, 14, &sber, "", "", 900_000, 0))
	// Everything that is left, now carrying no basis at all.
	add(op(portfolio.TypeSell, 15, &sber, "16", "", 200_000, 3))

	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	checkLotInvariants(t, p)
	checkRealizationsSumToTotal(t, p)
	if realizedOf(t, p) != 1_049_490 {
		t.Errorf("realized = %d, want 1049490 (see the derivation above)", realizedOf(t, p))
	}
	if len(p.Realizations) != 7 {
		t.Fatalf("realizations = %d, want 7 — five sales and two amortizations, and nothing for either transfer", len(p.Realizations))
	}
	// The mix really did carry a parcel whose purchase day nobody recorded all
	// the way into an event: without this the invariant above could be holding
	// over a sequence where the hard case never arose.
	var undated int
	for _, r := range p.Realizations {
		for _, pc := range r.Released {
			if pc.AcquiredOn == nil {
				undated++
			}
		}
	}
	if undated == 0 {
		t.Error("no released piece came out undated — the fixture no longer exercises basis with no known purchase date")
	}
}

// TestAmortizationOnAPaperNeverAcquiredIsRefused pins the refusal that closes
// the phantom amortization of issue #17.
//
// The mechanics of the fault are two lines of arithmetic: with nothing acquired
// there is no basis, the amortization retires min(amount, 0) = 0 of it, and the
// event's result is the WHOLE payment. So a single mistyped instrument turned
// 4 000,00 ₽ of somebody's coupon schedule into 4 000,00 ₽ of realized profit
// under a position with no shares, no cost and no purchase behind it — a figure
// that goes into a tax return, with nothing on any screen to tell it from a real
// one.
//
// The literal below is the number the OLD engine published, written out rather
// than derived: it is what the test is against, and computing it from the
// fixture would let the fixture and the claim move together.
func TestAmortizationOnAPaperNeverAcquiredIsRefused(t *testing.T) {
	_, err := portfolio.Compute([]portfolio.Operation{
		op(portfolio.TypeAmortization, 10, &ofz, "", "", 400_000, 0),
	})
	if !errors.Is(err, portfolio.ErrBadOperation) {
		t.Fatalf("Compute = %v, want ErrBadOperation — 400000 minor would otherwise be realized profit on a paper nobody bought", err)
	}
	if !strings.Contains(err.Error(), "never acquired") {
		t.Errorf("refusal = %q, want it to say the paper was never acquired", err)
	}
}

// TestAmortizationIsAllowedWherePrincipalCanCome checks the three shapes the
// refusal above must NOT reach, each of which is a journal this program itself
// writes. A guard that refuses those is worse than the phantom it prevents: it
// takes the whole account's positions screen down for healthy data.
func TestAmortizationIsAllowedWherePrincipalCanCome(t *testing.T) {
	// Same-day maturity: the redemption empties the position (quantity 0, cost
	// 0) and the final partial repayment is folded AFTER it, because within one
	// date the journal folds by the order it was recorded in. This is the shape
	// the importer produces from what a broker sends on a maturity date.
	redeemedThenRepaid := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &ofz, "10", "950", -950_000, 0),
		op(portfolio.TypeSell, 10, &ofz, "10", "1000", 1_000_000, 0),
		op(portfolio.TypeAmortization, 10, &ofz, "", "", 30_000, 0),
	}
	// A bond still held whose basis earlier amortizations have used up: further
	// principal really is pure gain, and that is deliberately supported.
	basisSpent := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &ofz, "10", "950", -950_000, 0),
		op(portfolio.TypeAmortization, 5, &ofz, "", "", 950_000, 0),
		op(portfolio.TypeAmortization, 6, &ofz, "", "", 40_000, 0),
	}
	// A parcel that arrived by transfer is an acquisition too — the account did
	// not buy it here, but a lot of it exists and the principal has somewhere to
	// come from.
	arrivedByTransfer := []portfolio.Operation{
		{
			Type: portfolio.TypeTransferIn, OccurredOn: day(1), InstrumentID: &ofz,
			Currency: "RUB", Quantity: dp("10"), AmountMinor: 950_000,
		},
		// Past the 950 000 the parcel carried, so the excess is realized and the
		// figure below cannot come out right by the operation being ignored.
		op(portfolio.TypeAmortization, 5, &ofz, "", "", 990_000, 0),
	}
	for name, tc := range map[string]struct {
		ops          []portfolio.Operation
		wantRealized int64
	}{
		// 50 000 from the sale, then the repayment is realized whole: the basis
		// left after the redemption is nothing, which is the truth here.
		"repayment folded after the same-day redemption": {redeemedThenRepaid, 50_000 + 30_000},
		"basis already fully amortized":                  {basisSpent, 40_000},
		"parcel acquired by transfer":                    {arrivedByTransfer, 40_000},
	} {
		t.Run(name, func(t *testing.T) {
			pos, err := portfolio.Compute(tc.ops)
			if err != nil {
				t.Fatalf("Compute: %v", err)
			}
			if realizedOf(t, pos[ofz]) != tc.wantRealized {
				t.Errorf("realized = %d, want %d", realizedOf(t, pos[ofz]), tc.wantRealized)
			}
		})
	}
}

// TestPaymentsOnAPaperNeverAcquiredStayLegitimate is the other half of the
// boundary: an amortization is the ONE payment the engine turns into a claim
// about cost, and the refusal must not spread to the payments that make no such
// claim. A dividend, a coupon and the tax withheld from either are booked as
// income in the currency they arrived in and say nothing about what the paper
// cost — a journal that opens with payments alone is ordinary (the paper was
// bought before the import window, or arrived by a transfer nobody recorded).
func TestPaymentsOnAPaperNeverAcquiredStayLegitimate(t *testing.T) {
	pos, err := portfolio.Compute([]portfolio.Operation{
		op(portfolio.TypeCoupon, 10, &ofz, "", "", 12_000, 0),
		op(portfolio.TypeDividend, 11, &ofz, "", "", 3_000, 0),
		op(portfolio.TypeTax, 12, &ofz, "", "", -1_500, 0),
	})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[ofz]
	if p.IncomeMinorIn("RUB") != 12_000+3_000-1_500 {
		t.Errorf("income = %d, want 13500", p.IncomeMinorIn("RUB"))
	}
	if realizedOf(t, p) != 0 {
		t.Errorf("realized = %d, want 0 — payments claim nothing about cost", realizedOf(t, p))
	}
}

// realizedOf is a position's realized result, and it FAILS THE TEST when the
// position has none — a disposal that settled in another currency leaves no
// figure in any single one (see portfolio.Position.RealizedPnL). Every call
// below therefore asserts two things at once: the number, and that there is a
// number, which is what keeps a test from quietly comparing a zero against a
// zero the moment the currency rule starts refusing to answer.
func realizedOf(t *testing.T, p *portfolio.Position) int64 {
	t.Helper()
	minor, inOneCurrency := p.RealizedPnL()
	if !inOneCurrency {
		t.Fatalf("position %s has no realized result in one currency: a disposal settled in another", p.InstrumentID)
	}
	return minor
}

// TestRedemptionComputesExactlyAsASale is the load-bearing test of the new
// type, and it is a DIFFERENTIAL one on purpose: a redemption differs from a
// sale in what happened, not in what it comes to, so the only way the change
// can go wrong is by the two drifting apart.
//
// Every figure is compared against the same journal with the disposal recorded
// as a sale — cost, quantity, realized result, the released parcels and their
// dates — rather than against literals of its own. Literals would pin today's
// arithmetic; this pins the identity, which is the actual claim.
func TestRedemptionComputesExactlyAsASale(t *testing.T) {
	journal := func(disposal portfolio.Type) []portfolio.Operation {
		return []portfolio.Operation{
			op(portfolio.TypeBuy, 1, &sber, "10", "100", -100_000, 50),
			op(portfolio.TypeBuy, 2, &sber, "10", "110", -110_000, 55),
			op(disposal, 9, &sber, "15", "120", 180_000, 90),
		}
	}

	asSale, err := portfolio.Compute(journal(portfolio.TypeSell))
	if err != nil {
		t.Fatalf("Compute as a sale: %v", err)
	}
	asRedemption, err := portfolio.Compute(journal(portfolio.TypeRedemption))
	if err != nil {
		t.Fatalf("Compute as a redemption: %v", err)
	}

	sale, redeemed := asSale[sber], asRedemption[sber]
	if !sale.Quantity.Equal(redeemed.Quantity) {
		t.Errorf("quantity: sale %s, redemption %s", sale.Quantity, redeemed.Quantity)
	}
	if sale.CostMinor != redeemed.CostMinor {
		t.Errorf("cost: sale %d, redemption %d", sale.CostMinor, redeemed.CostMinor)
	}
	saleRealized, saleOK := sale.RealizedPnL()
	redeemedRealized, redeemedOK := redeemed.RealizedPnL()
	if saleOK != redeemedOK || saleRealized != redeemedRealized {
		t.Errorf("realized: sale %d/%v, redemption %d/%v", saleRealized, saleOK, redeemedRealized, redeemedOK)
	}
	if len(sale.Realizations) != 1 || len(redeemed.Realizations) != 1 {
		t.Fatalf("realizations: sale %d, redemption %d", len(sale.Realizations), len(redeemed.Realizations))
	}
	// The parcels the queue gave up, piece by piece with their purchase days:
	// this is what a ruble result is struck from, so a difference here would
	// move money even where the totals above happened to agree.
	if !reflect.DeepEqual(sale.Realizations[0], redeemed.Realizations[0]) {
		t.Errorf("the disposal itself differs:\n sale       %+v\n redemption %+v",
			sale.Realizations[0], redeemed.Realizations[0])
	}
	// And a positive result really was produced, so the comparison above is not
	// two empty answers agreeing.
	if !saleOK || saleRealized == 0 {
		t.Fatalf("the fixture realized nothing (%d, %v): the equality above would be vacuous", saleRealized, saleOK)
	}
}

// TestRedemptionRefusesWhatASaleRefuses. The refusals are part of the identity
// too, and the message has to name the type the owner wrote rather than the one
// the branch is shared with.
func TestRedemptionRefusesWhatASaleRefuses(t *testing.T) {
	_, err := portfolio.Compute([]portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "10", "100", -100_000, 0),
		op(portfolio.TypeRedemption, 9, &sber, "10", "120", -1, 0),
	})
	if !errors.Is(err, portfolio.ErrBadOperation) {
		t.Fatalf("err = %v, want ErrBadOperation: money must come IN on a redemption", err)
	}
	if !strings.Contains(err.Error(), "redemption") {
		t.Errorf("error %q does not name the type the journal actually holds", err)
	}
}
