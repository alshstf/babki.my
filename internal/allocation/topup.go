package allocation

import (
	"fmt"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/structure"
)

// Change is what a top-up moves in or out of a kind, and the kind's share of
// the worth after it, in percent.
type Change struct {
	Key        string
	Minor      int64
	ShareAfter decimal.Decimal
}

// TopUp lays money over the kinds by their targets (variant Б of Р-37): new
// money, which adds to the worth, and money the brokers hold free, which only
// moves within it — out of the brokers' free money into the kinds. It goes to
// the kinds below their target in the worth after, in proportion to what each
// lacks; what is above its target gets nothing and nothing is sold. With
// targets adding up to 100 what is lacking is never less than the money: it
// is the money plus what the other kinds are above their targets. Every kind
// of the view, in its order — the brokers' free money with what left it.
func TopUp(v View, fresh, fromBrokers int64) ([]Change, error) {
	if fresh < 0 || fromBrokers < 0 || fresh+fromBrokers == 0 {
		return nil, fmt.Errorf("%w: the money to put in is above zero", family.ErrValidation)
	}
	targeted := false
	held := map[string]int64{}
	for _, r := range v.Kinds {
		targeted = targeted || r.Target != nil
		held[r.Key] = r.Minor
	}
	if !targeted {
		return nil, fmt.Errorf("%w: no targets by kind to put the money by", family.ErrValidation)
	}
	free := string(structure.KindBrokerCash)
	if fromBrokers > held[free] {
		return nil, fmt.Errorf("%w: the brokers hold less free money than that", family.ErrValidation)
	}
	held[free] -= fromBrokers
	after := v.Worth + fresh
	lacks := make([]int64, len(v.Kinds))
	for i, r := range v.Kinds {
		if r.Target == nil {
			continue
		}
		want := decimal.NewFromInt(after).Mul(*r.Target).Div(hundred).Round(0).IntPart()
		if gap := want - held[r.Key]; gap > 0 {
			lacks[i] = gap
		}
	}
	moves := make([]int64, len(v.Kinds))
	share(moves, lacks, fresh+fromBrokers)
	out := make([]Change, 0, len(v.Kinds))
	for i, r := range v.Kinds {
		if r.Key == free {
			moves[i] -= fromBrokers
		}
		shareAfter := decimal.Zero
		if after > 0 {
			shareAfter = decimal.NewFromInt(r.Minor + moves[i]).Mul(hundred).Div(decimal.NewFromInt(after)).Round(2)
		}
		out = append(out, Change{Key: r.Key, Minor: moves[i], ShareAfter: shareAfter})
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
