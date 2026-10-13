// Package account owns financial accounts and their manual balance marks.
// Tables: accounts, account_balances.
package account

import (
	"slices"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/intermediary"
)

type Type string

const (
	TypeBrokerage  Type = "brokerage"
	TypeChecking   Type = "checking"
	TypeSavings    Type = "savings"
	TypeDeposit    Type = "deposit"
	TypeCreditCard Type = "credit_card"
	TypeLoan       Type = "loan"
	TypeCash       Type = "cash"
	// TypeValuables is where bars and coins of precious metal are kept
	// (decision Р-39): its worth is theirs, valued by internal/valuables.
	TypeValuables Type = "valuables"
)

var validTypes = map[Type]bool{
	TypeBrokerage: true, TypeChecking: true, TypeSavings: true, TypeDeposit: true,
	TypeCreditCard: true, TypeLoan: true, TypeCash: true, TypeValuables: true,
}

func (t Type) Valid() bool { return validTypes[t] }

// IsLiability reports whether balances of this type count as debt.
func (t Type) IsLiability() bool { return t == TypeCreditCard || t == TypeLoan }

// LiabilityTypes lists the valid types IsLiability reports true for, sorted,
// so SQL can split assets from debts by the same rule.
func LiabilityTypes() []string {
	out := make([]string, 0, len(validTypes))
	for t := range validTypes {
		if t.IsLiability() {
			out = append(out, string(t))
		}
	}
	slices.Sort(out)
	return out
}

type Status string

const (
	StatusActive   Status = "active"
	StatusArchived Status = "archived"
)

type Account struct {
	ID          uuid.UUID
	SpaceID     uuid.UUID
	OwnerUserID *uuid.UUID
	Name        string
	Type        Type
	Currency    string
	Institution string
	Status      Status
	// ValuedByBalance is the family's choice to count a brokerage account kept
	// by its operations by its balance rather than by its journal (see
	// Handler.valuations).
	ValuedByBalance bool
	// TradesAbroad is whether the account's broker trades on foreign exchanges,
	// where a foreign share sells at its home exchange's price (decision Р-20).
	TradesAbroad bool
	// KeptByOperations is the family's choice to keep an everyday account — not
	// a brokerage one — by its journal rather than by balance marks (household
	// stage 1; see Handler.valuations).
	KeptByOperations bool
	// ForeignBroker is a broker abroad, who withholds no Russian tax: its
	// client pays it by return (tax year, decision Р-41).
	ForeignBroker bool
	// TaxOwnerUserID is whose income the account's is for tax; nil: its
	// personal owner, else the space's owner (decision Р-42).
	TaxOwnerUserID *uuid.UUID
	// Intermediary is the catalog's bank or broker the family picked; nil
	// finds it by Institution (decision Р-47).
	Intermediary *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// IntermediaryOf is the bank or broker holding the account: the family's pick,
// else the one its «bank or broker» names; byHand says the family picked.
// None for cash and valuables, or a name the catalog does not know.
func IntermediaryOf(a Account) (i intermediary.Intermediary, byHand, ok bool) {
	c, err := intermediary.Load()
	if err != nil {
		return intermediary.Intermediary{}, false, false
	}
	if a.Intermediary != nil {
		i, ok = c.ByID(*a.Intermediary)
		return i, true, ok
	}
	switch a.Type {
	case TypeCash, TypeValuables:
		return intermediary.Intermediary{}, false, false
	case TypeBrokerage:
		i, ok = c.Match(a.Institution, intermediary.Broker)
	default:
		i, ok = c.Match(a.Institution, intermediary.Bank)
	}
	return i, false, ok
}

type BalancePoint struct {
	AsOf        time.Time
	AmountMinor int64
}

type WithBalance struct {
	Account
	Balance *BalancePoint
}

type CurrencyTotal struct {
	Currency         string
	AssetsMinor      int64
	LiabilitiesMinor int64
	NetMinor         int64
}

// Update is a partial account update; nil fields are unchanged. OwnerUserID
// is a double pointer: nil leaves it, a pointer to nil makes it shared.
type Update struct {
	Name             *string
	Institution      *string
	OwnerUserID      **uuid.UUID
	Status           *Status
	ValuedByBalance  *bool
	TradesAbroad     *bool
	KeptByOperations *bool
	ForeignBroker    *bool
	// TaxOwnerUserID is a double pointer like OwnerUserID: a pointer to nil
	// gives the account back to its default owner for tax.
	TaxOwnerUserID **uuid.UUID
	// Intermediary likewise: a pointer to nil finds it by name again.
	Intermediary **string
}
