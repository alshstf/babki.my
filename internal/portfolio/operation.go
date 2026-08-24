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
	TypeBuy  Type = "buy"
	TypeSell Type = "sell"
	// TypeRedemption is a bond reaching maturity: the issuer takes the paper
	// back and pays the principal. THE ARITHMETIC IS A SALE'S, exactly — the
	// bonds leave, the money arrives, and the queue gives up the basis they
	// carried — and the engine therefore treats the two as one throughout.
	//
	// It is a type of its own all the same, for two reasons that are not about
	// arithmetic. The journal already names the PARTIAL repayment separately
	// (TypeAmortization), so the full one masquerading as a sale was the odd
	// one out; and the word on the screen was a lie about what happened —
	// nobody sold anything, the bond ran out. НК РФ ст. 214.1 names the two
	// together ("реализации (погашения)"), which is why one computation serves
	// both and why nothing here needs a second rule.
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
	// TypeExchangeOut and TypeExchangeIn are the two legs of a SECURITIES
	// CONVERSION: one paper becomes another — a depositary receipt converted
	// into the share it represented, a fund's units reissued under a new ISIN —
	// with N units of the old giving M units of the new, on ONE account, on one
	// day.
	//
	// IT IS NOT A DISPOSAL AND MUST NEVER BE FOLDED AS ONE. Nothing was sold and
	// nothing was bought: the holder paid nothing and received nothing, so no
	// result is realized and no new basis is created. НК РФ ст. 214.1 п. 13
	// abz. 17 says as much for the case this was built for — the expense behind
	// shares received when a depositary receipt is redeemed is the price the
	// RECEIPT was acquired at — and ст. 219.1 (as amended by 389-ФЗ of
	// 2023-07-31) counts the holding period from the receipt's own acquisition.
	// So the parcel travels whole: its cost basis and the days behind that basis
	// arrive on the new paper unchanged, and only the number of units is
	// restated.
	//
	// THE TWO LEGS DESCRIBE DIFFERENT PARCELS, which is what separates this pair
	// from a transfer's. A transfer moves the same shares to another account, so
	// one breakdown serves both legs and is stored once; a conversion changes
	// the instrument AND the count, so the departing leg's pieces sum to N units
	// of the old paper and the arriving leg's to M of the new. Each leg
	// therefore carries and stores its OWN breakdown (see
	// operation.carriesOwnLots), and the piece-for-piece correspondence between
	// them is what carries a date and a cost from one paper to the other.
	TypeExchangeOut Type = "exchange_out"
	TypeExchangeIn  Type = "exchange_in"
	// TypeSpinoffOut and TypeSpinoffIn are the two legs of a SPIN-OFF: paper A
	// STAYS with the holder and paper B appears beside it, N units of A giving
	// M units of B, on one account, on one day. The owner's own case is
	// Т-Капитал carving the blocked assets out of TECH, TSPX and TUSD into the
	// closed funds TECH2, TSPX2 and TUSD2 on 2023-12-22, units one for one —
	// and the broker sent no operation for any of it.
	//
	// WHAT SEPARATES IT FROM A CONVERSION IS THAT NOTHING LEAVES. A conversion
	// retires the old paper and its whole basis travels; a spin-off keeps the
	// old paper, its units untouched, and moves only a SHARE of the money that
	// was paid for it. НК РФ ст. 214.1 п. 13 abz. 8 sends the question to
	// ст. 277 п. 7, which fixes the share exactly: the units of the additional
	// fund are worth the part of the original units' cost that the carved-out
	// assets were of the fund's net assets before the carve-out, and the
	// original units' cost is reduced by that same figure. Neither income nor
	// expense arises on the day.
	//
	// SO THE DEPARTING LEG MOVES NO UNITS AT ALL, and it carries no quantity —
	// the field is nil, exactly as a split's is, because there is no count to
	// put in it and any count put there would be read as shares leaving. What
	// it carries instead is the money: AmountMinor is the basis moved, and
	// TransferLots names, lot by lot and in queue order, which parcel gave up
	// how much of it. Those pieces carry each lot's OWN quantity — not as
	// something that moved, but as the lot's identity, so that a journal which
	// has grown a parcel underneath the record is caught rather than silently
	// re-allocated (see Position.applySpinoffOut).
	//
	// THE ARRIVING LEG IS THE ORDINARY ONE: M units of B, built from the very
	// pieces the departing leg named, each keeping its cost and the day the
	// original parcel was acquired — a spin-off is not a purchase, and the day
	// the new paper appeared is not the day its money was spent.
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

