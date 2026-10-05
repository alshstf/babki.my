package portfolio_test

import (
	"testing"

	"babki.my/babki/internal/portfolio"
)

// withFace puts the face value per unit before the repayment on an
// amortization, as the importer does from the exchange's schedule.
func withFace(o portfolio.Operation, minor int64) portfolio.Operation {
	o.FaceBeforeMinor = &minor
	return o
}

// Decision Р-4, on the memo's own example: ten bonds of 1 000 ₽ bought at 950 ₽
// (9 500 ₽), repaid 20 % of the face in one year, 30 % in the next, the last
// 50 % at maturity. As the tax code does it, each repayment retires the share
// of the basis it is of the outstanding principal, and the result comes year
// by year — +100, +150, +250 ₽ — rather than all at the end.
func TestAmortizationRetiresBasisInProportionToThePrincipal(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &ofz, "10", "950", -950_000, 0),
		withFace(op(portfolio.TypeAmortization, 10, &ofz, "", "", 200_000, 0), 100_000),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if got := pos[ofz].CostMinor; got != 760_000 {
		t.Errorf("after the first repayment the cost is %d, want 760000 — 9 500 ₽ less 20 %%", got)
	}
	if got := realizedOf(t, pos[ofz]); got != 10_000 {
		t.Errorf("the first repayment's result is %d, want 10000 (+100 ₽)", got)
	}

	// The face is 800 ₽ now; 300 ₽ a bond is 37,5 % of what is left.
	ops = append(ops, withFace(op(portfolio.TypeAmortization, 11, &ofz, "", "", 300_000, 0), 80_000))
	pos, err = portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute 2: %v", err)
	}
	if got := pos[ofz].CostMinor; got != 475_000 {
		t.Errorf("after the second repayment the cost is %d, want 475000", got)
	}
	if got := realizedOf(t, pos[ofz]); got != 25_000 {
		t.Errorf("the two repayments' result is %d, want 25000 (+100 + +150 ₽)", got)
	}

	// The rest at maturity: a redemption of the ten bonds for 5 000 ₽.
	ops = append(ops, op(portfolio.TypeRedemption, 12, &ofz, "10", "500", 500_000, 0))
	pos, err = portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute 3: %v", err)
	}
	if got := realizedOf(t, pos[ofz]); got != 50_000 {
		t.Errorf("the bond's whole result is %d, want 50000 — the same +500 ₽ either rule gives over its life", got)
	}
}

// Without a face value the old rule stands: the repayment retires basis until
// none is left, and the result waits for the end.
func TestAmortizationWithoutAFaceValueKeepsTheOldRule(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &ofz, "10", "950", -950_000, 0),
		op(portfolio.TypeAmortization, 10, &ofz, "", "", 200_000, 0),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if pos[ofz].CostMinor != 750_000 || realizedOf(t, pos[ofz]) != 0 {
		t.Errorf("cost=%d realized=%d, want 750000/0", pos[ofz].CostMinor, realizedOf(t, pos[ofz]))
	}
}

// Every parcel gives up the same share, dated as it was bought: two purchases at
// different prices each lose 20 % of their own cost.
func TestAmortizationTakesTheShareFromEveryParcel(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &ofz, "5", "900", -450_000, 0),
		op(portfolio.TypeBuy, 2, &ofz, "5", "1000", -500_000, 0),
		withFace(op(portfolio.TypeAmortization, 10, &ofz, "", "", 200_000, 0), 100_000),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	lots := pos[ofz].Lots
	if len(lots) != 2 || lots[0].CostMinor != 360_000 || lots[1].CostMinor != 400_000 {
		t.Errorf("parcels after the repayment = %+v, want 360000 and 400000", lots)
	}
}

// A repayment larger than the principal its face value says is outstanding
// (a stale schedule, a premium) retires the whole basis and no more: no
// parcel is left owing a negative cost.
func TestAmortizationRetiresNoMoreThanTheWholeBasis(t *testing.T) {
	ops := []portfolio.Operation{
		op(portfolio.TypeBuy, 1, &ofz, "10", "950", -950_000, 0),
		withFace(op(portfolio.TypeAmortization, 10, &ofz, "", "", 1_500_000, 0), 100_000),
	}
	pos, err := portfolio.Compute(ops)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if pos[ofz].CostMinor != 0 || realizedOf(t, pos[ofz]) != 550_000 {
		t.Errorf("cost=%d realized=%d, want 0/550000", pos[ofz].CostMinor, realizedOf(t, pos[ofz]))
	}
	for _, lot := range pos[ofz].Lots {
		if lot.CostMinor < 0 {
			t.Errorf("a parcel owes %d", lot.CostMinor)
		}
	}
}

// The basis a repayment retires keeps the day its purchase settled (decision
// Р-3): it is priced in another currency at that day's rate, as a sale's
// released basis is.
func TestAmortizationRetiresBasisWithItsSettlementDay(t *testing.T) {
	buy := op(portfolio.TypeBuy, 1, &ofz, "10", "950", -950_000, 0)
	settled := buy.OccurredOn.AddDate(0, 0, 1)
	buy.SettledOn = &settled
	pos, err := portfolio.Compute([]portfolio.Operation{
		buy,
		withFace(op(portfolio.TypeAmortization, 10, &ofz, "", "", 200_000, 0), 100_000),
	})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	r := pos[ofz].Realizations
	if len(r) != 1 || len(r[0].Released) != 1 {
		t.Fatalf("realizations = %+v, want one repayment retiring one piece", r)
	}
	if got := r[0].Released[0].RateOn; got == nil || !got.Equal(settled) {
		t.Errorf("the retired piece is priced on %v, want the purchase's settlement day %s", got, settled.Format("2006-01-02"))
	}
}
