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
	"babki.my/babki/internal/portfolio/portfoliotest"
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

// dayp is day as a known acquisition date.
func dayp(n int) *time.Time {
	t := day(n)
	return &t
}

// acquired renders an acquisition date for a failure message, naming the
// unknown case.
func acquired(t *time.Time) string {
	if t == nil {
		return "unknown"
	}
	return t.Format("2006-01-02")
}

// sameAcquisition compares acquisition dates, unknown equal to unknown,
// without dereferencing nil.
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
	if portfoliotest.Realized(t, p) != wantRealized {
		t.Errorf("realized = %d, want %d", portfoliotest.Realized(t, p), wantRealized)
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
	if portfoliotest.Realized(t, p) != 0 {
		t.Errorf("realized = %d, want 0", portfoliotest.Realized(t, p))
	}
}

func TestDriftRemainderGoesToLastPiece(t *testing.T) {
	// A lot of 3 at 100.01 sold one by one: 3333, then 3334, then the remaining
	// 3334 — summing to 10001 exactly.
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
	if portfoliotest.Realized(t, p) != 12_000-10_001 {
		t.Errorf("realized = %d, want %d", portfoliotest.Realized(t, p), 12_000-10_001)
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
		// A conversion is cash-level: no position is created for it.
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
	if pos[ofz].CostMinor != 0 || portfoliotest.Realized(t, pos[ofz]) != 100_000 {
		t.Errorf("cost=%d realized=%d, want 0/100000", pos[ofz].CostMinor, portfoliotest.Realized(t, pos[ofz]))
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
	if p == nil || !p.Quantity.IsZero() || portfoliotest.Realized(t, p) != 250_000 {
		t.Fatalf("closed position = %+v", p)
	}
}

// An entry reaching a single-currency figure must repeat the position's
// currency. The exemptions (income, fees, sales, moneyless entries) are pinned
// in their own files.
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
		// The message names both currencies and the instrument.
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

// lotSums totals the remaining lots, which must equal the position's quantity
// and cost.
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

// A partial sale shrinks the lot; the remainder keeps its day.
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

// Through a long mix of awkward buys and sells, lot costs sum exactly to the
// position cost, and money spent equals what is held plus what was released.
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
	releasedMinor := proceedsMinor - sellFeesMinor - portfoliotest.Realized(t, p)
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

// Splits scale quantities and amortizations drain cost, keeping the lot
// totals equal to the position.
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

// A transfer_in without a breakdown creates an undated lot. The transfer day,
// which the engine used to put there, is a fact about paperwork, not a
// purchase, and is asserted against by name.
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
	// Only the date is unknown; quantity and cost are real.
	if !p.Lots[0].Quantity.Equal(d("4")) || p.Lots[0].CostMinor != 40_000 {
		t.Errorf("lot = {qty %s cost %d}, want {4 40000}", p.Lots[0].Quantity, p.Lots[0].CostMinor)
	}
	checkLotInvariants(t, p)
}

// An undated lot is a full member of the queue: released in turn, split
// proportionally, rescaled by a split. (Its place, first, is pinned
// elsewhere.)
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
	// The sale took from the undated lot at the head: 3333 released, 6668 left.
	if p.Lots[0].AcquiredOn != nil {
		t.Errorf("first lot acquired on %s, want unknown — a release must not date what it leaves behind", acquired(p.Lots[0].AcquiredOn))
	}
	if p.Lots[0].CostMinor != 6_668 {
		t.Errorf("undated lot cost = %d, want 6668 (10001 − floor(10001/3))", p.Lots[0].CostMinor)
	}
	if portfoliotest.Realized(t, p) != 4_000-3_333 {
		t.Errorf("realized = %d, want %d — the sale must consume the undated lot at ITS cost, not the buy's",
			portfoliotest.Realized(t, p), 4_000-3_333)
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

// A transferred lot bought earlier than the account's own is sold first: FIFO
// is by acquisition (НК РФ ст. 214.1 п. 13, 26 CFR 1.1012-1(c)(1)(i)).
//
//	day 20: buy 10 for 300 000; day 25: transfer in 10 bought on day 2 for
//	100 000; then sell 10 for 400 000
//	  arrival order:     realized 100 000, remaining 300 000 (day 2)
//	  acquisition order: realized 300 000, remaining 300 000 (day 20)
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

	if portfoliotest.Realized(t, p) == 400_000-300_000 {
		t.Fatalf("realized = %d — the day-%d purchase was released because the journal mentions it first; the parcel bought on day %d is the earlier ACQUISITION and the queue is built from that",
			portfoliotest.Realized(t, p), 20, 2)
	}
	if portfoliotest.Realized(t, p) != 400_000-100_000 {
		t.Errorf("realized = %d, want %d (400000 − the day-2 parcel's cost 100000)",
			portfoliotest.Realized(t, p), 400_000-100_000)
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

	// The remainder in roubles, as Handler.positionInBase computes it:
	// 	  before: 100000 × 60 =  6000000
	// 	  now:    300000 × 90 = 27000000
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

// Amortization drains by acquisition order too: an older transferred lot is
// drained before a newer purchase the journal mentions first.
//
//	day 20 buy $3 000; day 25 transfer of shares bought day 2, $1 000;
//	then a $400 amortization drains the day-2 lot
//	at 60 (day 2) and 90 (day 20): 600×60 + 3 000×90 = 306 000 ₽, not
//	2 600×90 + 1 000×60 = 294 000 ₽
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

	// The rouble basis: each lot at its own day's rate.
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

// A lot with nothing to give yields no amortization piece. Otherwise the
// undated zero-basis lot at the head would emit an undated empty piece and
// null the rouble realized figure for nothing.
func TestAmortizationSkipsAnEmptyUndatedLot(t *testing.T) {
	ops := []portfolio.Operation{
		// A zero-basis parcel without a breakdown: undated, at the head.
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

// An undated lot leaves the queue before every dated one, however old: the
// head is the only place needing no invented date (cf. 26 CFR
// 1.6045A-1(b)(10)).
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

	// Selling ten takes the undated parcel whole and leaves the old purchase.
	sold := append(append([]portfolio.Operation{}, ops...),
		op(portfolio.TypeSell, 6, &sber, "10", "", 100_000, 0))
	after, err := portfolio.Compute(sold)
	if err != nil {
		t.Fatalf("Compute after the sale: %v", err)
	}
	q := after[sber]
	if portfoliotest.Realized(t, q) == 100_000-10_000 {
		t.Fatalf("realized = %d — the day-1 purchase was released; a lot whose acquisition is unknown leaves first", portfoliotest.Realized(t, q))
	}
	if portfoliotest.Realized(t, q) != 100_000-90_000 {
		t.Errorf("realized = %d, want %d (100000 − the undated parcel's 90000)",
			portfoliotest.Realized(t, q), 100_000-90_000)
	}
	// Selling drains the unknown first, so what is left can be valued again.
	if len(q.Lots) != 1 || !sameAcquisition(q.Lots[0].AcquiredOn, dayp(1)) {
		t.Errorf("lots after the sale = %+v, want only the day-%d purchase", q.Lots, 1)
	}
	checkLotInvariants(t, q)
}

// Same-day lots keep journal order, deterministically: the law says nothing
// finer than the day. Seven transfers each insert an undated, a day-2 and a
// day-4 piece into the middle of the queue, which an unstable sort would
// permute; every cost is distinct.
func TestQueueOrderIsStableUnderTies(t *testing.T) {
	const parcels = 7
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 2, &sber, "1", "", -1_000, 0),
		op(portfolio.TypeBuy, 4, &sber, "1", "", -2_000, 0),
	}
	// The costs in order of entry; the queue is the three lists concatenated.
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

// A purchase's lot always carries its day.
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

// A release inside the oldest lot yields one piece with that lot's date.
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

// A release across two lots yields a piece per lot, in order, each with its
// own cost and date.
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

// A partial release takes a floored share of the lot's cost and its date.
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

// Over an awkward mix, ReleasedLots' pieces sum to ReleasedCost and to the
// requested quantity.
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
	// Six units remain: a 1-unit tail of the day-6 lot and the day-9 lot.
	// Releases stay inside, cross cleanly, cross awkwardly, and drain all.
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

// ReleasedLots refuses an oversell like the other release paths.
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

// A split keeps quantities the journal can record (ten places): a 1:3 reverse
// split of 0.35 is 0.116666666655, which no sell row could name. Every
// quantity is recordable and none exceeds the exact product.
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

	// 0.7 × 0.3333333333 = 0.23333333331; the journal keeps 0.2333333333.
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
	// The lost fraction comes off one lot (the running total is truncated), so
	// lots still sum exactly; no date moves.
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

	// The whole position can now be sold in one recordable entry.
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

// A reverse split that rounds a lot's holding away keeps its cost and day.
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

// A split over a queue reordered by an older transferred lot moves no cost
// between acquisition days.
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
	// The older transferred parcel still leads.
	if !sameAcquisition(p.Lots[0].AcquiredOn, dayp(2)) || !sameAcquisition(p.Lots[1].AcquiredOn, dayp(20)) {
		t.Fatalf("lots dated %s then %s, want %s then %s — a split must not reorder the queue",
			acquired(p.Lots[0].AcquiredOn), acquired(p.Lots[1].AcquiredOn), day(2).Format("2006-01-02"), day(20).Format("2006-01-02"))
	}
	// Quantities halve; costs are untouched.
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

// releasedText renders a realization's pieces for a failure message.
func releasedText(pieces []portfolio.ReleasedLot) string {
	parts := make([]string, 0, len(pieces))
	for _, pc := range pieces {
		parts = append(parts, fmt.Sprintf("%s units/%d minor acquired %s", pc.Quantity, pc.CostMinor, acquired(pc.AcquiredOn)))
	}
	return "[" + strings.Join(parts, "; ") + "]"
}

// checkReleased compares pieces and reports the whole breakdown on mismatch.
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

// checkRealizationsSumToTotal checks the realizations account exactly for
// the realized total.
func checkRealizationsSumToTotal(t *testing.T, p *portfolio.Position) {
	t.Helper()
	var sum int64
	for _, r := range p.Realizations {
		sum += r.PnLMinor()
	}
	if sum != portfoliotest.Realized(t, p) {
		t.Errorf("realizations sum to %d, but the position realized %d (off by %d) — every minor unit of the total must be accounted for by an event",
			sum, portfoliotest.Realized(t, p), sum-portfoliotest.Realized(t, p))
		for i, r := range p.Realizations {
			t.Logf("  event %d on %s: proceeds %d, fee %d, released %s → %d",
				i, r.OccurredOn.Format("2006-01-02"), r.ProceedsMinor, r.FeeMinor, releasedText(r.Released), r.PnLMinor())
		}
	}
}

// A sale records what it was made of: 15 shares across lots bought on days 2
// and 3 yield two pieces with their own dates (НК РФ ст. 210 п. 5).
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
	// The whole day-2 lot (100 000 + 10 fee) and half the day-3 lot (55 005).
	checkReleased(t, r.Released, []portfolio.ReleasedLot{
		{Quantity: d("10"), CostMinor: 100_010, AcquiredOn: dayp(2)},
		{Quantity: d("5"), CostMinor: 55_005, AcquiredOn: dayp(3)},
	})
	if want := int64(180_000 - 18 - 155_015); r.PnLMinor() != want {
		t.Errorf("event result = %d, want %d", r.PnLMinor(), want)
	}
	checkRealizationsSumToTotal(t, p)
}

// Three sales on three days make three realizations, in order, each with its
// own day and proceeds.
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
		// Each third of a 90 000 lot releases 30 000, on the lot's day.
		checkReleased(t, r.Released, []portfolio.ReleasedLot{{Quantity: d("3"), CostMinor: 30_000, AcquiredOn: dayp(1)}})
	}
	checkRealizationsSumToTotal(t, p)
}

