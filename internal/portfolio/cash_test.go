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
