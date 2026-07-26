// Package operation owns the unified operations journal — the single source
// of truth of the system. Positions and valuations are deterministic
// projections computed from it. Table: operations.
package operation

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

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
func (t Type) RequiresInstrument() bool {
	switch t {
	case TypeBuy, TypeSell, TypeDividend, TypeCoupon, TypeAmortization,
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