// A transfer out produces no realization, with or without a breakdown.
func TestTransferOutRecordsNoRealization(t *testing.T) {
	for name, ops := range map[string][]portfolio.Operation{
		"with a recorded breakdown": {
			named(op(portfolio.TypeBuy, 1, &sber, "10", "100", -100_000, 0)),
		},
		"without one": {
			op(portfolio.TypeBuy, 1, &sber, "10", "100", -100_000, 0),
			op(portfolio.TypeTransferOut, 5, &sber, "4", "", 40_000, 0),
		},
	} {
		t.Run(name, func(t *testing.T) {
			if len(ops) == 1 {
				ops = append(ops, recordedOut(t, ops, 5, "4"))
			}
			pos, err := portfolio.Compute(ops)
			if err != nil {
				t.Fatalf("Compute: %v", err)
			}
			p := pos[sber]
			if len(p.Realizations) != 0 {
				t.Errorf("realizations = %d %+v, want 0 — a transfer between one's own accounts realizes nothing",
					len(p.Realizations), p.Realizations)
			}
			if portfoliotest.Realized(t, p) != 0 {
				t.Errorf("realized = %d, want 0", portfoliotest.Realized(t, p))
			}
			checkRealizationsSumToTotal(t, p)
		})
	}
}

// An amortization records a realization even when its result is zero here:
// in roubles the covered part is not neutral.
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
	// Fully covered: nothing realized here; 250 000 retired on the purchase day,
	// pieces without quantity.
	first := p.Realizations[0]
	if !first.OccurredOn.Equal(day(10)) || first.ProceedsMinor != 250_000 || first.FeeMinor != 0 {
		t.Errorf("first event = %s/%d/%d, want %s/250000/0",
			first.OccurredOn.Format("2006-01-02"), first.ProceedsMinor, first.FeeMinor, day(10).Format("2006-01-02"))
	}
	checkReleased(t, first.Released, []portfolio.ReleasedLot{{Quantity: d("0"), CostMinor: 250_000, AcquiredOn: dayp(1)}})
	if first.PnLMinor() != 0 {
		t.Errorf("first event result = %d, want 0", first.PnLMinor())
	}
	// Beyond the basis: 700 000 retires, 100 000 is realized.
	second := p.Realizations[1]
	if !second.OccurredOn.Equal(day(20)) || second.ProceedsMinor != 800_000 {
		t.Errorf("second event = %s/%d, want %s/800000",
			second.OccurredOn.Format("2006-01-02"), second.ProceedsMinor, day(20).Format("2006-01-02"))
	}
	checkReleased(t, second.Released, []portfolio.ReleasedLot{{Quantity: d("0"), CostMinor: 700_000, AcquiredOn: dayp(1)}})
	if second.PnLMinor() != 100_000 {
		t.Errorf("second event result = %d, want 100000", second.PnLMinor())
	}
	if portfoliotest.Realized(t, p) != 100_000 {
		t.Errorf("realized = %d, want 100000 — unchanged by recording what it was made of", portfoliotest.Realized(t, p))
	}
	checkRealizationsSumToTotal(t, p)
}

