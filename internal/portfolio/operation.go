package portfolio

import (
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Operation and Type live here so the engine does not import package operation,
// which calls the engine; operation re-exports both by alias.

type Type string

const (
	TypeBuy  Type = "buy"
	TypeSell Type = "sell"
	// TypeRedemption is a bond reaching maturity. The arithmetic is a sale's and
	// the engine treats them alike; it is a type of its own because nobody sold
	// anything (НК РФ ст. 214.1: «реализации (погашения)»).
	TypeRedemption   Type = "redemption"
	TypeDeposit      Type = "deposit"
	TypeWithdrawal   Type = "withdrawal"
	TypeDividend     Type = "dividend"
	TypeCoupon       Type = "coupon"
	TypeAmortization Type = "amortization"
	TypeFee          Type = "fee"
	TypeTax          Type = "tax"
	TypeTransferIn   Type = "transfer_in"
	TypeTransferOut  Type = "transfer_out"
	// TypeExchangeOut and TypeExchangeIn are the legs of a securities conversion
	// on one account (a depositary receipt into its share, a fund under a new
	// ISIN): N units of one paper become M of another. It is not a disposal: the
	// basis and its days travel whole (НК РФ ст. 214.1 п. 13 абз. 17; holding period
	// per ст. 219.1 as amended by 389-ФЗ). Each leg stores its own breakdown, piece
	// for piece, since the instrument and the count change.
	TypeExchangeOut Type = "exchange_out"
	TypeExchangeIn  Type = "exchange_in"
	// TypeSpinoffOut and TypeSpinoffIn are the legs of a spin-off: paper A stays
	// and paper B appears beside it (TECH, TSPX, TUSD into TECH2, TSPX2, TUSD2 on
	// 2023-12-22). Only a share of A's basis moves — by default none (decision
	// Р-16, as the broker keeps it). The departing leg moves no units and has no
	// quantity; its pieces carry each lot's own count as identity (see
	// Position.applySpinoffOut). The arriving leg is an ordinary parcel built from
	// those pieces, keeping the original acquisition days.
	TypeSpinoffOut Type = "spinoff_out"
	TypeSpinoffIn  Type = "spinoff_in"
	TypeSplit      Type = "split"
	TypeInterest   Type = "interest"
	TypeConversion Type = "conversion"
)

var validTypes = map[Type]bool{
	TypeBuy: true, TypeSell: true, TypeRedemption: true, TypeDeposit: true, TypeWithdrawal: true,
	TypeDividend: true, TypeCoupon: true, TypeAmortization: true, TypeFee: true,
	TypeTax: true, TypeTransferIn: true, TypeTransferOut: true, TypeSplit: true,
	TypeInterest: true, TypeConversion: true,
	TypeExchangeOut: true, TypeExchangeIn: true,
	TypeSpinoffOut: true, TypeSpinoffIn: true,
}

func (t Type) Valid() bool { return validTypes[t] }

// Types lists every operation type, sorted; a test holds it to the schema's
// CHECK.
func Types() []Type {
	out := make([]Type, 0, len(validTypes))
	for t := range validTypes {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

// RequiresInstrument reports whether the type needs an instrument. Dividends
// and coupons may be recorded at the cash level; amortization always needs a
// bond.
func (t Type) RequiresInstrument() bool {
	switch t {
	case TypeBuy, TypeSell, TypeRedemption, TypeAmortization,
		TypeTransferIn, TypeTransferOut, TypeSplit,
		TypeExchangeOut, TypeExchangeIn,
		TypeSpinoffOut, TypeSpinoffIn:
		return true
	}
	return false
}

// mustMatchPositionCurrency reports whether the entry must be in its
// position's currency, and so settles it when unsettled (Compute's get).
//
// The rule protects the single-currency figures: CostMinor, lot costs, and the
// basis a disposal retires.
//   - Dividend, coupon, tax: false; income is kept per currency.
//   - Fee: false; fees are kept per currency (a rouble commission on selling a
//     yuan bond).
//   - Sell and redemption: false; proceeds and fee go to a Realization with its
//     own currency, and what is retired is decided by quantity.
//   - Amortization: true; it retires basis by amount, which would need a rate.
//   - Anything moving no money (a zero-basis transfer, a split): false.
//
// Types the engine does not fold are refused elsewhere; new types default to
// strict.
func (o Operation) mustMatchPositionCurrency() bool {
	switch o.Type {
	case TypeDividend, TypeCoupon, TypeTax, TypeFee, TypeSell, TypeRedemption:
		return false
	}
	return o.AmountMinor != 0 || o.FeeMinor != 0 || LotsCost(o.TransferLots) != 0
}

// Operation is one journal entry. AmountMinor is the signed cash effect (buy <
// 0, sell > 0); on a transfer it is the moved basis with no cash meaning; on a
// split 0.
type Operation struct {
	ID           uuid.UUID
	SpaceID      uuid.UUID
	AccountID    uuid.UUID
	InstrumentID *uuid.UUID
	Type         Type
	OccurredOn   time.Time
	// OccurredAt is the source's instant, when it gives one; within a day the
	// journal folds by it (operation.foldsBefore). The engine never reads it.
	OccurredAt  *time.Time
	SettledOn   *time.Time
	Quantity    *decimal.Decimal
	Price       *decimal.Decimal
	AmountMinor int64
	Currency    string
	FeeMinor    int64
	Note        string
	// TradingMode is where the operation happened, in the reporter's code ("TQBR",
	// "FINEX_OTC"); nil when unreported. The engine never reads it.
	TradingMode     *string
	TransferGroupID *uuid.UUID
	// TransferLots is the FIFO breakdown of what a transfer moved, each piece with
	// its acquisition day. It is stored once with the transfer_in but belongs to
	// both legs, which both fold from it (see Position.releaseRecorded).
	//
	// It is empty for other types and for transfers with a hand-given basis or from
	// before breakdowns; their purchase dates are unknowable, so the created lot is
	// undated and the row's base-currency figure is null. A piece may itself be
	// undated when an undated parcel moves on.
	TransferLots []ReleasedLot
	SplitRatio   *decimal.Decimal
	// FaceBeforeMinor is, on an amortization, the bond's outstanding face value
	// per unit before this repayment, in the operation's currency: the repayment
	// then retires basis in proportion (НК РФ ст. 214.1 п. 13, decision Р-4).
	// Without it the old rule applies. Nil on other types.
	FaceBeforeMinor *int64
	// CategoryID and Counterparty say what money that came or went was for and
	// with whom (household stage 1): a spending is a withdrawal with a spending
	// category. The engine never reads them.
	CategoryID   *uuid.UUID
	Counterparty string
	// MemberID is whose the row is when not the account's owner's: the member
	// who spent or earned it. The engine never reads it.
	MemberID *uuid.UUID
	// Parts split a spending or an earning across categories (decision Р-36):
	// their amounts, positive, add up to the row's; empty when the row is its
	// one category's. The engine never reads them.
	Parts      []CategoryPart
	Source     string
	ExternalID *string
	CreatedAt  time.Time
}

// CategoryPart is a part of a row filed under a category of its own.
type CategoryPart struct {
	CategoryID uuid.UUID
	Amount     int64
}
