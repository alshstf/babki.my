// Package operation owns the operations journal, the system's single source of
// truth; positions and valuations are projections of it. Operation and Type are
// aliases of portfolio's types, so the engine needs nothing from this package and
// Service can replay journals through it without an import cycle.
package operation

import "babki.my/babki/internal/portfolio"

type (
	Operation = portfolio.Operation
	Part      = portfolio.CategoryPart
	Type      = portfolio.Type
	// ReleasedLot is one piece of a transfer's FIFO breakdown, carried in
	// Operation.TransferLots and stored in table operation_transfer_lots.
	ReleasedLot = portfolio.ReleasedLot
)

const (
	TypeBuy          = portfolio.TypeBuy
	TypeSell         = portfolio.TypeSell
	TypeRedemption   = portfolio.TypeRedemption
	TypeDeposit      = portfolio.TypeDeposit
	TypeWithdrawal   = portfolio.TypeWithdrawal
	TypeDividend     = portfolio.TypeDividend
	TypeCoupon       = portfolio.TypeCoupon
	TypeAmortization = portfolio.TypeAmortization
	TypeFee          = portfolio.TypeFee
	TypeTax          = portfolio.TypeTax
	TypeTransferIn   = portfolio.TypeTransferIn
	TypeTransferOut  = portfolio.TypeTransferOut
	TypeExchangeOut  = portfolio.TypeExchangeOut
	TypeExchangeIn   = portfolio.TypeExchangeIn
	TypeSpinoffOut   = portfolio.TypeSpinoffOut
	TypeSpinoffIn    = portfolio.TypeSpinoffIn
	TypeSplit        = portfolio.TypeSplit
	TypeInterest     = portfolio.TypeInterest
	TypeConversion   = portfolio.TypeConversion
)

// SourceRegistry writes the rows the corporate-actions registry materializes:
// splits and the legs of conversions and spin-offs. Like an imported row, a
// registry row is a projection of a fact recorded elsewhere and would be written
// back if deleted, so it is edited in the registry. The column's CHECK constraint
// closes the set of sources (migration 0022).
const SourceRegistry = "registry"

// SourceTable is the writer of rows loaded from a table a person uploaded
// (internal/importer/table). They are the person's own, like a hand entry:
// nothing writes them back, so a person edits and deletes them in the journal.
// A broker's rows and the registry's are not theirs to change.
const SourceTable = "csv"

// OwnedByHand reports whether a person may edit and delete a row of source.
func OwnedByHand(source string) bool { return source == SourceManual || source == SourceTable }
