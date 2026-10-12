package allocation

import (
	"fmt"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
)

// Buy is what a top-up puts into a kind and the kind's share of the worth
// after it, in percent.
type Buy struct {
	Key        string
	Minor      int64
	ShareAfter decimal.Decimal
}

// TopUp lays new money over the kinds by their targets (variant Б of Р-37):
// to the kinds below their target in the worth with the money in, in
// proportion to what each lacks; what is above its target gets nothing and
// nothing is sold. With targets adding up to 100 what is lacking is never
// less than the money: it is the money plus what the other kinds are above
// their targets. Every kind of the view, in its order.
func TopUp(v View, amount int64) ([]Buy, error) {
	if amount <= 0 {
		return nil, fmt.Errorf("%w: the money to put in is above zero", family.ErrValidation)
	}
	targeted := false
	for _, r := range v.Kinds {
		targeted = targeted || r.Target != nil
	}
	if !targeted {
		return nil, fmt.Errorf("%w: no targets by kind to put the money by", family.ErrValidation)
	}
	after := v.Worth + amount
	lacks := make([]int64, len(v.Kinds))
	for i, r := range v.Kinds {
		if r.Target == nil {
			continue
		}
		want := decimal.NewFromInt(after).Mul(*r.Target).Div(hundred).Round(0).IntPart()
		if gap := want - r.Minor; gap > 0 {
			lacks[i] = gap
		}
	}
	buys := make([]int64, len(v.Kinds))
	share(buys, lacks, amount)
	out := make([]Buy, 0, len(v.Kinds))
	for i, r := range v.Kinds {
		after := decimal.NewFromInt(r.Minor + buys[i]).Mul(hundred).Div(decimal.NewFromInt(after)).Round(2)
		out = append(out, Buy{Key: r.Key, Minor: buys[i], ShareAfter: after})
	}
	return out, nil
}

// share adds amount to into in proportion to weights, the rounding left to
// the heaviest; nothing when the weights are all zero — every kind at or
// above its target, which the targets adding up to 100 rule out.
func share(into, weights []int64, amount int64) {
	var total int64
	heaviest := -1
	for i, w := range weights {
		total += w
		if w > 0 && (heaviest < 0 || w > weights[heaviest]) {
			heaviest = i
		}
	}
	if total == 0 {
		return
	}
	var given int64
	for i, w := range weights {
		part := decimal.NewFromInt(amount).Mul(decimal.NewFromInt(w)).Div(decimal.NewFromInt(total)).Floor().IntPart()
		into[i] += part
		given += part
	}
	into[heaviest] += amount - given
}
