package portfolio_test

import (
	"errors"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"babki.my/babki/internal/platform/money"
	"babki.my/babki/internal/portfolio"
)

// A POSITION'S COST AND ITS INCOME NEED NOT BE IN ONE CURRENCY, and these tests
// are what says so. A yuan bond is bought for yuan and pays its coupons in
// rubles, converted by the broker on the day of the payment; a dollar share's
// dividend and the tax withheld on it arrive in rubles too. That is ordinary
// Russian brokerage practice — on the owner's own account it accounted for 131
// operations across 14 papers, every one of them refused by the engine with the
// same sentence — and the model simply had no room for it.
//
// THE SAME ARGUMENT HAS SINCE BEEN FOLLOWED WHEREVER IT LEADS, and it does not
// lead to every type. What still has to repeat the position's currency is
// whatever puts money into a figure that holds one: a purchase, a transfer
// carrying a basis, and an amortization, which retires basis BY AMOUNT and so
// would need an fx rate to say how much a ruble payment took off a yuan cost.
// A commission is now kept per currency exactly as the income is, and a SALE
// need not match at all — its proceeds go to a disposal that carries its own
// currency, and what it retires is decided by the quantity sold. An entry that
// moves no money matches nothing because it claims nothing. Both halves are
// pinned here: the refusals below are as much the subject of these tests as the
// acceptances (see Operation.mustMatchPositionCurrency, and
// engine_position_currency_test.go for the rule type by type).
//
// Every figure is written out as a literal rather than derived from the
// fixtures, so a change in how the engine folds income cannot move the expected
// answer with it.
// opInFee is opIn with a commission on the entry.
func opInFee(typ portfolio.Type, dayN int, inst *uuid.UUID, currency, qty string, amount, feeMinor int64) portfolio.Operation {
	o := opIn(typ, dayN, inst, currency, qty, amount)
	o.FeeMinor = feeMinor
	return o
}

func opIn(typ portfolio.Type, dayN int, inst *uuid.UUID, currency, qty string, amount int64) portfolio.Operation {
	o := portfolio.Operation{
		Type: typ, OccurredOn: day(dayN), AmountMinor: amount,
		Currency: currency, InstrumentID: inst,
	}
	if qty != "" {
		o.Quantity = dp(qty)
	}
	return o
}

// wantIncome compares a position's income against an expected list, entry by
// entry, INCLUDING THE ORDER — which is part of what is being tested: the
// entries are ordered by currency code, not by the order the payments appear in
// the journal, so that the same money always renders and compares the same way.
func wantIncome(t *testing.T, p *portfolio.Position, want []portfolio.CurrencyMinor) {
	t.Helper()
	if len(p.IncomeByCurrency) != len(want) {
		t.Fatalf("income = %v, want %v", p.IncomeByCurrency, want)
	}
	for i, e := range want {
		if p.IncomeByCurrency[i] != e {
			t.Errorf("income[%d] = %v, want %v (whole list: %v)", i, p.IncomeByCurrency[i], e, p.IncomeByCurrency)
		}
	}
}

// TestCouponInAnotherCurrencyIsIncomeInThatCurrency is the case the whole
// change exists for: the bond is bought for yuan and pays in rubles. The cost
// stays yuan to the fen, and the coupon appears as rubles — not added to the
// yuan, not converted by the engine (which knows no rates and must not), and
// not refused.
func TestCouponInAnotherCurrencyIsIncomeInThatCurrency(t *testing.T) {
	bond := uuid.New()
	pos, err := portfolio.Compute([]portfolio.Operation{
		opIn(portfolio.TypeBuy, 1, &bond, "CNY", "10", -70_000),
		opIn(portfolio.TypeCoupon, 5, &bond, "RUB", "", 123_456),
	})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[bond]
	if p.Currency != "CNY" {
		t.Errorf("currency = %q, want CNY — the currency the paper was PAID for", p.Currency)
	}
	if p.CostMinor != 70_000 {
		t.Errorf("cost = %d, want 70000 (fen)", p.CostMinor)
	}
	if len(p.Lots) != 1 || p.Lots[0].CostMinor != 70_000 {
		t.Errorf("lots = %v, want one lot of 70000 fen", p.Lots)
	}
	wantIncome(t, p, []portfolio.CurrencyMinor{{Currency: "RUB", Minor: 123_456}})
	if got := p.IncomeMinorIn("RUB"); got != 123_456 {
		t.Errorf("income in RUB = %d, want 123456", got)
	}
	if got := p.IncomeMinorIn("CNY"); got != 0 {
		t.Errorf("income in CNY = %d, want 0 — the coupon arrived in rubles and nothing may quietly restate it in yuan", got)
	}
}

