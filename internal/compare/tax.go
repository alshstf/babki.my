package compare

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/interest"
	"babki.my/babki/internal/taxyear"
)

// freeBase is what the year's highest key rate is applied to for the
// interest free of tax: 1 млн roubles (НК РФ ст. 214.2).
const freeBase = 1_000_000_00

// taxContext is the person's year as the tax on interest sees it: their
// interest this year with what is still to come, their other investment
// incomes, this year's free part, and later years' — 1 млн × the scenario's
// key. Later years take this year's other incomes: there is no forecast of
// them.
type taxContext struct {
	person   string
	known    bool
	rules    family.TaxRules
	year     int
	other    int64
	invested int64
	freeNow  int64
	// freeIn is a year's free part by the scenario's key rate.
	freeIn func(year int) int64
}

// taxOf is the person's tax context; known is false without the year's
// estimate or its key rate.
func (s *Service) taxOf(ctx context.Context, spaceID, person uuid.UUID, today time.Time, key func(year int) decimal.Decimal, rules family.TaxRules) (taxContext, error) {
	out := taxContext{rules: rules, year: today.Year()}
	out.freeIn = func(year int) int64 {
		return key(year).Mul(decimal.NewFromInt(freeBase)).Div(decimal.NewFromInt(100)).Round(0).IntPart()
	}
	if s.members != nil {
		members, err := s.members.ListMembers(ctx, spaceID)
		if err != nil {
			return out, err
		}
		for _, m := range members {
			if m.ID == person {
				out.person = m.DisplayName
			}
		}
	}
	if s.taxes == nil || rules.TaxCurrency == "" {
		return out, nil
	}
	y, err := s.taxes.Of(ctx, spaceID, today.Year())
	if errors.Is(err, taxyear.ErrNoEstimate) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if y.KeyRate == nil {
		return out, nil
	}
	out.known = true
	out.freeNow = y.KeyRate.Mul(decimal.NewFromInt(freeBase)).Div(decimal.NewFromInt(100)).Round(0).IntPart()
	for _, p := range y.People {
		if p.UserID != person {
			continue
		}
		out.other = p.Interest + p.InterestEstimated + p.InterestAhead
		out.invested = p.Base - max(p.Interest+p.InterestEstimated-out.freeNow, 0)
	}
	return out, nil
}

// added is the tax the payments add to the person's: for each year they
// fall in, the tax on the person's interest with them less the tax without,
// the other investment incomes under it; ok is false when not known.
func (t taxContext) added(payments []interest.Payment) (int64, bool) {
	if !t.known {
		return 0, false
	}
	byYear := map[int]int64{}
	for _, p := range payments {
		byYear[p.On.Year()] += p.Minor
	}
	var out int64
	for year, x := range byYear {
		free := t.freeIn(year)
		if year == t.year {
			free = max(t.freeNow, free)
		}
		band := func(interest int64) int64 {
			return family.Banded(t.rules.InvestmentBands[:], t.invested+max(interest-free, 0), t.rules.TaxUnitMinor)
		}
		out += band(t.other+x) - band(t.other)
	}
	return out, true
}
