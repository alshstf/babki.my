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
	// TransferLots is the FIFO breakdown of what a transfer moved: the
	// source lots it consumed, in FIFO order, each with the day it was
	// acquired (see ReleasedLot).
	//
	// It is STORED once, next to the receiving (transfer_in) leg, whose
	// account has no other way to know those days — but it belongs to BOTH
	// legs and is read onto both (see operation.Store.attachTransferLots).
	// The pieces describe the parcel, not the arrival: the same instrument,
	// quantity and basis leave the source that reach the destination, and the
	// days behind that basis are the same days on either side. Both legs are
	// folded from them — the arriving account rebuilds these lots, the
	// departing one gives up these lots (see Position.releaseRecorded) — which
	// is what keeps a pair from describing two different parcels. It did not
	// always: the departing leg used to work out a release of its own from the
	// queue, and the day the queue's rule changed, every transfer already
	// recorded started releasing lots other than the ones it had frozen (see
	// the package doc).
	//
	// Empty for every other type, for transfers whose basis was supplied by
	// hand (no source lots exist behind such a number), and for transfers
	// recorded before the breakdown was stored at all. For those the original
	// acquisition dates are simply not knowable, and both things derived from
	// the row agree about it: the LOT such a transfer creates carries no date
	// at all, because a lot's date claims to say when the shares were bought
	// and nobody knows (see Lot.AcquiredOn) — and the ROW's ruble equivalent
	// (operation.amountTerms) is null for the same reason, on both legs alike,
	// rather than converting on the transfer's own date as it once did. A
	// figure struck at that date would be exactly the invented number this
	// whole mechanism exists to remove: the shares were not bought on it.
	//
	// A piece INSIDE a breakdown can likewise carry no date, once a parcel
	// that arrived undated is moved on again. The breakdown then records the
	// mixture as it is, and nothing invents the missing half.
	TransferLots []ReleasedLot
	SplitRatio   *decimal.Decimal
	Source       string
	ExternalID   *string
	CreatedAt    time.Time
}
