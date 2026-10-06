package portfolio

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// An account's money is a holding: a balance of every cash effect, and dated
// parcels behind it. Rates are applied a layer up.

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

// Every money-moving entry counts, including those the engine refuses by type
// (deposit, withdrawal, interest, conversion legs).
//
//	deposit           +1 000 000
//	buy shares          −250 200 (incl. 200 fee)
//	sell them           +299 700 (300 000 − 300 fee)
//	coupon                +5 000
//	own commission          −900
//	interest                +700
//	withdrawal          −100 000
//	                       954 300
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

// A securities transfer's amount is a cost basis, not money.
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

// MovesCash answers for every type, so a new one cannot default to "money" (a
// spin-off's legs once credited the basis twice, #185).
func TestMovesCashClassifiesEveryType(t *testing.T) {
	want := map[Type]bool{
		// Money that changed the balance.
		TypeBuy: true, TypeSell: true, TypeRedemption: true,
		TypeDeposit: true, TypeWithdrawal: true,
		TypeDividend: true, TypeCoupon: true, TypeAmortization: true,
		TypeFee: true, TypeTax: true, TypeInterest: true, TypeConversion: true,
		// A cost basis travelling with a parcel: the amount is not a payment.
		TypeTransferIn: false, TypeTransferOut: false,
		TypeExchangeOut: false, TypeExchangeIn: false,
		TypeSpinoffOut: false, TypeSpinoffIn: false,
		// Rewrites quantities and carries no amount at all.
		TypeSplit: false,
	}
	if len(want) != len(validTypes) {
		t.Fatalf("this table classifies %d types, the enum has %d — classify the new one", len(want), len(validTypes))
	}
	for typ, w := range want {
		if got := MovesCash(Operation{Type: typ, AmountMinor: 1}); got != w {
			t.Errorf("MovesCash(%s) = %v, want %v", typ, got, w)
		}
	}
}

// A spin-off's moved basis (25 000 of 100 000) is not money.
func TestCashIgnoresTheBasisASpinoffMoves(t *testing.T) {
	ops := []Operation{
		cashOp(t, TypeDeposit, "2026-01-10", "RUB", 1_000_000, 0),
		cashOp(t, TypeSpinoffOut, "2026-02-10", "RUB", 2_500_000, 0),
		cashOp(t, TypeSpinoffIn, "2026-02-10", "RUB", 2_500_000, 0),
	}
	cash, err := Cash(ops)
	if err != nil {
		t.Fatalf("Cash: %v", err)
	}
	rub := cash["RUB"]
	if rub.Minor != 1_000_000 {
		t.Errorf("balance = %d, want 1000000 — a spin-off moves basis, not money", rub.Minor)
	}
	if len(rub.Lots) != 1 {
		t.Errorf("cash parcels = %d, want 1 — the deposit alone", len(rub.Lots))
	}
}

// A conversion's two entries keep each currency's balance apart.
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

// Parcels are spent oldest first, so what is left knows its day.
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

// Spending yuan never seen arriving (an unexplained broker trade) makes the
// balance negative, reported rather than refused or floored.
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

// A negative balance names the first day the journal went short — the day an
// opening balance goes on (decision Р-2) — even when it recovered in between;
// a balance back at nought or above names none.
func TestCashNamesTheDayItFirstWentShort(t *testing.T) {
	for name, c := range map[string]struct {
		ops  []Operation
		want string
	}{
		"short since the first purchase": {
			ops: []Operation{
				cashOp(t, TypeBuy, "2026-03-02", "RUB", -30_000_000, 0),
				cashOp(t, TypeDividend, "2026-07-15", "RUB", 400_000, 0),
			},
			want: "2026-03-02",
		},
		"short, made good, short again": {
			ops: []Operation{
				cashOp(t, TypeBuy, "2026-03-02", "RUB", -100_000, 0),
				cashOp(t, TypeDeposit, "2026-04-01", "RUB", 150_000, 0),
				cashOp(t, TypeBuy, "2026-05-01", "RUB", -200_000, 0),
			},
			want: "2026-03-02",
		},
		"short once, not now": {
			ops: []Operation{
				cashOp(t, TypeBuy, "2026-03-02", "RUB", -100_000, 0),
				cashOp(t, TypeDeposit, "2026-04-01", "RUB", 100_000, 0),
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			cash, err := Cash(c.ops)
			if err != nil {
				t.Fatal(err)
			}
			got := cash["RUB"].OverdrawnSince
			switch {
			case c.want == "" && got != nil:
				t.Errorf("short since %s, want no day: the balance is %d", got.Format(time.DateOnly), cash["RUB"].Minor)
			case c.want != "" && (got == nil || !got.Equal(day(t, c.want))):
				t.Errorf("short since %v, want %s", got, c.want)
			}
		})
	}
}

// A currency held and fully spent is still named, with a zero balance.
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

// A departure records its parcels and its day, so a banked currency gain is
// visible.
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

// One payment can take several parcels, each with its own day.
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

// Spending money never seen arriving records no departure: there is no parcel,
// and a cost of nought would make the whole payment a gain.
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

// Arriving money pays off an overdraft first, so the parcels match the balance
// (without it, an account total came out at −3.5 million instead of +4).
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
	// The recorded departure is the fee alone; covering an overdraft is not a
	// disposal.
	if len(usd.Realizations) != 1 || usd.Realizations[0].Minor() != 5_000 {
		t.Errorf("realizations = %+v, want the 5000 fee alone", usd.Realizations)
	}
}