// A realization may carry an undated basis piece (sold from a transfer
// without a breakdown); the absence is carried, not replaced.
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
	// The undated parcel goes whole; the rest is floor(150 000 × 5/10) = 75 000.
	checkReleased(t, p.Realizations[0].Released, []portfolio.ReleasedLot{
		{Quantity: d("10"), CostMinor: 100_000, AcquiredOn: nil},
		{Quantity: d("5"), CostMinor: 75_000, AcquiredOn: dayp(2)},
	})
	if want := int64(300_000 - 20 - 175_000); p.Realizations[0].PnLMinor() != want {
		t.Errorf("event result = %d, want %d", p.Realizations[0].PnLMinor(), want)
	}
	checkRealizationsSumToTotal(t, p)
}

// The realizations are the realized total, through splits, transfers in and
// out (resolved like the write path), an undated parcel and amortizations,
// with awkward amounts. Both totals are pinned by hand:
//
//	day  3 sell:  12 345 − 30 011 −  11 =  −17 677
//	day  7 sell: 130 007 − 36 680 −  13 =   93 314
//	day  9 amrt:   5 000 −  5 000       =        0
//	day 11 sell:  60 001 − 46 119 −   9 =   13 873
//	day 13 sell:  40 000 − 25 000 −   7 =   14 993
//	day 14 amrt: 900 000 − 155 010      =  744 990
//	day 15 sell: 200 000 −      0 −   3 =  199 997
//	                                      1 049 490
//
// Seven realizations, none for the transfers.
func TestRealizationsSumToRealizedPnL(t *testing.T) {
	var ops []portfolio.Operation
	add := func(o portfolio.Operation) { ops = append(ops, named(o)) }
	split := func(dayN int, ratio string) portfolio.Operation {
		o := op(portfolio.TypeSplit, dayN, &sber, "", "", 0, 0)
		o.SplitRatio = dp(ratio)
		return o
	}
	// moveOut is a departing leg with its breakdown resolved as the transfer
	// service does.
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
	// Shares older than some held ones reorder the queue.
	add(transferIn(6, "9", 90_009, piece("4", 40_004, 1), piece("5", 50_005, 4)))
	add(op(portfolio.TypeSell, 7, &sber, "11", "", 130_007, 13))
	add(moveOut(8, "7"))
	add(op(portfolio.TypeAmortization, 9, &sber, "", "", 5_000, 0))
	add(split(10, "0.5"))
	add(op(portfolio.TypeSell, 11, &sber, "4", "", 60_001, 9))
	// A hand-given basis: undated, at the head, sold into on day 13.
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
	if portfoliotest.Realized(t, p) != 1_049_490 {
		t.Errorf("realized = %d, want 1049490 (see the derivation above)", portfoliotest.Realized(t, p))
	}
	if len(p.Realizations) != 7 {
		t.Fatalf("realizations = %d, want 7 — five sales and two amortizations, and nothing for either transfer", len(p.Realizations))
	}
	// An undated parcel did reach a realization.
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

// An amortization on a paper never acquired here is refused (#17): it would
// credit the whole payment as profit. The literal is the old engine's figure.
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

// Amortizations the refusal must allow, each a journal this program writes.
func TestAmortizationIsAllowedWherePrincipalCanCome(t *testing.T) {
	// A final repayment folded after the same-day redemption that emptied the
	// position, as the importer writes it.
	redeemedThenRepaid := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &ofz, "10", "950", -950_000, 0),
		op(portfolio.TypeSell, 10, &ofz, "10", "1000", 1_000_000, 0),
		op(portfolio.TypeAmortization, 10, &ofz, "", "", 30_000, 0),
	}
	// A held bond whose basis earlier amortizations spent: further principal is
	// gain.
	basisSpent := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &ofz, "10", "950", -950_000, 0),
		op(portfolio.TypeAmortization, 5, &ofz, "", "", 950_000, 0),
		op(portfolio.TypeAmortization, 6, &ofz, "", "", 40_000, 0),
	}
	// An arriving transfer is an acquisition too.
	arrivedByTransfer := []portfolio.Operation{
		{
			Type: portfolio.TypeTransferIn, OccurredOn: day(1), InstrumentID: &ofz,
			Currency: "RUB", Quantity: dp("10"), AmountMinor: 950_000,
		},
		// Past the parcel's 950 000, so the excess is realized.
		op(portfolio.TypeAmortization, 5, &ofz, "", "", 990_000, 0),
	}
	for name, tc := range map[string]struct {
		ops          []portfolio.Operation
		wantRealized int64
	}{
		// 50 000 from the sale, then the repayment realized whole.
		"repayment folded after the same-day redemption": {redeemedThenRepaid, 50_000 + 30_000},
		"basis already fully amortized":                  {basisSpent, 40_000},
		"parcel acquired by transfer":                    {arrivedByTransfer, 40_000},
	} {
		t.Run(name, func(t *testing.T) {
			pos, err := portfolio.Compute(tc.ops)
			if err != nil {
				t.Fatalf("Compute: %v", err)
			}
			if portfoliotest.Realized(t, pos[ofz]) != tc.wantRealized {
				t.Errorf("realized = %d, want %d", portfoliotest.Realized(t, pos[ofz]), tc.wantRealized)
			}
		})
	}
}

