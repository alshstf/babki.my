package portfolio

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// The money an account holds is a HOLDING, and these tests are about the two
// things that makes it: a balance made of every cash effect in the journal, and
// the parcels behind it, each knowing the day it arrived. What a parcel is worth
// in another currency is decided a layer up — this package holds no rates.

func day(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return d
}

func cashOp(t *testing.T, typ Type, on, currency string, amountMinor, feeMinor int64) Operation {
	t.Helper()
	return Operation{
		ID: uuid.New(), Type: typ, OccurredOn: day(t, on),
		Currency: currency, AmountMinor: amountMinor, FeeMinor: feeMinor,
	}
}

// TestCashCountsEveryEffectIncludingTheOnesNoPositionSees is the balance in one
// fixture, with every kind of entry that moves money present at once — including
// the four the position engine refuses by type (deposit, withdrawal, interest
// and a conversion's legs), which are exactly what a cash balance is made of.
//
//	deposit                 +1 000 000
//	a purchase of shares      -250 000, commission 200   -> -250 200
//	its later sale            +300 000, commission 300   -> +299 700
//	a coupon                    +5 000
//	a commission of its own       -900
//	interest on the balance       +700
//	a withdrawal              -100 000
//	                          ==========
//	                             954 300
func TestCashCountsEveryEffectIncludingTheOnesNoPositionSees(t *testing.T) {
	ops := []Operation{
		cashOp(t, TypeDeposit, "2026-01-10", "RUB", 1_000_000, 0),
		cashOp(t, TypeBuy, "2026-02-10", "RUB", -250_000, 200),
		cashOp(t, TypeSell, "2026-03-10", "RUB", 300_000, 300),
		cashOp(t, TypeCoupon, "2026-04-10", "RUB", 5_000, 0),
		cashOp(t, TypeFee, "2026-04-11", "RUB", -900, 0),
		cashOp(t, TypeInterest, "2026-04-12", "RUB", 700, 0),
		cashOp(t, TypeWithdrawal, "2026-05-10", "RUB", -100_000, 0),
	}

	cash, err := Cash(ops)
	if err != nil {
		t.Fatalf("Cash: %v", err)
	}
	rub, ok := cash["RUB"]
	if !ok {
		t.Fatalf("no RUB position among %v", cash)
	}
	switch rub.Minor {
	case 954_800:
		t.Errorf("balance = 954800 — the two commissions were not taken. A purchase's is charged on top of its amount and a sale's comes out of the proceeds; both are amount less fee")
	case 954_300:
	default:
		t.Errorf("balance = %d, want 954300", rub.Minor)
	}
}

// TestCashIgnoresWhatMovesNoMoney: a securities transfer carries a COST BASIS in
// its amount, not cash. Shares moving between two of the owner's accounts change
// no balance on either — and an amount of 300 000 read as money would be a
// third of a million rubles appearing from nowhere.
func TestCashIgnoresWhatMovesNoMoney(t *testing.T) {
	ratio := decimal.RequireFromString("2")
	ops := []Operation{
		cashOp(t, TypeDeposit, "2026-01-10", "RUB", 1_000_000, 0),
		cashOp(t, TypeTransferIn, "2026-02-10", "RUB", 300_000, 0),
		cashOp(t, TypeTransferOut, "2026-03-10", "RUB", 300_000, 0),
	}
	split := cashOp(t, TypeSplit, "2026-04-10", "RUB", 0, 0)
	split.SplitRatio = &ratio
	ops = append(ops, split)

	cash, err := Cash(ops)
	if err != nil {
		t.Fatalf("Cash: %v", err)
	}
	if got := cash["RUB"].Minor; got != 1_000_000 {
		t.Errorf("balance = %d, want 1000000 — only the deposit is money", got)
	}
}

