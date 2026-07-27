package portfolio

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Operation and Type are defined here (rather than in package operation)
// because the engine in this file is the base of the dependency: it must
// stay free of any dependency on package operation's Store/Service so that
// operation.Service (which needs to call portfolio.Compute for consistency
// checks) can depend on this package without creating an import cycle.
// Package operation re-exports both by alias, so operation.Operation and
// operation.Type remain the canonical names used by the rest of the system;
// this is purely a placement detail.

type Type string

const (
	TypeBuy          Type = "buy"
	TypeSell         Type = "sell"
	TypeDeposit      Type = "deposit"
	TypeWithdrawal   Type = "withdrawal"
	TypeDividend     Type = "dividend"
	TypeCoupon       Type = "coupon"
	TypeAmortization Type = "amortization"
	TypeFee          Type = "fee"
	TypeTax          Type = "tax"
	TypeTransferIn   Type = "transfer_in"
	TypeTransferOut  Type = "transfer_out"
	TypeSplit        Type = "split"
	TypeInterest     Type = "interest"
	TypeConversion   Type = "conversion"
)

var validTypes = map[Type]bool{
	TypeBuy: true, TypeSell: true, TypeDeposit: true, TypeWithdrawal: true,
	TypeDividend: true, TypeCoupon: true, TypeAmortization: true, TypeFee: true,
	TypeTax: true, TypeTransferIn: true, TypeTransferOut: true, TypeSplit: true,
	TypeInterest: true, TypeConversion: true,
}

func (t Type) Valid() bool   { return validTypes[t] }
func (t Type) IsTrade() bool { return t == TypeBuy || t == TypeSell }

// RequiresInstrument reports whether the type is meaningless without one.
// Dividend and coupon are deliberately excluded: they may be recorded at
// the cash level (no specific instrument attribution) per the operation
// service's validation contract; amortization always tracks a bond
// position, so it keeps the requirement.
func (t Type) RequiresInstrument() bool {
	switch t {
	case TypeBuy, TypeSell, TypeAmortization,
		TypeTransferIn, TypeTransferOut, TypeSplit:
		return true
	}
	return false
}

// Operation is one journal entry. AmountMinor is the signed cash effect on
// the account (buy < 0, sell > 0, ...); for transfers it carries the moved
// cost basis and has zero cash meaning; for splits it is 0.
type Operation struct {
	ID              uuid.UUID
	SpaceID         uuid.UUID
	AccountID       uuid.UUID
	InstrumentID    *uuid.UUID
	Type            Type
	OccurredOn      time.Time
	SettledOn       *time.Time
	Quantity        *decimal.Decimal
	Price           *decimal.Decimal
	AmountMinor     int64
	Currency        string
	FeeMinor        int64
	Note            string
	TransferGroupID *uuid.UUID
	SplitRatio      *decimal.Decimal
	Source          string
	ExternalID      *string
	CreatedAt       time.Time
}
