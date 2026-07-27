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
)

const (
	TypeBuy          = portfolio.TypeBuy
	TypeSell         = portfolio.TypeSell
	TypeDeposit      = portfolio.TypeDeposit
	TypeWithdrawal   = portfolio.TypeWithdrawal
	TypeDividend     = portfolio.TypeDividend
	TypeCoupon       = portfolio.TypeCoupon
	TypeAmortization = portfolio.TypeAmortization
	TypeFee          = portfolio.TypeFee
	TypeTax          = portfolio.TypeTax
	TypeTransferIn   = portfolio.TypeTransferIn
	TypeTransferOut  = portfolio.TypeTransferOut
	TypeSplit        = portfolio.TypeSplit
	TypeInterest     = portfolio.TypeInterest
	TypeConversion   = portfolio.TypeConversion
)
