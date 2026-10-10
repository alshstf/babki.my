// Package benchmark weighs the family's return against total-return indices
// (#401): a model portfolio that put the same money into the index on the
// same days — the family's worth at the start, every contribution and
// withdrawal — and its money-weighted rate beside the family's. Fairer than
// «the index rose so much», as it counts when the money came. It owns no
// table: the indices are market data, the flows the accounts'.
package benchmark

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/money"
	"babki.my/babki/internal/platform/xirr"
)

// Result is one index's model portfolio over the period: what it would be
// worth at the end, in the base currency, and its annual rate; Complete is
// false when the index has no value on a day it needed one, or when the
// family took out more than the model would have held — the model would
// then owe the index, and its rate would say nothing of the family's.
type Result struct {
	Code       string
	Currency   string
	End        int64
	AnnualRate *float64
	Complete   bool
}

type basis interface {
	FamilyReturnBasis(ctx context.Context, spaceID uuid.UUID, base string, from, to time.Time) (account.ReturnBasis, error)
}

type spaces interface {
	SpaceByID(ctx context.Context, id uuid.UUID) (family.Space, error)
}

type indices interface {
	IndexValueOn(ctx context.Context, code string, on time.Time) (decimal.Decimal, bool, error)
}

// Service weighs returns against the benchmarks.
type Service struct {
	basis   basis
	spaces  spaces
	indices indices
	rates   marketdata.RateSource
}

func NewService(b basis, sp spaces, ix indices, rates marketdata.RateSource) *Service {
	return &Service{basis: b, spaces: sp, indices: ix, rates: rates}
}

// Compare is every benchmark's model portfolio over the family's period.
func (s *Service) Compare(ctx context.Context, spaceID uuid.UUID, from, to time.Time) (string, []Result, error) {
	sp, err := s.spaces.SpaceByID(ctx, spaceID)
	if err != nil {
		return "", nil, err
	}
	b, err := s.basis.FamilyReturnBasis(ctx, spaceID, sp.BaseCurrency, from, to)
	if err != nil {
		return "", nil, err
	}
	memo := marketdata.NewRateMemo(s.rates)
	out := make([]Result, 0, len(marketdata.Benchmarks))
	for _, bm := range marketdata.Benchmarks {
		// price is a unit of the index on a day, in the base currency.
		price := func(on time.Time) (decimal.Decimal, bool, error) {
			v, ok, err := s.indices.IndexValueOn(ctx, bm.Code, on)
			if err != nil || !ok || bm.Currency == sp.BaseCurrency {
				return v, ok, err
			}
			res := memo.Rate(ctx, bm.Currency, sp.BaseCurrency, on)
			if errors.Is(res.Err, marketdata.ErrNoRate) {
				return decimal.Zero, false, nil
			}
			if res.Err != nil {
				return decimal.Zero, false, res.Err
			}
			return v.Mul(res.Rate), true, nil
		}
		r, err := model(bm, b, from, to, price)
		if err != nil {
			return "", nil, err
		}
		out = append(out, r)
	}
	return sp.BaseCurrency, out, nil
}

// model puts the period's money into the index: the worth at the start buys
// units at the start, each flow buys (money in) or sells (money out) units on
// its day, and the units are valued at the end.
func model(bm marketdata.Benchmark, b account.ReturnBasis, from, to time.Time,
	price func(time.Time) (decimal.Decimal, bool, error),
) (Result, error) {
	r := Result{Code: bm.Code, Currency: bm.Currency, Complete: true}
	units := decimal.Zero
	buy := func(on time.Time, minor int64) error {
		if minor == 0 {
			return nil
		}
		p, ok, err := price(on)
		if err != nil {
			return err
		}
		if !ok || !p.IsPositive() {
			r.Complete = false
			return nil
		}
		units = units.Add(decimal.NewFromInt(minor).Div(p))
		if units.IsNegative() {
			r.Complete = false
		}
		return nil
	}
	if err := buy(from, b.Start.Minor); err != nil {
		return Result{}, err
	}
	for _, f := range b.Flows {
		// A flow is signed as the family sees it: money put in is negative.
		if err := buy(f.Day, -f.Minor); err != nil {
			return Result{}, err
		}
	}
	end, ok, err := price(to)
	if err != nil {
		return Result{}, err
	}
	if !ok || !r.Complete {
		r.Complete = false
		return r, nil
	}
	if r.End, err = money.Minor(units.Mul(end)); err != nil {
		return Result{}, err
	}
	flows := make([]xirr.Flow, 0, len(b.Flows)+2)
	if b.Start.Minor != 0 {
		flows = append(flows, xirr.Flow{Day: from, Amount: -float(b.Start.Minor)})
	}
	for _, f := range b.Flows {
		flows = append(flows, xirr.Flow{Day: f.Day, Amount: float(f.Minor)})
	}
	flows = append(flows, xirr.Flow{Day: to, Amount: float(r.End)})
	if rate, ok := xirr.Rate(flows); ok {
		r.AnnualRate = &rate
	}
	return r, nil
}

func float(minor int64) float64 {
	f, _ := decimal.NewFromInt(minor).Float64()
	return f
}