// Dividends, coupons and taxes on a paper never acquired here stay legitimate:
// they claim nothing about cost.
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
	if portfoliotest.Realized(t, p) != 0 {
		t.Errorf("realized = %d, want 0 — payments claim nothing about cost", portfoliotest.Realized(t, p))
	}
}

// A redemption computes exactly as a sale: every figure is compared with the
// same journal recorded as a sale.
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
	// The released parcels, piece by piece with their days.
	if !reflect.DeepEqual(sale.Realizations[0], redeemed.Realizations[0]) {
		t.Errorf("the disposal itself differs:\n sale       %+v\n redemption %+v",
			sale.Realizations[0], redeemed.Realizations[0])
	}
	// A positive result was produced, so the comparison is not empty.
	if !saleOK || saleRealized == 0 {
		t.Fatalf("the fixture realized nothing (%d, %v): the equality above would be vacuous", saleRealized, saleOK)
	}
}

// A redemption refuses what a sale refuses, naming its own type.
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

// Selling the last share takes a shareless lot's money with it, wherever it
// stood in the queue (#193).
func TestSellingTheLastShareTakesAShareLessLotsMoneyWithIt(t *testing.T) {
	split := op(portfolio.TypeSplit, 3, &sber, "", "", 0, 0)
	split.SplitRatio = dp("0.0000000001")
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "5", "100", -50_000, 0),
		op(portfolio.TypeBuy, 2, &sber, "0.4", "10", -400, 0),
		split, // 5 -> 0.0000000005, 0.4 -> nothing the journal can name
		op(portfolio.TypeSell, 4, &sber, "0.0000000005", "", 60_000, 0),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	if !p.Quantity.IsZero() || p.CostMinor != 0 {
		t.Errorf("closed position holds %s units and %d of basis, want neither", p.Quantity, p.CostMinor)
	}
	checkLotInvariants(t, p)

	if len(p.Realizations) != 1 {
		t.Fatalf("realizations = %d, want the one sale", len(p.Realizations))
	}
	released := p.Realizations[0].Released
	if len(released) != 2 {
		t.Fatalf("released = %+v, want the sold lot and the shareless one behind it", released)
	}
	last := released[1]
	if !last.Quantity.IsZero() || last.CostMinor != 400 || !sameAcquisition(last.AcquiredOn, dayp(2)) {
		t.Errorf("shareless piece = %s/%d/%s, want 0/400 on the day it was bought",
			last.Quantity, last.CostMinor, acquired(last.AcquiredOn))
	}
	if got, ok := p.RealizedPnL(); !ok || got != 60_000-50_400 {
		t.Errorf("realized = %d (%v), want 9600 — everything paid for the position is its cost", got, ok)
	}
}

