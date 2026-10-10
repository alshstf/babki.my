package cashflow

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/money"
)

// MaxMonths is the longest period a report covers: ten years by month.
const MaxMonths = 120

type journals interface {
	ListMoneyFlows(ctx context.Context, spaceID uuid.UUID, from, to time.Time) ([]operation.Operation, error)
}

type accounts interface {
	ListWithBalance(ctx context.Context, spaceID uuid.UUID) ([]account.WithBalance, error)
}

type categories interface {
	List(ctx context.Context, spaceID uuid.UUID) ([]category.Category, error)
}

type spaces interface {
	SpaceByID(ctx context.Context, id uuid.UUID) (family.Space, error)
}

// Service builds reports.
type Service struct {
	journal    journals
	accounts   accounts
	categories categories
	spaces     spaces
	rates      marketdata.RateSource
}

func NewService(j journals, acc accounts, cats categories, sp spaces, rates marketdata.RateSource) *Service {
	return &Service{journal: j, accounts: acc, categories: cats, spaces: sp, rates: rates}
}

// Whose narrows a report to one member's money (UserID) or to the family's
// shared money (Shared); the zero value is the whole family.
type Whose struct {
	UserID *uuid.UUID
	Shared bool
}

// covers reports whether a row on account a is the report's: a row naming a
// member (operation.Operation.MemberID) is that member's, any other the
// account owner's — the family's on a shared account.
func (w Whose) covers(op operation.Operation, a account.Account) bool {
	owner := a.OwnerUserID
	if op.MemberID != nil {
		owner = op.MemberID
	}
	switch {
	case w.Shared:
		return owner == nil
	case w.UserID != nil:
		return owner != nil && *owner == *w.UserID
	}
	return true
}

// Report is the money of spaceID between from and to, both days included.
func (s *Service) Report(ctx context.Context, spaceID uuid.UUID, from, to time.Time, whose Whose) (Report, error) {
	if to.Before(from) {
		return Report{}, fmt.Errorf("%w: the period ends before it starts", family.ErrValidation)
	}
	r := Report{From: from, To: to, Months: months(from, to), MissingRates: []string{}}
	if len(r.Months) > MaxMonths {
		return Report{}, fmt.Errorf("%w: a report covers at most %d months", family.ErrValidation, MaxMonths)
	}
	sp, err := s.spaces.SpaceByID(ctx, spaceID)
	if err != nil {
		return Report{}, err
	}
	r.BaseCurrency = sp.BaseCurrency

	list, err := s.accounts.ListWithBalance(ctx, spaceID)
	if err != nil {
		return Report{}, err
	}
	byID := map[uuid.UUID]account.Account{}
	for _, a := range list {
		byID[a.ID] = a.Account
	}
	cats, err := s.categories.List(ctx, spaceID)
	if err != nil {
		return Report{}, err
	}
	all, err := s.journal.ListMoneyFlows(ctx, spaceID, from, to)
	if err != nil {
		return Report{}, err
	}
	var ops []operation.Operation
	for _, op := range all {
		if a, ok := byID[op.AccountID]; ok && whose.covers(op, a) {
			ops = append(ops, op)
		}
	}

	entries, err := s.convert(ctx, &r, ops, byID)
	if err != nil {
		return Report{}, err
	}
	assemble(&r, entries, byID, cats)
	return r, nil
}

// convert puts each covered row's money into the base currency at its day's
// rate, all rates asked in one round trip. A row whose day has no rate is left
// out and its currency named.
func (s *Service) convert(ctx context.Context, r *Report, ops []operation.Operation, covered map[uuid.UUID]account.Account) ([]entry, error) {
	memo := marketdata.NewRateMemo(s.rates)
	var queries []marketdata.RateQuery
	for _, op := range ops {
		if _, ok := covered[op.AccountID]; ok && op.Currency != r.BaseCurrency {
			queries = append(queries, marketdata.RateQuery{From: op.Currency, To: r.BaseCurrency, On: op.OccurredOn})
		}
	}
	memo.Prefetch(ctx, queries)

	missing := map[string]bool{}
	entries := make([]entry, 0, len(ops))
	for _, op := range ops {
		if _, ok := covered[op.AccountID]; !ok {
			continue
		}
		rate := decimal.NewFromInt(1)
		if op.Currency != r.BaseCurrency {
			res := memo.Rate(ctx, op.Currency, r.BaseCurrency, op.OccurredOn)
			if errors.Is(res.Err, marketdata.ErrNoRate) {
				missing[op.Currency] = true
				r.LeftOut++
				continue
			}
			if res.Err != nil {
				return nil, res.Err
			}
			rate = res.Rate
		}
		amount, err := money.Minor(decimal.NewFromInt(op.AmountMinor).Mul(rate))
		if err != nil {
			return nil, fmt.Errorf("cashflow: operation %s: %w", op.ID, err)
		}
		fee, err := money.Minor(decimal.NewFromInt(op.FeeMinor).Mul(rate))
		if err != nil {
			return nil, fmt.Errorf("cashflow: operation %s fee: %w", op.ID, err)
		}
		parts, err := convertParts(op, amount, rate)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry{op: op, amount: amount, fee: fee, parts: parts})
	}
	for c := range missing {
		r.MissingRates = append(r.MissingRates, c)
	}
	slices.Sort(r.MissingRates)
	return entries, nil
}

// convertParts is the row's parts in the base currency, signed as the row's
// amount there; the last takes what rounding left, so they add up to it.
func convertParts(op operation.Operation, amount int64, rate decimal.Decimal) ([]operation.Part, error) {
	if len(op.Parts) == 0 {
		return nil, nil
	}
	sign := int64(1)
	if amount < 0 {
		sign = -1
	}
	out := make([]operation.Part, len(op.Parts))
	var sum int64
	for i, p := range op.Parts {
		v, err := money.Minor(decimal.NewFromInt(p.Amount).Mul(rate))
		if err != nil {
			return nil, fmt.Errorf("cashflow: operation %s part: %w", op.ID, err)
		}
		out[i] = operation.Part{CategoryID: p.CategoryID, Amount: sign * v}
		sum += sign * v
	}
	out[len(out)-1].Amount += amount - sum
	return out, nil
}
