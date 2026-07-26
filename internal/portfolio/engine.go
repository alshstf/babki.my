// Package portfolio computes positions from the operations journal.
// The engine is pure: it takes one account's operations ordered by
// (occurred_on, created_at) ascending and returns per-instrument positions.
// It never touches the database — determinism is the point: the journal is
// the single source of truth and positions are always recomputable.
//
// Transfer cost basis is a snapshot: when a transfer pair is created, the
// moved FIFO cost is computed once (see ReleasedCost) and stored on the
// transfer_in operation. Editing the source account's earlier history later
// does not retroactively adjust an existing transfer's basis — a known and
// accepted MVP simplification.
package portfolio

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

var (
	// ErrOversell means a sell/transfer_out exceeds the held quantity.
	ErrOversell = errors.New("not enough quantity")
	// ErrBadOperation means the journal entry violates engine invariants.
	ErrBadOperation = errors.New("invalid operation")
)

type lot struct {
	quantity  decimal.Decimal
	costMinor int64
}

// Position is the running state of one instrument within one account.
// Closed positions (zero quantity) are kept: realized P&L and income
// remain meaningful history.
type Position struct {
	InstrumentID     uuid.UUID
	Currency         string
	Quantity         decimal.Decimal
	CostMinor        int64 // remaining FIFO cost basis (fees capitalized on buy)
	RealizedPnLMinor int64
	IncomeMinor      int64 // dividends + coupons − instrument-attributed taxes
	FeesMinor        int64
	lots             []lot
}

func badOp(o Operation, msg string) error {
	return fmt.Errorf("%w: %s %s: %s", ErrBadOperation, o.Type, o.OccurredOn.Format("2006-01-02"), msg)
}

// releaseFIFO removes qty from the position's lots front-to-back and
// returns the released cost. Partial lot pieces use floor proportioning;
// the final piece of a lot takes the lot's remaining cost so that the sum
// of released costs always equals the original lot cost exactly.
func (p *Position) releaseFIFO(qty decimal.Decimal) (int64, error) {
	if qty.GreaterThan(p.Quantity) {
		return 0, fmt.Errorf("%w: have %s, need %s", ErrOversell, p.Quantity, qty)
	}
	var released int64
	remaining := qty
	for remaining.IsPositive() {
		l := &p.lots[0]
		if l.quantity.LessThanOrEqual(remaining) {
			released += l.costMinor
			remaining = remaining.Sub(l.quantity)
			p.lots = p.lots[1:]
			continue
		}
		// partial piece: floor share, remainder stays in the lot
		share := decimal.NewFromInt(l.costMinor).
			Mul(remaining).Div(l.quantity).Floor().IntPart()
		l.costMinor -= share
		l.quantity = l.quantity.Sub(remaining)
		released += share
		remaining = decimal.Zero
	}
	p.Quantity = p.Quantity.Sub(qty)
	p.CostMinor -= released
	return released, nil
}

func (p *Position) addLot(qty decimal.Decimal, costMinor int64) {
	p.lots = append(p.lots, lot{quantity: qty, costMinor: costMinor})
	p.Quantity = p.Quantity.Add(qty)
	p.CostMinor += costMinor
}