// TestABuyInAnotherCurrencyIsStillRefused is the other half of the rule, and
// the fixture deliberately carries an accepted ruble coupon first: what was
// widened is income alone, and a purchase priced in a second currency is still
// a journal that cannot be folded — its cost would have to be added, fen to
// kopeck, into one int64.
func TestABuyInAnotherCurrencyIsStillRefused(t *testing.T) {
	bond := uuid.New()
	_, err := portfolio.Compute([]portfolio.Operation{
		opIn(portfolio.TypeBuy, 1, &bond, "CNY", "10", -70_000),
		opIn(portfolio.TypeCoupon, 5, &bond, "RUB", "", 123_456),
		opIn(portfolio.TypeBuy, 6, &bond, "RUB", "10", -1_000_000),
	})
	if !errors.Is(err, portfolio.ErrBadOperation) {
		t.Fatalf("err = %v, want ErrBadOperation", err)
	}
	// The refusal has to name both currencies: it is shown to the owner as the
	// reason an imported row could not be parsed (see the importer's
	// ReasonEngineRefused), and "a currency does not match" answers nothing.
	if !strings.Contains(err.Error(), "RUB") || !strings.Contains(err.Error(), "CNY") {
		t.Errorf("refusal %q names neither the currency offered nor the one expected", err)
	}
}

// TestATaxInAThirdCurrencyReducesTheIncomeItWasWithheldFrom pins that a tax
// lands in the currency IT was charged in. Booking it against the position's
// currency, or against whichever currency happened to be first, would overstate
// one income and understate another by the same amount, in silence.
func TestATaxInAThirdCurrencyReducesTheIncomeItWasWithheldFrom(t *testing.T) {
	bond := uuid.New()
	pos, err := portfolio.Compute([]portfolio.Operation{
		opIn(portfolio.TypeBuy, 1, &bond, "CNY", "10", -70_000),
		opIn(portfolio.TypeCoupon, 5, &bond, "RUB", "", 123_456),
		opIn(portfolio.TypeTax, 5, &bond, "RUB", "", -16_049),
		opIn(portfolio.TypeTax, 6, &bond, "USD", "", -500),
	})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[bond]
	wantIncome(t, p, []portfolio.CurrencyMinor{
		{Currency: "RUB", Minor: 107_407},
		{Currency: "USD", Minor: -500},
	})
	if p.CostMinor != 70_000 {
		t.Errorf("cost = %d, want 70000 — a tax is income, never basis", p.CostMinor)
	}
}

// TestAFeeInAnotherCurrencyIsBookedInThatCurrency. A commission is charged in
// the currency the money moved in, which need not be the one the paper is
// priced in: the broker settles the sale of a yuan bond in rubles and charges
// its commission in rubles too.
//
// This used to be a refusal, on the argument that the position's fee total was
// one number in one currency. That was true of the FIGURE, and the figure is a
// list now — so the argument no longer holds, and keeping the refusal would
// have cost a whole sale over a charge of a few rubles.
//
// Both entries are pinned, and the yuan one is what makes the test mean
// anything: a list that simply took whatever it was given would pass a check
// that only looked at the ruble entry.
func TestAFeeInAnotherCurrencyIsBookedInThatCurrency(t *testing.T) {
	bond := uuid.New()
	pos, err := portfolio.Compute([]portfolio.Operation{
		opInFee(portfolio.TypeBuy, 1, &bond, "CNY", "10", -70_000, 70),
		opIn(portfolio.TypeCoupon, 5, &bond, "RUB", "", 123_456),
		opIn(portfolio.TypeFee, 6, &bond, "RUB", "", -300),
	})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[bond]
	want := []portfolio.CurrencyMinor{
		{Currency: "CNY", Minor: 70},
		{Currency: "RUB", Minor: 300},
	}
	if !slices.Equal(p.FeesByCurrency, want) {
		t.Errorf("fees = %v, want %v", p.FeesByCurrency, want)
	}
	// The purchase commission is capitalized into the basis as well, which is
	// what it has always been: the fee list records the charge, it does not
	// replace the cost it became part of.
	if p.CostMinor != 70_070 {
		t.Errorf("cost = %d, want 70070 — the purchase commission is part of the basis", p.CostMinor)
	}
}

