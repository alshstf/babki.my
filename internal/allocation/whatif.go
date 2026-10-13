package allocation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/portfolio"
	"babki.my/babki/internal/structure"
)

// OpKind is what a planned operation does (decision Р-48).
type OpKind string

const (
	OpBuy      OpKind = "buy"
	OpSell     OpKind = "sell"
	OpDeposit  OpKind = "deposit"
	OpWithdraw OpKind = "withdraw"
	OpTransfer OpKind = "transfer"
)

// Op is one planned operation: Minor of Currency bought or sold of the
// paper, put in, taken out or moved to the other account.
type Op struct {
	Kind       OpKind
	Account    uuid.UUID
	To         *uuid.UUID
	Instrument *uuid.UUID
	Currency   string
	Minor      int64
}

// Verdict is whether a plan brings a part of the worth closer to its target
// or ceiling, further from it, leaves it where it is, or there is none.
type Verdict string

const (
	Closer  Verdict = "closer"
	Further Verdict = "further"
	Same    Verdict = "same"
	NoGoal  Verdict = "none"
)

// CutRow is a key's part before and after a plan, in the base currency and as
// shares of its base — the worth for kinds and currencies, the papers for
// countries and sectors — against its target or ceiling.
type CutRow struct {
	Key                     string
	Before, After           int64
	ShareBefore, ShareAfter decimal.Decimal
	Target                  *decimal.Decimal
	Verdict                 Verdict
}

// Cut is a dimension's rows.
type Cut struct {
	Dimension Dimension
	Rows      []CutRow
}

// PaperSale is what selling part of a paper is estimated to gain.
type PaperSale struct {
	Instrument uuid.UUID
	Sale
}

// WhatIf is a plan laid against the targets and ceilings: every cut, the
// sales' gains and their tax — nil when a sale's gain is not known or the
// rules estimate none.
type WhatIf struct {
	BaseCurrency string
	Cuts         []Cut
	Sales        []PaperSale
	Tax          *int64
	TaxRate      *decimal.Decimal
	MissingRates []string
}

type catalog interface {
	ByID(ctx context.Context, id uuid.UUID) (instrument.Instrument, error)
}

// WithCatalog lets the service lay planned operations: the papers' kinds from
// the catalog, sums in other currencies at today's rate.
func (s *Service) WithCatalog(c catalog, rates marketdata.RateSource) *Service {
	s.catalog, s.rates = c, rates
	return s
}

var errPlan = fmt.Errorf("%w: an operation is buy, sell, deposit, withdraw or transfer of a positive sum in a currency; buying and selling name a share, bond or fund, a transfer the other account", family.ErrValidation)

// WhatIf lays the plan on the worth and sets it, before and after, against
// the targets and ceilings; nothing is written.
func (s *Service) WhatIf(ctx context.Context, spaceID uuid.UUID, valuation structure.Valuation, plan []Op) (WhatIf, error) {
	if s.catalog == nil || s.rates == nil {
		return WhatIf{}, errors.New("allocation: what-if is not set up")
	}
	if len(plan) == 0 {
		return WhatIf{}, errPlan
	}
	var changes []structure.Change
	sold := map[uuid.UUID]map[uuid.UUID]int64{} // account → paper → minor of currency sold
	soldCurrency := map[uuid.UUID]string{}
	for _, op := range plan {
		if op.Minor <= 0 || !currencyCode.MatchString(op.Currency) {
			return WhatIf{}, errPlan
		}
		money := func(acc uuid.UUID, minor int64) structure.Change {
			return structure.Change{Account: acc, Currency: op.Currency, Minor: minor}
		}
		switch op.Kind {
		case OpBuy, OpSell:
			if op.Instrument == nil {
				return WhatIf{}, errPlan
			}
			paper, err := s.catalog.ByID(ctx, *op.Instrument)
			if err != nil {
				return WhatIf{}, err
			}
			if !slices.Contains([]instrument.Type{instrument.TypeShare, instrument.TypeBond, instrument.TypeETF}, paper.Type) {
				return WhatIf{}, errPlan
			}
			sign := int64(1)
			if op.Kind == OpSell {
				sign = -1
				if sold[op.Account] == nil {
					sold[op.Account] = map[uuid.UUID]int64{}
				}
				sold[op.Account][paper.ID] += op.Minor
				soldCurrency[paper.ID] = op.Currency
			}
			id := paper.ID
			changes = append(changes,
				structure.Change{Account: op.Account, Paper: &id, Type: paper.Type, ISIN: paper.ISIN, Currency: op.Currency, Minor: sign * op.Minor},
				money(op.Account, -sign*op.Minor))
		case OpDeposit:
			changes = append(changes, money(op.Account, op.Minor))
		case OpWithdraw:
			changes = append(changes, money(op.Account, -op.Minor))
		case OpTransfer:
			if op.To == nil || *op.To == op.Account {
				return WhatIf{}, errPlan
			}
			changes = append(changes, money(op.Account, -op.Minor), money(*op.To, op.Minor))
		default:
			return WhatIf{}, errPlan
		}
	}
	before, err := s.structures.Of(ctx, spaceID, valuation)
	if err != nil {
		return WhatIf{}, err
	}
	after, err := s.structures.OfWith(ctx, spaceID, valuation, changes)
	if err != nil {
		return WhatIf{}, err
	}
	st, err := s.Settings(ctx, spaceID)
	if err != nil {
		return WhatIf{}, err
	}
	vb := withCeilings(view(before, st.Mode, st.Kinds, st.Currencies), before, st)
	va := withCeilings(view(after, st.Mode, st.Kinds, st.Currencies), after, st)
	out := WhatIf{
		BaseCurrency: before.BaseCurrency, MissingRates: after.MissingRates,
		Cuts: []Cut{
			{ByKind, cut(vb.Kinds, va.Kinds, false)},
			{ByCurrency, cut(vb.Currencies, va.Currencies, false)},
			{ByCountry, cut(vb.Countries, va.Countries, true)},
			{BySector, cut(vb.Sectors, va.Sectors, true)},
			{ByBroker, cut(vb.Brokers, va.Brokers, true)},
			{ByBank, cut(vb.Banks, va.Banks, true)},
		},
	}
	if len(sold) > 0 {
		if err := s.salesOf(ctx, spaceID, before.BaseCurrency, sold, soldCurrency, &out); err != nil {
			return WhatIf{}, err
		}
	}
	return out, nil
}