// TestCashKeepsEachCurrencyApart. A conversion is TWO entries, one per side, and
// the pair is what makes a currency balance possible at all: without it the
// yuan a bond was bought with came from nowhere.
func TestCashKeepsEachCurrencyApart(t *testing.T) {
	ops := []Operation{
		cashOp(t, TypeDeposit, "2026-01-10", "RUB", 10_000_000, 0),
		// 100 000 ₽ for 8 000 ¥.
		cashOp(t, TypeConversion, "2026-02-10", "RUB", -10_000_000, 0),
		cashOp(t, TypeConversion, "2026-02-10", "CNY", 800_000, 0),
		// A yuan bond bought with part of them.
		cashOp(t, TypeBuy, "2026-03-10", "CNY", -500_000, 0),
	}

	cash, err := Cash(ops)
	if err != nil {
		t.Fatalf("Cash: %v", err)
	}
	if got := cash["RUB"].Minor; got != 0 {
		t.Errorf("RUB balance = %d, want 0 — every ruble went into the exchange", got)
	}
	if got := cash["CNY"].Minor; got != 300_000 {
		t.Errorf("CNY balance = %d, want 300000 (8 000 ¥ bought, 5 000 ¥ spent)", got)
	}
	if len(cash) != 2 {
		t.Errorf("currencies = %d, want 2 — rubles and yuan are never added together", len(cash))
	}
}

// TestCashLotsKeepTheDayMoneyArrived is what separates this from a balance. The
// parcels are consumed oldest-first, so what is LEFT knows when it came — which
// is the only thing that makes it valuable in another currency (a layer up
// strikes each parcel at its own day's rate, exactly as a share's lots are).
func TestCashLotsKeepTheDayMoneyArrived(t *testing.T) {
	ops := []Operation{
		cashOp(t, TypeConversion, "2026-01-10", "CNY", 100_000, 0), // 1 000 ¥
		cashOp(t, TypeConversion, "2026-02-10", "CNY", 200_000, 0), // 2 000 ¥
		cashOp(t, TypeBuy, "2026-03-10", "CNY", -150_000, 0),       // 1 500 ¥ spent
	}

	cash, err := Cash(ops)
	if err != nil {
		t.Fatalf("Cash: %v", err)
	}
	lots := cash["CNY"].Lots
	if len(lots) != 1 {
		t.Fatalf("lots = %+v, want one: the January parcel is gone entirely and half of February's is left", lots)
	}
	if lots[0].Minor != 150_000 {
		t.Errorf("the remaining parcel is %d, want 150000", lots[0].Minor)
	}
	if !lots[0].On.Equal(day(t, "2026-02-10")) {
		t.Errorf("the remaining parcel is dated %s, want 2026-02-10 — the queue takes the OLDEST first, so what is left is the newer money", lots[0].On.Format("2006-01-02"))
	}
	if sum := lots[0].Minor; sum != cash["CNY"].Minor {
		t.Errorf("the parcels sum to %d and the balance is %d — on a positive balance they are the same money counted twice", sum, cash["CNY"].Minor)
	}
}

// TestCashGoesNegativeRatherThanRefusing is the owner's own account. Some of his
// currency purchases are trades the broker will not explain, so the journal
// spends yuan it never saw arrive. A share position that went negative would be
// a broken journal and is refused; a cash balance that does is an ordinary
// consequence of a gap already reported elsewhere, and hiding it behind a floor
// of zero would hide exactly the discrepancy a reader needs to see.
func TestCashGoesNegativeRatherThanRefusing(t *testing.T) {
	ops := []Operation{
		cashOp(t, TypeCoupon, "2026-01-10", "CNY", 10_000, 0),
		cashOp(t, TypeBuy, "2026-02-10", "CNY", -50_000, 0),
	}

	cash, err := Cash(ops)
	if err != nil {
		t.Fatalf("Cash refused a negative balance: %v", err)
	}
	if got := cash["CNY"].Minor; got != -40_000 {
		t.Errorf("balance = %d, want -40000", got)
	}
	if len(cash["CNY"].Lots) != 0 {
		t.Errorf("lots = %+v, want none: nothing is held", cash["CNY"].Lots)
	}
}