// TestIncomeDoesNotSettleThePositionCurrency. A ruble coupon on a yuan bond is
// not a statement that the position is in rubles, and the journal does not
// always open with the purchase: the paper may have been bought before the
// import window, or arrived by transfer. If the first payment settled the
// currency, the yuan purchase that follows it would be refused — the very
// refusal this change removes, merely moved to another journal order.
//
// The two journals below hold the same three operations and differ only in
// which one comes first, and both must produce the same position.
func TestIncomeDoesNotSettleThePositionCurrency(t *testing.T) {
	bond := uuid.New()
	incomeFirst := []portfolio.Operation{
		opIn(portfolio.TypeCoupon, 1, &bond, "RUB", "", 123_456),
		opIn(portfolio.TypeBuy, 2, &bond, "CNY", "10", -70_000),
	}
	pos, err := portfolio.Compute(incomeFirst)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[bond]
	if p.Currency != "CNY" {
		t.Fatalf("currency = %q, want CNY — the purchase settles it, the coupon does not", p.Currency)
	}
	if p.CostMinor != 70_000 {
		t.Errorf("cost = %d, want 70000 (fen)", p.CostMinor)
	}
	wantIncome(t, p, []portfolio.CurrencyMinor{{Currency: "RUB", Minor: 123_456}})
}

// TestAFeeDoesNotSettleThePositionCurrencyEither is the boundary of the rule
// above, stated as a value rather than left to be inferred, and it is the
// answer that CHANGED: a fee used to close what a payment left open, because
// the fee total was one number in one currency. It is a list now, so a ruble
// charge says no more about what the paper is priced in than a ruble coupon
// does — and the yuan purchase that follows is accepted rather than refused for
// having arrived second.
func TestAFeeDoesNotSettleThePositionCurrencyEither(t *testing.T) {
	bond := uuid.New()
	pos, err := portfolio.Compute([]portfolio.Operation{
		opIn(portfolio.TypeFee, 1, &bond, "RUB", "", -300),
		opIn(portfolio.TypeBuy, 2, &bond, "CNY", "10", -70_000),
	})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[bond]
	if p.Currency != "CNY" {
		t.Fatalf("currency = %q, want CNY — the purchase settles it, the charge does not", p.Currency)
	}
	if got := p.FeesMinorIn("RUB"); got != 300 {
		t.Errorf("fees in RUB = %d, want 300", got)
	}
	if got := p.FeesMinorIn("CNY"); got != 0 {
		t.Errorf("fees in CNY = %d, want 0 — nothing was charged in the position's own currency", got)
	}
}

