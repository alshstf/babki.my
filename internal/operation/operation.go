// Package operation owns the unified operations journal — the single source
// of truth of the system. Positions and valuations are deterministic
// projections computed from it. Table: operations.
//
// Operation and Type are aliases of the identically-named types defined in
// package portfolio. They live there so the pure computation engine has no
// dependency on this package's Store/Service, letting Service depend on
// portfolio (to replay the journal through Compute for consistency checks)
// without an import cycle. This package remains the canonical name for the
// domain type; the split is a placement detail only.
package operation

import "babki.my/babki/internal/portfolio"

type (
	Operation = portfolio.Operation
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
	TypeSplit        = portfolio.TypeSplit
	TypeInterest     = portfolio.TypeInterest
	TypeConversion   = portfolio.TypeConversion
)

// SourceRegistry is the writer of the rows the corporate-actions registry
// materializes into journals: splits, and the two legs of a conversion.
//
// IT IS A SOURCE OF ITS OWN rather than "manual", and the distinction is what
// the delete rule already reads (see Service.Delete): a registry row is a
// projection of a fact recorded elsewhere — what happened to the PAPER, which
// is true for every account that held it — so deleting the row on one account
// would be undone the next time the registry is applied, exactly as an imported
// row is written back by its importer. The fact is edited in the registry, and
// the journals follow.
//
// The set of sources is closed by a CHECK constraint on the column, so adding
// one is a migration (0021) as well as a constant.
const SourceRegistry = "registry"