// TestCashNamesACurrencyWhoseBalanceCameToNought: an account that bought dollars
// and sold them all again HAS held dollars. Saying nothing about them is a
// different claim from saying the balance is zero, and the second is the true
// one.
func TestCashNamesACurrencyWhoseBalanceCameToNought(t *testing.T) {
	ops := []Operation{
		cashOp(t, TypeConversion, "2026-01-10", "USD", 100_000, 0),
		cashOp(t, TypeConversion, "2026-02-10", "USD", -100_000, 0),
	}

	cash, err := Cash(ops)
	if err != nil {
		t.Fatalf("Cash: %v", err)
	}
	usd, ok := cash["USD"]
	if !ok {
		t.Fatalf("the dollars are missing entirely from %v", cash)
	}
	if usd.Minor != 0 || len(usd.Lots) != 0 {
		t.Errorf("balance %d with %d parcels, want 0 and none", usd.Minor, len(usd.Lots))
	}
}

// TestCashByCurrencyIsOrdered: a map's order is random and these figures go on a
// screen.
func TestCashByCurrencyIsOrdered(t *testing.T) {
	ops := []Operation{
		cashOp(t, TypeDeposit, "2026-01-10", "USD", 1, 0),
		cashOp(t, TypeDeposit, "2026-01-10", "CNY", 1, 0),
		cashOp(t, TypeDeposit, "2026-01-10", "RUB", 1, 0),
	}
	cash, err := Cash(ops)
	if err != nil {
		t.Fatalf("Cash: %v", err)
	}
	var got []string
	for _, p := range CashByCurrency(cash) {
		got = append(got, p.Currency)
	}
	if len(got) != 3 || got[0] != "CNY" || got[1] != "RUB" || got[2] != "USD" {
		t.Errorf("order = %v, want CNY RUB USD", got)
	}
}

// TestCashRecordsWhatLeftAndWhen is what makes a banked currency result
// visible at all. The parcels a departure took, and the day it went, are the two
// halves of "sold at a better rate than it was bought" — and a screen holding
// only balances reports a gain of exactly nought on money that has already been
// turned back.
func TestCashRecordsWhatLeftAndWhen(t *testing.T) {
	ops := []Operation{
		cashOp(t, TypeConversion, "2026-01-10", "USD", 100_000, 0), // $1 000 in
		cashOp(t, TypeConversion, "2026-03-10", "USD", -60_000, 0), // $600 out
	}

	cash, err := Cash(ops)
	if err != nil {
		t.Fatalf("Cash: %v", err)
	}
	usd := cash["USD"]
	if len(usd.Realizations) != 1 {
		t.Fatalf("realizations = %+v, want one", usd.Realizations)
	}
	r := usd.Realizations[0]
	if r.Minor() != 60_000 {
		t.Errorf("the departure accounts for %d, want 60000", r.Minor())
	}
	if !r.OccurredOn.Equal(day(t, "2026-03-10")) {
		t.Errorf("the departure is dated %s, want 2026-03-10 — that is the day whose rate its proceeds are struck at", r.OccurredOn.Format("2006-01-02"))
	}
	if len(r.Released) != 1 || !r.Released[0].On.Equal(day(t, "2026-01-10")) {
		t.Errorf("released %+v, want one parcel dated 2026-01-10 — the day the money ARRIVED, which is what its cost is struck at", r.Released)
	}
	if usd.Minor != 40_000 || len(usd.Lots) != 1 || usd.Lots[0].Minor != 40_000 {
		t.Errorf("what is left is %d in %+v, want 40000 in one parcel", usd.Minor, usd.Lots)
	}
}