// TestAPositionWithNoCostIsTheSameWhateverOrderItsPayoutsArrived. A paper
// bought before the import window, or one that arrived by transfer, can leave a
// journal holding NOTHING BUT PAYMENTS: no purchase, no transfer leg, no fee —
// nothing that settles the position's currency. Such a position still has to be
// drawn, and what it is drawn under must not depend on which payment the
// journal happens to list first, or two accounts holding the same money would
// render as two different rows.
//
// The two journals below are the same two payments in the two possible orders.
// The dollar dividend is the larger money and arrives later; the ruble tax is
// listed first in one of them. Taking the currency from whichever payment came
// first put a DOLLAR share under a ruble sign in one order and under a dollar
// sign in the other, from the same two facts.
//
// The figures are literals rather than the fixtures' own numbers, and the two
// results are ALSO compared against each other: agreeing on a wrong answer
// would satisfy only half of this test.
func TestAPositionWithNoCostIsTheSameWhateverOrderItsPayoutsArrived(t *testing.T) {
	share := uuid.New()
	taxFirst := []portfolio.Operation{
		opIn(portfolio.TypeTax, 4, &share, "RUB", "", -39_000),
		opIn(portfolio.TypeDividend, 5, &share, "USD", "", 5_000),
	}
	dividendFirst := []portfolio.Operation{
		opIn(portfolio.TypeDividend, 5, &share, "USD", "", 5_000),
		opIn(portfolio.TypeTax, 4, &share, "RUB", "", -39_000),
	}

	var got [2]*portfolio.Position
	for i, ops := range [2][]portfolio.Operation{taxFirst, dividendFirst} {
		pos, err := portfolio.Compute(ops)
		if err != nil {
			t.Fatalf("Compute(journal %d): %v", i, err)
		}
		p := pos[share]
		wantIncome(t, p, []portfolio.CurrencyMinor{
			{Currency: "RUB", Minor: -39_000},
			{Currency: "USD", Minor: 5_000},
		})
		// RUB, and not because a ruble was paid first: it is the lower currency
		// code of the two, which is the order the income itself is kept in. In
		// the second journal the dollar arrives first and the answer is the
		// same one.
		if p.Currency != "RUB" {
			t.Errorf("journal %d: currency = %q, want RUB — chosen by currency code, not by which payment the journal lists first", i, p.Currency)
		}
		// And it is chosen by THAT order rather than by an order of its own —
		// the claim Position.Currency makes about itself: the currency a
		// position with no cost carries is the first entry of its own income.
		// (wantIncome above has already established there is one.)
		if p.Currency != p.IncomeByCurrency[0].Currency {
			t.Errorf("journal %d: currency = %q but income starts at %q — the two orders have drifted apart",
				i, p.Currency, p.IncomeByCurrency[0].Currency)
		}
		// The currency above is a convention for drawing the row, and these
		// four figures are why it can be one: nothing that would have to be
		// denominated in it exists (see Position.Currency).
		if p.CostMinor != 0 || len(p.Lots) != 0 || p.FeesMinorIn(p.Currency) != 0 || len(p.Realizations) != 0 {
			t.Errorf("journal %d: cost %d, %d lots, fees %d, %d realizations — a position with no cost-touching operation must have none of these, or the currency it carries would be a claim about them",
				i, p.CostMinor, len(p.Lots), p.FeesMinorIn(p.Currency), len(p.Realizations))
		}
		got[i] = p
	}

	if !reflect.DeepEqual(got[0], got[1]) {
		t.Errorf("the same two payments in two orders gave two positions:\n%+v\n%+v", got[0], got[1])
	}
}

// TestAFeeSettlesTheCurrencyAPaymentOnlyLentIt is the receiving half of the
// boundary TestAFeeSettlesThePositionCurrencyBeforeAnyPurchase draws — the half
// that must be ACCEPTED, and that the engine refused before income stopped
// settling the currency.
//
// A ruble coupon opens the journal, a yuan commission follows it and a yuan
// purchase follows that. The coupon lends the position a currency it does not
// own: the commission settles it for good, and the purchase then agrees with
// the commission rather than with the payment. Pinning only the refusing order
// would leave the rule half-covered — a change making a payment settle the
// currency again would be caught by nothing here, and it is exactly the change
// that refused 131 of the owner's operations.
func TestAFeeSettlesTheCurrencyAPaymentOnlyLentIt(t *testing.T) {
	bond := uuid.New()
	pos, err := portfolio.Compute([]portfolio.Operation{
		opIn(portfolio.TypeCoupon, 1, &bond, "RUB", "", 123_456),
		opIn(portfolio.TypeFee, 2, &bond, "CNY", "", -300),
		opIn(portfolio.TypeBuy, 3, &bond, "CNY", "10", -70_000),
	})
	if err != nil {
		t.Fatalf("Compute: %v — a ruble coupon must not make a yuan commission and a yuan purchase illegal", err)
	}
	p := pos[bond]
	if p.Currency != "CNY" {
		t.Errorf("currency = %q, want CNY — the commission settled it, the coupon only lent one", p.Currency)
	}
	if p.FeesMinorIn(p.Currency) != 300 {
		t.Errorf("fees = %d, want 300 (fen)", p.FeesMinorIn(p.Currency))
	}
	if p.CostMinor != 70_000 {
		t.Errorf("cost = %d, want 70000 (fen)", p.CostMinor)
	}
	wantIncome(t, p, []portfolio.CurrencyMinor{{Currency: "RUB", Minor: 123_456}})
}