// A partial sale leaves a shareless lot waiting.
func TestAPartialSaleLeavesAShareLessLotWaiting(t *testing.T) {
	split := op(portfolio.TypeSplit, 3, &sber, "", "", 0, 0)
	split.SplitRatio = dp("0.0000000001")
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &sber, "6", "100", -60_000, 0),
		op(portfolio.TypeBuy, 2, &sber, "0.4", "10", -400, 0),
		split,
		op(portfolio.TypeSell, 4, &sber, "0.0000000003", "", 40_000, 0),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[sber]
	if p.CostMinor != 30_400 || len(p.Lots) != 2 {
		t.Errorf("position cost %d over %d lots, want 30400 over 2 — half the first lot and the whole shareless one", p.CostMinor, len(p.Lots))
	}
	checkLotInvariants(t, p)
}

// A shareless piece in a transfer record is legitimate; both legs fold it
// under its own lot and day.
func TestATransferRecordNamesAShareLessParcelByItsOwnDay(t *testing.T) {
	split := op(portfolio.TypeSplit, 2, &sber, "", "", 0, 0)
	split.SplitRatio = dp("0.0000000001")
	early := named(op(portfolio.TypeBuy, 1, &sber, "0.4", "10", -400, 0))
	late := named(op(portfolio.TypeBuy, 3, &sber, "5", "100", -50_000, 0))
	pieces := []portfolio.ReleasedLot{
		pieceFrom(early, 0, "0", 400, 1),
		pieceFrom(late, 0, "5", 50_000, 3),
	}
	out := op(portfolio.TypeTransferOut, 5, &sber, "5", "", 50_400, 0)
	out.TransferLots = pieces
	source, err := portfolio.Compute([]portfolio.Operation{early, split, late, out})
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	if p := source[sber]; !p.Quantity.IsZero() || p.CostMinor != 0 || len(p.Lots) != 0 {
		t.Errorf("source keeps %s units, %d of basis, %d lots; want nothing", p.Quantity, p.CostMinor, len(p.Lots))
	}

	in := op(portfolio.TypeTransferIn, 5, &sber, "5", "", 50_400, 0)
	in.TransferLots = pieces
	dest, err := portfolio.Compute([]portfolio.Operation{in})
	if err != nil {
		t.Fatalf("destination: %v", err)
	}
	p := dest[sber]
	if len(p.Lots) != 2 {
		t.Fatalf("destination lots = %+v, want the shareless parcel and the shares", p.Lots)
	}
	if !p.Lots[0].Quantity.IsZero() || p.Lots[0].CostMinor != 400 || !sameAcquisition(p.Lots[0].AcquiredOn, dayp(1)) {
		t.Errorf("shareless lot = %s/%d/%s, want 0/400 on day 1 — its money keeps the day it was spent",
			p.Lots[0].Quantity, p.Lots[0].CostMinor, acquired(p.Lots[0].AcquiredOn))
	}
	checkLotInvariants(t, p)
}

// A shareless piece with no matching parcel is refused loudly.
func TestAShareLessPieceWithNoParcelBehindItIsRefused(t *testing.T) {
	out := op(portfolio.TypeTransferOut, 5, &sber, "5", "", 50_400, 0)
	out.TransferLots = []portfolio.ReleasedLot{
		{Quantity: d("0"), CostMinor: 400, AcquiredOn: dayp(1)},
		{Quantity: d("5"), CostMinor: 50_000, AcquiredOn: dayp(3)},
	}
	_, err := portfolio.Compute([]portfolio.Operation{
		op(portfolio.TypeBuy, 3, &sber, "5", "100", -50_400, 0),
		out,
	})
	if !errors.Is(err, portfolio.ErrBadOperation) {
		t.Fatalf("err = %v, want ErrBadOperation", err)
	}
}