// TestCashSplitsADepartureAcrossTheParcelsItTakes: one payment can reach back
// through several arrivals, and each carries its own day. Valuing the whole
// departure at the oldest parcel's rate — or at the newest — is a different
// number, and the split is what makes it the right one.
func TestCashSplitsADepartureAcrossTheParcelsItTakes(t *testing.T) {
	ops := []Operation{
		cashOp(t, TypeConversion, "2026-01-10", "USD", 100_000, 0),
		cashOp(t, TypeConversion, "2026-02-10", "USD", 100_000, 0),
		cashOp(t, TypeBuy, "2026-03-10", "USD", -150_000, 0),
	}

	cash, err := Cash(ops)
	if err != nil {
		t.Fatalf("Cash: %v", err)
	}
	released := cash["USD"].Realizations[0].Released
	if len(released) != 2 {
		t.Fatalf("released %+v, want two parcels: the January one entirely and half of February's", released)
	}
	if released[0].Minor != 100_000 || !released[0].On.Equal(day(t, "2026-01-10")) {
		t.Errorf("the first parcel is %+v, want 100000 dated 2026-01-10", released[0])
	}
	if released[1].Minor != 50_000 || !released[1].On.Equal(day(t, "2026-02-10")) {
		t.Errorf("the second parcel is %+v, want 50000 dated 2026-02-10 — the REMAINDER of the newer arrival, keeping its own day", released[1])
	}
}

// TestCashRecordsNoDepartureForMoneyItNeverSaw. Spending yuan whose purchase the
// broker would not explain releases nothing: there is no parcel to take, and a
// departure recorded with an empty hand would be a profit struck against a cost
// of nought — the whole payment counted as gain. The negative balance is where
// that gap is reported instead.
func TestCashRecordsNoDepartureForMoneyItNeverSaw(t *testing.T) {
	ops := []Operation{cashOp(t, TypeBuy, "2026-02-10", "CNY", -50_000, 0)}

	cash, err := Cash(ops)
	if err != nil {
		t.Fatalf("Cash: %v", err)
	}
	if got := cash["CNY"].Realizations; len(got) != 0 {
		t.Errorf("realizations = %+v, want none: nothing was held, so nothing was given up", got)
	}
	if cash["CNY"].Minor != -50_000 {
		t.Errorf("balance = %d, want -50000 — the gap is reported here", cash["CNY"].Minor)
	}
}

// TestCashCoversAnOverdraftBeforeHoldingAnything is the invariant this position
// lives or dies by: WHAT THE PARCELS SAY MUST BE WHAT THE BALANCE SAYS.
//
// The case is the owner's own, and it is not exotic. A share is bought with
// dollars whose purchase the journal never saw (a currency trade the broker
// would not explain), so the balance goes below nought; a later sale brings
// dollars in. Without an overdraft to pay off first, the whole arrival becomes a
// parcel and the position claims to hold eight times what it has — and every
// figure struck from those parcels is wrong in the same proportion.
//
// Found by an account total that came out at minus three and a half million on
// a fixture whose answer was plus four.
func TestCashCoversAnOverdraftBeforeHoldingAnything(t *testing.T) {
	ops := []Operation{
		cashOp(t, TypeBuy, "2026-03-10", "USD", -100_000, 0), // spent, never seen arriving
		cashOp(t, TypeSell, "2026-05-10", "USD", 120_000, 0), // and now money comes in
		cashOp(t, TypeFee, "2026-05-10", "USD", -5_000, 0),
	}

	cash, err := Cash(ops)
	if err != nil {
		t.Fatalf("Cash: %v", err)
	}
	usd := cash["USD"]
	if usd.Minor != 15_000 {
		t.Fatalf("balance = %d, want 15000", usd.Minor)
	}
	var held int64
	for _, l := range usd.Lots {
		held += l.Minor
	}
	switch held {
	case 115_000:
		t.Errorf("the parcels hold 115000 against a balance of 15000 — the overdraft was forgotten, so the arrival was counted as held in full. Everything struck from these parcels is then wrong by the same 100000")
	case 15_000:
	default:
		t.Errorf("the parcels hold %d, want 15000 — as much as the balance", held)
	}
	// The departure recorded is the fee alone. Covering an overdraft is not a
	// disposal: the spending it pays off had no known cost, and pairing it with
	// the arriving money's own day would invent one.
	if len(usd.Realizations) != 1 || usd.Realizations[0].Minor() != 5_000 {
		t.Errorf("realizations = %+v, want the 5000 fee alone", usd.Realizations)
	}
}
