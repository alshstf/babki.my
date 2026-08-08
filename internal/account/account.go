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

// LiabilityTypes lists every valid type IsLiability reports true for, as the
// plain strings the `accounts.type` column stores.
//
// IT IS DERIVED AND NOT TYPED OUT, because SummaryByCurrency has to split
// assets from debts inside SQL, where the method above cannot be called. That
// split used to be a literal `('credit_card','loan')` in the query — a second
// statement of the same rule, in another language, which a new liability type
// would have left silently behind: the account would be created and listed
// perfectly and would then be summed as an asset. Filtering the valid types
// through IsLiability leaves one statement of the rule and makes the query's
// list follow it.
//
// Sorted, so the value is the same on every call: Go randomizes map iteration,
// and a query parameter that reordered between requests would make an
// otherwise identical statement look different to anything reading logs.
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
	CreatedAt   time.Time
	UpdatedAt   time.Time
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

// Update describes a partial account update; nil fields are left unchanged.
// OwnerUserID uses a double pointer: nil = unchanged, *nil = clear to shared.
type Update struct {
	Name        *string
	Institution *string
	OwnerUserID **uuid.UUID
	Status      *Status
}
