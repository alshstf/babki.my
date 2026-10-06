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

// opInFee is opIn with a commission on the entry.
//
// These tests pin that a position's cost and income need not share a currency
// (a yuan bond paying rouble coupons: 131 of the owner's operations were once
// refused for it), and where the rule still holds: purchases, transfers with a
// basis and amortizations must match; fees and sales need not. Figures are
// literals.
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

// wantIncome compares income entry by entry, including the order (by currency
// code, not journal order).
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

// A yuan bond's rouble coupon is rouble income; the cost stays yuan.
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

// A purchase in another currency is still refused, after an accepted rouble
// coupon.
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
	// The refusal names both currencies: owners see it as an unparsed row's
	// reason.
	if !strings.Contains(err.Error(), "RUB") || !strings.Contains(err.Error(), "CNY") {
		t.Errorf("refusal %q names neither the currency offered nor the one expected", err)
	}
}

// A tax lands in the currency it was charged in.
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

// A commission is booked in its own currency (a rouble fee on a yuan bond's
// sale); both list entries are checked.
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
	// The purchase commission is also in the basis; the fee list records the
	// charge.
	if p.CostMinor != 70_070 {
		t.Errorf("cost = %d, want 70070 — the purchase commission is part of the basis", p.CostMinor)
	}
}

// Income does not settle the position's currency: the same three operations in
// either order give the same position.
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

// A fee does not settle it either: the yuan purchase after a rouble fee is
// accepted.
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

// A cost-free, payments-only position is drawn the same whatever the order of
// its payments; both results are checked against literals and each other.
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
		// RUB is the lower currency code, not the first payment's.
		if p.Currency != "RUB" {
			t.Errorf("journal %d: currency = %q, want RUB — chosen by currency code, not by which payment the journal lists first", i, p.Currency)
		}
		// The currency is the income list's first entry, as Position.Currency
		// claims.
		if p.Currency != p.IncomeByCurrency[0].Currency {
			t.Errorf("journal %d: currency = %q but income starts at %q — the two orders have drifted apart",
				i, p.Currency, p.IncomeByCurrency[0].Currency)
		}
		// Nothing denominated in that currency exists, which is why it can be a
		// label.
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

// A rouble coupon, then a yuan commission, then a yuan purchase: the
// commission settles the currency and the purchase agrees with it.
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
	// The commission was charged outside a trade, so it also comes off the income.
	wantIncome(t, p, []portfolio.CurrencyMinor{{Currency: "CNY", Minor: -300}, {Currency: "RUB", Minor: 123_456}})
}

// Payments whose total leaves int64 are refused rather than wrapped (the write
// side caps single amounts far lower, so this journal is damaged).
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

// Income is ordered by currency whatever the journal order.
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

// A yuan bond redeemed for roubles is accepted and leaves no single realized
// figure: roubles less a yuan basis is a quantity of neither.
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
	// The commission is on the fee list in roubles.
	if got := p.FeesMinorIn("RUB"); got != 454 {
		t.Errorf("fees in RUB = %d, want 454", got)
	}
}

// One sale in another currency withholds the whole realized figure rather than
// publishing a partial sum.
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

// An amortization in another currency is still refused: it retires basis by
// amount, which would need a rate.
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

// A transfer carrying no basis need not match the currency (the owner's 2 400
// shares denominated in dollars, refused for years over zero).
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
	// The parcel has no date and no cost, as for any transfer without a recorded
	// basis.
	if len(p.Lots) != 2 || p.Lots[0].AcquiredOn != nil || p.Lots[0].CostMinor != 0 {
		t.Errorf("lots = %+v, want the dateless costless parcel at the head of the queue", p.Lots)
	}
}

// A transfer carrying a basis must still match.
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
