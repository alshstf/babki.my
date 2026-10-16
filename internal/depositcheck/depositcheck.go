// Package depositcheck tells the conditions of a deposit's or a savings
// account's bonuses from the family's own data, for the accounts kept in the
// automatic mode (decision Р-53 «Б, но»): purchases on the bank's cards in a
// month from the journals, wages paid into the bank by the categories marked
// as wages, the balance kept in the bank by its marks.
package depositcheck

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/deposit"
	"babki.my/babki/internal/operation"
)

type accounts interface {
	ListWithBalance(ctx context.Context, spaceID uuid.UUID) ([]account.WithBalance, error)
	BalanceHistory(ctx context.Context, spaceID uuid.UUID) (map[uuid.UUID][]account.BalancePoint, error)
}

type operations interface {
	ListMoneyFlows(ctx context.Context, spaceID uuid.UUID, from, to time.Time) ([]operation.Operation, error)
}

type categories interface {
	List(ctx context.Context, spaceID uuid.UUID) ([]category.Category, error)
}

// Service measures a space's banks month by month.
type Service struct {
	accounts   accounts
	operations operations
	categories categories
}

func New(a accounts, o operations, c categories) *Service {
	return &Service{accounts: a, operations: o, categories: c}
}

// The roubles only: the banks' thresholds are in roubles, and a card in
// another currency would need its own rate of the day.
const rub = "RUB"

// MeasuresFor is the space's measure: the accounts, their banks and the
// wages' categories read once, a month's operations when first asked for.
// A failure to read a month leaves it unknown — a condition the family can
// still mark.
func (s *Service) MeasuresFor(ctx context.Context, spaceID uuid.UUID) (account.Measure, error) {
	list, err := s.accounts.ListWithBalance(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	history, err := s.accounts.BalanceHistory(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	cats, err := s.categories.List(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	wages := map[uuid.UUID]bool{}
	for _, c := range cats {
		if c.TaxMark != nil && *c.TaxMark == category.TaxWages {
			wages[c.ID] = true
		}
	}
	// The bank of each account, and which are its cards — the accounts a
	// purchase is paid from.
	bankOf := map[uuid.UUID]string{}
	cards := map[uuid.UUID]bool{}
	for _, a := range list {
		i, _, ok := account.IntermediaryOf(a.Account)
		if !ok {
			continue
		}
		bankOf[a.ID] = i.ID
		cards[a.ID] = a.Type == account.TypeChecking || a.Type == account.TypeCreditCard
	}
	var mu sync.Mutex
	months := map[time.Time][]operation.Operation{}
	failed := map[time.Time]bool{}
	flows := func(month time.Time) ([]operation.Operation, bool) {
		mu.Lock()
		defer mu.Unlock()
		if ops, ok := months[month]; ok {
			return ops, true
		}
		if failed[month] {
			return nil, false
		}
		ops, err := s.operations.ListMoneyFlows(ctx, spaceID, month, month.AddDate(0, 1, -1))
		if err != nil {
			failed[month] = true
			return nil, false
		}
		months[month] = ops
		return ops, true
	}
	return func(bank string, check deposit.Check, month time.Time) (int64, bool) {
		switch check {
		case deposit.CheckCardSpend, deposit.CheckWages:
			ops, ok := flows(month)
			if !ok {
				return 0, false
			}
			return measureFlows(ops, bank, check, bankOf, cards, wages)
		case deposit.CheckBankBalance:
			return balanceIn(list, history, bankOf, bank, month)
		}
		return 0, false
	}, nil
}

// measureFlows is what a month's money flows say of the bank: the purchases
// paid from its cards, or the wages paid into its accounts. Known when the
// family kept any of the bank's accounts that month — no operations at all
// say nothing, not «none».
func measureFlows(ops []operation.Operation, bank string, check deposit.Check, bankOf map[uuid.UUID]string, cards, wages map[uuid.UUID]bool) (int64, bool) {
	var sum int64
	seen := false
	for _, o := range ops {
		if bankOf[o.AccountID] != bank || o.Currency != rub {
			continue
		}
		switch check {
		case deposit.CheckCardSpend:
			if !cards[o.AccountID] {
				continue
			}
			seen = true
			if o.Type == operation.TypeWithdrawal && o.AmountMinor < 0 {
				sum += -o.AmountMinor
			}
		case deposit.CheckWages:
			seen = true
			if o.Type == operation.TypeDeposit && o.AmountMinor > 0 && o.CategoryID != nil && wages[*o.CategoryID] {
				sum += o.AmountMinor
			}
		}
	}
	return sum, seen
}

// balanceIn is the balance kept in the bank in a month: over its accounts,
// the least of their marks in the month and the last one before it. Known
// when one of them has a mark by the month's end.
func balanceIn(list []account.WithBalance, history map[uuid.UUID][]account.BalancePoint, bankOf map[uuid.UUID]string, bank string, month time.Time) (int64, bool) {
	end := month.AddDate(0, 1, 0)
	var sum int64
	known := false
	for _, a := range list {
		if bankOf[a.ID] != bank || a.Currency != rub {
			continue
		}
		var least *int64
		for _, p := range history[a.ID] {
			if !p.AsOf.Before(end) {
				break
			}
			v := p.AmountMinor
			if p.AsOf.Before(month) {
				least = &v // the last mark before the month, until one in it
				continue
			}
			if least == nil || v < *least {
				least = &v
			}
		}
		if least != nil {
			sum += *least
			known = true
		}
	}
	return sum, known
}