// TestIncomeThatLeavesTheRangeIsRefusedRatherThanWrapped. The payments are each
// an ordinary int64 and their total is not, which is the one shape Go's + turns
// into a plausible-looking figure of the wrong magnitude and the wrong sign
// — here, one kopeck of income where nine quintillion arrived. The write path
// caps a single amount far below this (money.MaxAmountMinor), so a journal
// reaching it is damaged rather than merely large; the guard is on the total
// regardless, because the alternative to a named refusal is a wrong number
// nobody can spot.
func TestIncomeThatLeavesTheRangeIsRefusedRatherThanWrapped(t *testing.T) {
	share := uuid.New()
	_, err := portfolio.Compute([]portfolio.Operation{
		opIn(portfolio.TypeDividend, 1, &share, "RUB", "", math.MaxInt64),
		opIn(portfolio.TypeDividend, 2, &share, "RUB", "", 1),
	})
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("err = %v, want ErrOverflow — maxint64 + 1 kopeck is not a sum of money", err)
	}
	if !strings.Contains(err.Error(), "RUB") {
		t.Errorf("refusal %q does not say which income total left the range", err)
	}
}

// TestIncomeIsOrderedByCurrencyWhateverTheJournalOrder. The order is a property
// of the money, not of the journal: these figures go on a screen and into
// assertions, and an order that depended on which payment arrived first would
// make two identical positions render differently. The journal below is
// deliberately neither ascending nor descending.
func TestIncomeIsOrderedByCurrencyWhateverTheJournalOrder(t *testing.T) {
	share := uuid.New()
	pos, err := portfolio.Compute([]portfolio.Operation{
		opIn(portfolio.TypeBuy, 1, &share, "CNY", "10", -70_000),
		opIn(portfolio.TypeDividend, 2, &share, "USD", "", 300),
		opIn(portfolio.TypeDividend, 3, &share, "CNY", "", 200),
		opIn(portfolio.TypeDividend, 4, &share, "RUB", "", 100),
		opIn(portfolio.TypeDividend, 5, &share, "USD", "", 7),
	})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	wantIncome(t, pos[share], []portfolio.CurrencyMinor{
		{Currency: "CNY", Minor: 200},
		{Currency: "RUB", Minor: 100},
		{Currency: "USD", Minor: 307},
	})
}

// TestASaleSettledInAnotherCurrencyIsRecordedAndLeavesNoSingleFigure is the
// yuan bond redeemed for rubles, which is what the owner's own account holds
// seven of.
//
// The sale is ACCEPTED — nothing about it reaches a figure that holds one
// currency: the proceeds and the fee go to the disposal, which carries its own,
// and what leaves the position is decided by the quantity sold. What it costs
// is the position's own realized figure, which stops existing: rubles received
// less a yuan basis is a quantity of neither, and the engine holds no rate to
// bridge them. Both halves are pinned, because accepting the sale while
// publishing a nonsense difference would be worse than the old refusal.
func TestASaleSettledInAnotherCurrencyIsRecordedAndLeavesNoSingleFigure(t *testing.T) {
	bond := uuid.New()
	pos, err := portfolio.Compute([]portfolio.Operation{
		opIn(portfolio.TypeBuy, 1, &bond, "CNY", "10", -70_000),
		opInFee(portfolio.TypeSell, 9, &bond, "RUB", "10", 1_137_541, 454),
	})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[bond]
	if p.Currency != "CNY" {
		t.Fatalf("currency = %q, want CNY — the purchase settles it and the sale does not", p.Currency)
	}
	if !p.Quantity.IsZero() {
		t.Errorf("quantity = %s, want 0 — the bonds left whatever the money arrived in", p.Quantity)
	}
	if p.CostMinor != 0 {
		t.Errorf("cost = %d, want 0 — the whole basis was released", p.CostMinor)
	}
	if minor, inOne := p.RealizedPnL(); inOne {
		t.Errorf("realized = %d in one currency, want no such figure: %d ₽ against a basis in fen is neither", minor, 1_137_541)
	}
	// The disposal itself keeps everything a rate-holding layer needs.
	if len(p.Realizations) != 1 {
		t.Fatalf("realizations = %d, want 1", len(p.Realizations))
	}
	r := p.Realizations[0]
	if r.Currency != "RUB" || r.ProceedsMinor != 1_137_541 || r.FeeMinor != 454 {
		t.Errorf("realization = %+v, want 1137541 and a fee of 454, both in RUB", r)
	}
	if got := portfolio.LotsCost(r.Released); got != 70_000 {
		t.Errorf("released basis = %d, want 70000 fen — the queue gives up the same parcels whatever the money was", got)
	}
	// And the commission is on the fee list under the currency it was charged
	// in, not folded into a yuan total at par.
	if got := p.FeesMinorIn("RUB"); got != 454 {
		t.Errorf("fees in RUB = %d, want 454", got)
	}
}