// cut sets a dimension's rows before and after against each other; ceilings
// count only what is above them.
func cut(before, after []Row, ceilings bool) []CutRow {
	byKey := map[string]Row{}
	for _, r := range after {
		byKey[r.Key] = r
	}
	out := []CutRow{}
	seen := map[string]bool{}
	add := func(b, a Row) {
		row := CutRow{Key: b.Key, Before: b.Minor, After: a.Minor, ShareBefore: b.Share, ShareAfter: a.Share, Target: b.Target, Verdict: NoGoal}
		if row.Key == "" {
			row.Key = a.Key
		}
		if row.Target == nil {
			row.Target = a.Target
		}
		if row.Target != nil {
			row.Verdict = verdict(row.ShareBefore, row.ShareAfter, *row.Target, ceilings)
		}
		out = append(out, row)
	}
	for _, b := range before {
		seen[b.Key] = true
		add(b, byKey[b.Key])
	}
	for _, a := range after {
		if !seen[a.Key] {
			add(Row{Key: a.Key, Share: decimal.Zero}, a)
		}
	}
	return out
}

// verdict compares the distances from the target — for a ceiling, how far
// above it — before and after; a hundredth of a point is no move.
func verdict(before, after, target decimal.Decimal, ceiling bool) Verdict {
	dist := func(share decimal.Decimal) decimal.Decimal {
		if ceiling {
			return decimal.Max(share.Sub(target), decimal.Zero)
		}
		return share.Sub(target).Abs()
	}
	d := dist(after).Sub(dist(before))
	switch {
	case d.Abs().LessThan(decimal.New(1, -2)):
		return Same
	case d.IsNegative():
		return Closer
	}
	return Further
}

// salesOf estimates what the plan's sales gain and the tax on it: each sold
// paper's part of its holding on the account, the earliest lots first.
func (s *Service) salesOf(ctx context.Context, spaceID uuid.UUID, base string, sold map[uuid.UUID]map[uuid.UUID]int64, currency map[uuid.UUID]string, out *WhatIf) error {
	if s.sales == nil || s.spaces == nil {
		return nil
	}
	sp, err := s.spaces.SpaceByID(ctx, spaceID)
	if err != nil {
		return err
	}
	rules := sp.CostBasisRules()
	today := time.Now().UTC()
	longAgo := today.AddDate(-rules.LongHoldingYears, 0, 0)
	memo := marketdata.NewRateMemo(s.rates)
	known, taxable := true, decimal.Zero
	for acc, papers := range sold {
		held, err := s.sales.ForSale(ctx, spaceID, acc)
		if err != nil {
			return err
		}
		for paper, minor := range papers {
			sale := PaperSale{Instrument: paper}
			i := slices.IndexFunc(held, func(h portfolio.SaleLots) bool { return h.InstrumentID == paper })
			inBase := decimal.NewFromInt(minor)
			if c := currency[paper]; c != base {
				res := memo.Rate(ctx, c, base, today)
				if res.Err != nil {
					inBase = decimal.Zero
					i = -1
				} else {
					inBase = inBase.Mul(res.Rate)
				}
			}
			if i >= 0 && held[i].LiquidInBase != nil && *held[i].LiquidInBase > 0 {
				part := decimal.Min(inBase.Div(decimal.NewFromInt(*held[i].LiquidInBase)), decimal.NewFromInt(1))
				g, f, t, ok := lotsGain(held[i], part, rules, longAgo)
				sale.Sale = Sale{Gain: g.Round(0).IntPart(), Free: f.Round(0).IntPart(), Known: ok}
				taxable = taxable.Add(t)
				known = known && ok
			} else {
				known = false
			}
			out.Sales = append(out.Sales, sale)
		}
	}
	if known && rules.SaleTaxBP > 0 {
		tax := decimal.Max(taxable, decimal.Zero).Mul(decimal.NewFromInt(int64(rules.SaleTaxBP))).Div(decimal.NewFromInt(10000)).Round(0).IntPart()
		rate := decimal.NewFromInt(int64(rules.SaleTaxBP)).Div(hundred)
		out.Tax, out.TaxRate = &tax, &rate
	}
	return nil
}