// RequiresInstrument reports whether the type is meaningless without one.
// Dividend and coupon are deliberately excluded: they may be recorded at
// the cash level (no specific instrument attribution) per the operation
// service's validation contract; amortization always tracks a bond
// position, so it keeps the requirement.
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

// mustMatchPositionCurrency reports whether this entry has to be denominated in
// the currency of the position it touches — and, by the same token, settles that
// currency when nothing has settled it yet (see Compute's get, which is the only
// caller).
//
// THE QUESTION IT ANSWERS IS WHETHER THIS ENTRY PUTS MONEY INTO A FIGURE THAT
// HOLDS ONE CURRENCY. Those figures are CostMinor, the cost of a lot, and the
// basis a disposal retires: each is a single int64 of minor units, and two
// currencies inside one is nonsense no rounding can rescue and no reader could
// detect. Everything else about a position is kept per currency or per event,
// and has nothing for a second currency to corrupt.
//
// FALSE FOR DIVIDEND, COUPON AND TAX, which produce nothing but income, kept per
// currency (Position.IncomeByCurrency). This is what lets a yuan bond pay its
// coupons in rubles and a dollar share pay its dividend, and have its tax
// withheld, in rubles: the broker converts on the day of the payment, the paper
// stays priced in its own currency, and both facts are recorded as they happened
// instead of one of them being refused.
//
// FALSE FOR A FEE, which is kept per currency too (Position.FeesByCurrency). It
// used to be strict, on the argument that the fee total was one number in the
// position's currency — which was true of the figure and is no longer, because
// the figure is a list now. The commission charged in rubles on the sale of a
// yuan bond is the case: refusing it lost the whole sale over a charge of four
// rubles, and capitalizing it into a yuan basis would have been worse.
//
// FALSE FOR A SALE AND FOR A REDEMPTION, which the engine treats as one thing
// (see TypeRedemption), and this is the exemption that needs the argument. Its proceeds
// and its fee go to a Realization, which carries its own currency
// (Realization.Currency), and what it retires is decided by the QUANTITY sold —
// the queue gives up the same parcels of the same basis whatever currency the
// money arrived in. So nothing of a sale reaches a single-currency figure, and
// it need not settle the position's currency either: a sale can only ever follow
// an acquisition that already did (with nothing acquired there is nothing to
// release, and the engine refuses it for that instead).
//
// TRUE FOR AN AMORTIZATION, which looks like a sale and is not. It retires basis
// BY AMOUNT, so a ruble payment against a yuan basis would need a rate to say how
// much of it was retired — and that rate would live on in the REMAINING basis,
// changing every later figure for that bond. A refusal naming that is honest; a
// rate invented here would not be.
//
// TRUE FOR ANYTHING THAT MOVES NO MONEY AT ALL — false, rather. A transfer whose
// basis is zero, a split, an entry with nothing in its amount, its fee or its
// carried lots: there is no sum for its currency to be wrong about. This is what
// admits the securities transfer that arrives with no cost attached, which the
// broker denominates in the paper's own currency while the receiving account
// holds it in another, and which was refused for years over a number that is
// nought either way.
//
// The types the engine never folds into a position — deposit, withdrawal,
// interest, conversion — answer by the money rule and it costs nothing: a
// conversion is skipped before this is reached and the rest are refused by type
// moments later. What the default buys is that a type added to the enum later is
// treated strictly until somebody decides otherwise.
func (o Operation) mustMatchPositionCurrency() bool {
	switch o.Type {
	case TypeDividend, TypeCoupon, TypeTax, TypeFee, TypeSell, TypeRedemption:
		return false
	}
	return o.AmountMinor != 0 || o.FeeMinor != 0 || LotsCost(o.TransferLots) != 0
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