// TestOneSaleInAnotherCurrencyWithholdsTheWholeRealizedFigure. A position that
// sold twice — once in its own currency, once in another — has no realized
// figure at all, rather than the first sale's result standing in for both.
//
// A partial sum is the failure this rule exists against: it is a real number of
// the right currency and the wrong size, and nothing on a screen distinguishes
// it from the whole.
func TestOneSaleInAnotherCurrencyWithholdsTheWholeRealizedFigure(t *testing.T) {
	bond := uuid.New()
	pos, err := portfolio.Compute([]portfolio.Operation{
		opIn(portfolio.TypeBuy, 1, &bond, "CNY", "20", -140_000),
		opIn(portfolio.TypeSell, 5, &bond, "CNY", "10", 80_000),
		opIn(portfolio.TypeSell, 9, &bond, "RUB", "10", 1_137_541),
	})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[bond]
	if minor, inOne := p.RealizedPnL(); inOne {
		t.Errorf("realized = %d, want no figure: the yuan sale alone came to 10000 fen and is not the whole result", minor)
	}
}

// TestAnAmortizationInAnotherCurrencyIsStillRefused is the line the change did
// NOT cross, and the reason is in the arithmetic rather than in caution: an
// amortization retires basis BY AMOUNT, so how much of a yuan basis a ruble
// payment took off can only be answered with a rate — and that answer would
// live on in the REMAINING basis, silently changing every later figure for the
// bond. A sale has no such problem because what it retires is the quantity sold.
func TestAnAmortizationInAnotherCurrencyIsStillRefused(t *testing.T) {
	bond := uuid.New()
	_, err := portfolio.Compute([]portfolio.Operation{
		opIn(portfolio.TypeBuy, 1, &bond, "CNY", "10", -70_000),
		opIn(portfolio.TypeAmortization, 9, &bond, "RUB", "", 10_000),
	})
	if !errors.Is(err, portfolio.ErrBadOperation) {
		t.Fatalf("err = %v, want ErrBadOperation — a ruble payment cannot retire a yuan basis by amount", err)
	}
	for _, want := range []string{"RUB", "CNY", bond.String()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

// TestATransferCarryingNoBasisNeedNotMatchTheCurrency is the smallest of the
// three changes and the one with nothing to weigh: the entry moves no money, so
// its currency is a label over no sum at all.
//
// It is the owner's own incoming transfer of 2400 shares, which the broker
// denominated in dollars while the receiving account holds the paper in rubles,
// and which was refused for years over an amount of zero.
func TestATransferCarryingNoBasisNeedNotMatchTheCurrency(t *testing.T) {
	share := uuid.New()
	pos, err := portfolio.Compute([]portfolio.Operation{
		opIn(portfolio.TypeBuy, 1, &share, "RUB", "100", -100_000),
		opIn(portfolio.TypeTransferIn, 5, &share, "USD", "2400", 0),
	})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p := pos[share]
	if p.Currency != "RUB" {
		t.Errorf("currency = %q, want RUB — a costless arrival settles nothing", p.Currency)
	}
	if got := p.Quantity.String(); got != "2500" {
		t.Errorf("quantity = %s, want 2500", got)
	}
	if p.CostMinor != 100_000 {
		t.Errorf("cost = %d, want 100000 — the arrival brought shares and no money", p.CostMinor)
	}
	// The arriving parcel has no acquisition date and no cost, which is the
	// existing rule for a transfer whose basis nobody recorded; the currency
	// exemption changes none of it.
	if len(p.Lots) != 2 || p.Lots[0].AcquiredOn != nil || p.Lots[0].CostMinor != 0 {
		t.Errorf("lots = %+v, want the dateless costless parcel at the head of the queue", p.Lots)
	}
}

// TestATransferCarryingABasisMustStillMatch is the boundary of the rule above,
// pinned so that "moves no money" cannot quietly become "is a transfer".
func TestATransferCarryingABasisMustStillMatch(t *testing.T) {
	share := uuid.New()
	_, err := portfolio.Compute([]portfolio.Operation{
		opIn(portfolio.TypeBuy, 1, &share, "RUB", "100", -100_000),
		opIn(portfolio.TypeTransferIn, 5, &share, "USD", "2400", 1),
	})
	if !errors.Is(err, portfolio.ErrBadOperation) {
		t.Fatalf("err = %v, want ErrBadOperation — one cent of dollar basis is still dollars", err)
	}
}
