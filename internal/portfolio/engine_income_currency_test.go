package portfolio_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

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
// What did NOT change is the rule for everything that touches cost or quantity:
// a purchase, a sale, either leg of a transfer, a split, an amortization and a
// fee must still repeat the position's currency, because each of them lands in
// a single int64 of minor units and two currencies in one such number is
// corruption no rounding can rescue (see Position.Currency and
// Type.mustMatchPositionCurrency). Both halves are pinned here: the refusals
// below are as much the subject of these tests as the acceptances.
//
// Every figure is written out as a literal rather than derived from the
// fixtures, so a change in how the engine folds income cannot move the expected
// answer with it.
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

// TestAFeeInAnotherCurrencyIsStillRefused. FeesMinor is one int64 in the
// position's currency and is published as such, so a commission charged in
// another one has nowhere to go: it stays a loud refusal rather than being
// folded in at par. The T-Invest importer already keeps such a commission off
// the instrument for exactly this reason (see tinvest.tradeCommission).
func TestAFeeInAnotherCurrencyIsStillRefused(t *testing.T) {
	bond := uuid.New()
	_, err := portfolio.Compute([]portfolio.Operation{
		opIn(portfolio.TypeBuy, 1, &bond, "CNY", "10", -70_000),
		opIn(portfolio.TypeCoupon, 5, &bond, "RUB", "", 123_456),
		opIn(portfolio.TypeFee, 6, &bond, "RUB", "", -300),
	})
	if !errors.Is(err, portfolio.ErrBadOperation) {
		t.Fatalf("err = %v, want ErrBadOperation — a fee in a second currency has nowhere to go", err)
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

// TestAFeeSettlesThePositionCurrencyBeforeAnyPurchase is the boundary of the
// rule above, stated as a value rather than left to be inferred: what a payment
// leaves open, a fee closes. FeesMinor is in the position's currency, so the
// moment a fee has been booked there is a figure a later purchase in another
// currency would contradict — and the refusal is the honest answer, the same
// one TestAFeeInAnotherCurrencyIsStillRefused gets from the other order.
func TestAFeeSettlesThePositionCurrencyBeforeAnyPurchase(t *testing.T) {
	bond := uuid.New()
	_, err := portfolio.Compute([]portfolio.Operation{
		opIn(portfolio.TypeFee, 1, &bond, "RUB", "", -300),
		opIn(portfolio.TypeBuy, 2, &bond, "CNY", "10", -70_000),
	})
	if !errors.Is(err, portfolio.ErrBadOperation) {
		t.Fatalf("err = %v, want ErrBadOperation", err)
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
