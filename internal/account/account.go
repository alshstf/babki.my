// Package account owns financial accounts and their manual balance marks.
// Tables: accounts, account_balances.
package account

import (
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
