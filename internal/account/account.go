// Package account owns financial accounts and their manual balance marks.
// Tables: accounts, account_balances.
package account

import (
	"slices"
	"time"

	"github.com/google/uuid"
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
)

var validTypes = map[Type]bool{
	TypeBrokerage: true, TypeChecking: true, TypeSavings: true, TypeDeposit: true,
	TypeCreditCard: true, TypeLoan: true, TypeCash: true,
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
	CreatedAt        time.Time
	UpdatedAt        time.Time
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
}
