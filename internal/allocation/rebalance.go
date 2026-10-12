package allocation

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/portfolio"
	"babki.my/babki/internal/structure"
)

// Move is what bringing the worth to the targets by buying and selling does
// to a kind (variant В of Р-37). Minor is bought (positive) or sold
// (negative) now. Later is what of the deposits above their target to take
// out as they end — breaking a deposit early loses its interest. Kept is
// what is above the target but cannot be sold now (a frozen paper valued in
// full). ShareAfter is the kind's share once done, in percent.
type Move struct {
	Key        string
	Minor      int64
	Later      int64
	Kept       int64
	ShareAfter decimal.Decimal
}

// Rebalance brings the kinds to their targets in the worth with fresh new
// money: each kind above its target sells down to it, as far as it can be
// sold now — deposits only as they end, frozen papers not at all — and the
// money from the sales with the new money buys the kinds below their target,
// in proportion to what each lacks; all of it when nothing is held back. A
// kind with no target is neither sold nor bought. Every kind of the view, in
// its order.
func Rebalance(v View, fresh int64) ([]Move, error) {
	if fresh < 0 {
		return nil, fmt.Errorf("%w: new money is not below zero", family.ErrValidation)
	}
	targeted := false
	for _, r := range v.Kinds {
		targeted = targeted || r.Target != nil
	}
	if !targeted {
		return nil, fmt.Errorf("%w: no targets by kind to bring the worth to", family.ErrValidation)
	}
	after := v.Worth + fresh
	moves := make([]Move, len(v.Kinds))
	lacks := make([]int64, len(v.Kinds))
	money := fresh
	for i, r := range v.Kinds {
		moves[i].Key = r.Key
		if r.Target == nil {
			continue
		}
		want := decimal.NewFromInt(after).Mul(*r.Target).Div(hundred).Round(0).IntPart()
		gap := want - r.Minor
		switch {
		case gap > 0:
			lacks[i] = gap
		case gap < 0 && r.Key == string(structure.KindDeposits):
			moves[i].Later = -gap
		case gap < 0:
			sold := min(-gap, r.Minor-v.Unsellable[r.Key])
			moves[i].Minor, moves[i].Kept = -sold, -gap-sold
			money += sold
		}
	}
	// What the kinds lack is the money plus all that is held back, so the
	// money never exceeds it; it falls short by what is held back.
	bought := make([]int64, len(v.Kinds))
	share(bought, lacks, money)
	for i, r := range v.Kinds {
		moves[i].Minor += bought[i]
		moves[i].ShareAfter = decimal.Zero
		if after > 0 {
			moves[i].ShareAfter = decimal.NewFromInt(r.Minor + moves[i].Minor).Mul(hundred).Div(decimal.NewFromInt(after)).Round(2)
		}
	}
	return moves, nil
}

// Sale is what selling a kind's papers is estimated to gain over their cost,
// in the base currency: Gain all of it, signed; Free the part of a positive
// gain on lots held long enough to be free of tax. Known is false when a
// lot's cost cannot be struck in the base currency — then nothing is
// claimed.
type Sale struct {
	Gain, Free int64
	Known      bool
}

// Estimate is what selling as a rebalance says would cost in tax (variant В
// of Р-37): per kind sold, its Sale; Tax at the rules' first rate on the gain
// not freed, offsetting gains and losses across the kinds — nil when a sale
// is not Known or the rules estimate none. Not a broker's figure: which lots
// a broker sells depends on the order, the year's other sales and the higher
// band are left out, commissions too.
type Estimate struct {
	Sales map[string]Sale
	Tax   *int64
}

// paperKinds are the kinds whose sales the estimate counts. Currency and
// metals held as money have their own rules, which it does not attempt.
var paperKinds = map[structure.Kind]bool{structure.KindShares: true, structure.KindBonds: true, structure.KindFunds: true}

// Estimating sells within each paper kind every paper that can be sold now
// in proportion to what it is worth, and of each the earliest lots first —
// the queue a broker keeps.
func Estimating(moves []Move, held []portfolio.SaleLots, rules family.TaxRules, today time.Time) Estimate {
	out := Estimate{Sales: map[string]Sale{}}
	byKind := map[string][]portfolio.SaleLots{}
	worth := map[string]int64{}
	for _, h := range held {
		k := structure.PaperKind(h.Type)
		if !paperKinds[k] || h.LiquidInBase == nil || *h.LiquidInBase <= 0 {
			continue
		}
		byKind[string(k)] = append(byKind[string(k)], h)
		worth[string(k)] += *h.LiquidInBase
	}
	known, taxable := true, decimal.Zero
	longAgo := today.AddDate(-rules.LongHoldingYears, 0, 0)
	for _, m := range moves {
		if m.Minor >= 0 || !paperKinds[structure.Kind(m.Key)] || worth[m.Key] == 0 {
			continue
		}
		part := decimal.NewFromInt(-m.Minor).Div(decimal.NewFromInt(worth[m.Key]))
		gain, free, ok := decimal.Zero, decimal.Zero, true
		for _, h := range byKind[m.Key] {
			left := h.Quantity.Mul(part)
			perUnit := decimal.NewFromInt(*h.LiquidInBase).Div(h.Quantity)
			for _, l := range h.Lots {
				if !left.IsPositive() {
					break
				}
				take := decimal.Min(left, l.Quantity)
				left = left.Sub(take)
				if !l.CostKnown {
					ok = false
					continue
				}
				cost := decimal.Zero
				if l.Quantity.IsPositive() {
					cost = decimal.NewFromInt(l.CostInBase).Mul(take).Div(l.Quantity)
				}
				g := perUnit.Mul(take).Sub(cost)
				gain = gain.Add(g)
				// A gain on a lot held long enough is free; a loss still offsets.
				if g.IsPositive() && rules.LongHoldingYears > 0 && l.AcquiredOn != nil && l.AcquiredOn.Before(longAgo) {
					free = free.Add(g)
					continue
				}
				taxable = taxable.Add(g)
			}
		}
		sale := Sale{Known: ok}
		if ok {
			sale.Gain, sale.Free = gain.Round(0).IntPart(), free.Round(0).IntPart()
		}
		out.Sales[m.Key] = sale
		known = known && ok
	}
	if known && rules.SaleTaxBP > 0 && len(out.Sales) > 0 {
		tax := decimal.Max(taxable, decimal.Zero).Mul(decimal.NewFromInt(int64(rules.SaleTaxBP))).Div(decimal.NewFromInt(10000)).Round(0).IntPart()
		out.Tax = &tax
	}
	return out
}