// Compute folds the journal into positions. See package doc for semantics.
func Compute(ops []Operation) (map[uuid.UUID]*Position, error) {
	positions := make(map[uuid.UUID]*Position)
	get := func(o Operation) *Position {
		p, ok := positions[*o.InstrumentID]
		if !ok {
			p = &Position{InstrumentID: *o.InstrumentID, Currency: o.Currency}
			positions[*o.InstrumentID] = p
		}
		return p
	}

	for _, o := range ops {
		if o.InstrumentID == nil {
			if o.Type.RequiresInstrument() {
				return nil, badOp(o, "instrument required")
			}
			continue // cash-level operation: not the engine's business
		}
		switch o.Type {
		case TypeBuy, TypeSell,
			TypeTransferIn, TypeTransferOut:
			if o.Quantity == nil || !o.Quantity.IsPositive() {
				return nil, badOp(o, "positive quantity required")
			}
		case TypeSplit:
			if o.SplitRatio == nil || !o.SplitRatio.IsPositive() {
				return nil, badOp(o, "positive split_ratio required")
			}
		}

		// Handle conversion ops before get() since they don't mutate positions
		if o.Type == TypeConversion {
			continue
		}

		p := get(o)
		switch o.Type {
		case TypeBuy:
			if o.AmountMinor >= 0 {
				return nil, badOp(o, "buy amount must be negative")
			}
			p.addLot(*o.Quantity, -o.AmountMinor+o.FeeMinor)
			p.FeesMinor += o.FeeMinor
		case TypeSell:
			if o.AmountMinor <= 0 {
				return nil, badOp(o, "sell amount must be positive")
			}
			released, err := p.releaseFIFO(*o.Quantity)
			if err != nil {
				return nil, fmt.Errorf("%s %s %s: %w", o.Type, o.InstrumentID, o.OccurredOn.Format("2006-01-02"), err)
			}
			p.RealizedPnLMinor += o.AmountMinor - released - o.FeeMinor
			p.FeesMinor += o.FeeMinor
		case TypeDividend, TypeCoupon:
			p.IncomeMinor += o.AmountMinor
		case TypeTax:
			p.IncomeMinor += o.AmountMinor // negative
		case TypeFee:
			p.FeesMinor += -o.AmountMinor // amount negative → positive fee
		case TypeAmortization:
			// return of principal: reduce cost basis, excess is realized gain
			if o.AmountMinor <= 0 {
				return nil, badOp(o, "amortization amount must be positive")
			}
			reduce := min(o.AmountMinor, p.CostMinor)
			p.CostMinor -= reduce
			drainLotsCost(p, reduce)
			p.RealizedPnLMinor += o.AmountMinor - reduce
		case TypeTransferOut:
			if _, err := p.releaseFIFO(*o.Quantity); err != nil {
				return nil, fmt.Errorf("%s %s %s: %w", o.Type, o.InstrumentID, o.OccurredOn.Format("2006-01-02"), err)
			}
			// released cost intentionally discarded: the pair's transfer_in
			// carries the basis captured at creation time (see package doc
			// for the recompute limitation).
		case TypeTransferIn:
			if o.AmountMinor < 0 {
				return nil, badOp(o, "transfer_in amount (cost basis) must be >= 0")
			}
			p.addLot(*o.Quantity, o.AmountMinor)
		case TypeSplit:
			ratio := *o.SplitRatio
			for i := range p.lots {
				p.lots[i].quantity = p.lots[i].quantity.Mul(ratio)
			}
			p.Quantity = p.Quantity.Mul(ratio)
		default:
			return nil, badOp(o, "type not applicable to instrument operations")
		}
	}
	return positions, nil
}

// drainLotsCost subtracts amount from lot costs front-to-back (amortization
// keeps quantities intact; only the cost basis shrinks).
func drainLotsCost(p *Position, amount int64) {
	for i := range p.lots {
		if amount == 0 {
			return
		}
		take := min(amount, p.lots[i].costMinor)
		p.lots[i].costMinor -= take
		amount -= take
	}
}

// ReleasedCost computes the FIFO cost basis of qty units of the instrument
// after folding the given journal, without mutating anything. It is used by
// the transfer service to capture the carried basis at creation time. The
// caller is expected to persist the returned basis on the transfer_in
// operation to maintain the snapshot semantics described in the package doc.
func ReleasedCost(ops []Operation, instrumentID uuid.UUID, qty decimal.Decimal) (int64, error) {
	positions, err := Compute(ops)
	if err != nil {
		return 0, err
	}
	p, ok := positions[instrumentID]
	if !ok {
		return 0, fmt.Errorf("%w: no position", ErrOversell)
	}
	return p.releaseFIFO(qty)
}
