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
//
// A position's currency is fixed by the first operation that touches the
// instrument in that account; every later operation for the same instrument
// must repeat it. Minor amounts of different currencies are never mixed into
// one int64.
//
// No double counting with account summaries: positions computed here are
// NOT part of GET /api/v1/summary. Account summaries are still built
// exclusively from manually entered balances (table account_balances), so an
// instrument's value never lands in the family totals twice — once as a
// position and once inside a brokerage balance. Market quotes have since
// arrived, but the two views have deliberately not been merged: switching
// brokerage accounts over to a computed "positions + cash" valuation would
// silently change what every recorded balance means, and that is its own
// piece of work.
package portfolio

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

var (
	// ErrOversell means a sell/transfer_out exceeds the held quantity.
	ErrOversell = errors.New("not enough quantity")
	// ErrBadOperation means the journal entry violates engine invariants.
	ErrBadOperation = errors.New("invalid operation")
)

// Lot is one acquisition that is still (partly) held: the quantity left of
// it, the cost basis still attributed to that quantity, and the day it was
// acquired. AcquiredOn is what lets a caller value each lot at the exchange
// rate of its own purchase date instead of one rate for the whole position;
// for a lot created by a transfer_in it is the transfer's date, because the
// carried basis is a snapshot that does not preserve the original dates
// (see the package doc).
type Lot struct {
	Quantity   decimal.Decimal
	CostMinor  int64
	AcquiredOn time.Time
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
	// Lots are the acquisitions still held, oldest first (FIFO order).
	// Their quantities sum to Quantity and their costs sum to CostMinor
	// exactly; a closed position has none.
	Lots []Lot
}

func badOp(o Operation, msg string) error {
	return fmt.Errorf("%w: %s %s: %s", ErrBadOperation, o.Type, o.OccurredOn.Format("2006-01-02"), msg)
}

// ReleasedLot is one piece of a FIFO release: the quantity taken from a
// single source lot, the cost basis attributed to that quantity, and the day
// the source lot was acquired (see Lot.AcquiredOn — the same rules apply: a
// partial piece inherits its lot's date, and a transfer-created lot carries
// the transfer date rather than the original purchase date). A release that
// spans several lots yields several pieces, oldest lot first.
type ReleasedLot struct {
	Quantity   decimal.Decimal
	CostMinor  int64
	AcquiredOn time.Time
}

// releaseFIFO removes qty from the position's lots front-to-back and
// returns the pieces released, oldest lot first. Partial lot pieces use
// floor proportioning; the final piece of a lot takes the lot's remaining
// cost so that the sum of released piece costs always equals the original
// lot cost exactly.
func (p *Position) releaseFIFO(qty decimal.Decimal) ([]ReleasedLot, error) {
	if qty.GreaterThan(p.Quantity) {
		return nil, fmt.Errorf("%w: have %s, need %s", ErrOversell, p.Quantity, qty)
	}
	var pieces []ReleasedLot
	var released int64
	remaining := qty
	for remaining.IsPositive() {
		l := &p.Lots[0]
		if l.Quantity.LessThanOrEqual(remaining) {
			pieces = append(pieces, ReleasedLot{Quantity: l.Quantity, CostMinor: l.CostMinor, AcquiredOn: l.AcquiredOn})
			released += l.CostMinor
			remaining = remaining.Sub(l.Quantity)
			p.Lots = p.Lots[1:]
			continue
		}
		// partial piece: floor share, remainder stays in the lot together
		// with its acquisition date — what is left was bought on the same
		// day as the part just released.
		share := decimal.NewFromInt(l.CostMinor).
			Mul(remaining).Div(l.Quantity).Floor().IntPart()
		pieces = append(pieces, ReleasedLot{Quantity: remaining, CostMinor: share, AcquiredOn: l.AcquiredOn})
		l.CostMinor -= share
		l.Quantity = l.Quantity.Sub(remaining)
		released += share
		remaining = decimal.Zero
	}
	p.Quantity = p.Quantity.Sub(qty)
	p.CostMinor -= released
	return pieces, nil
}

// releasedCost sums the pieces' costs — the one number most callers of
// releaseFIFO actually need.
func releasedCost(pieces []ReleasedLot) int64 {
	var total int64
	for _, pc := range pieces {
		total += pc.CostMinor
	}
	return total
}

func (p *Position) addLot(qty decimal.Decimal, costMinor int64, acquiredOn time.Time) {
	p.Lots = append(p.Lots, Lot{Quantity: qty, CostMinor: costMinor, AcquiredOn: acquiredOn})
	p.Quantity = p.Quantity.Add(qty)
	p.CostMinor += costMinor
}

// Compute folds the journal into positions. See package doc for semantics.
func Compute(ops []Operation) (map[uuid.UUID]*Position, error) {
	positions := make(map[uuid.UUID]*Position)
	// get returns the position for the operation's instrument, creating it on
	// first sight. The currency of that first operation becomes the position's
	// currency and every later operation must match it: minor amounts of two
	// currencies summed into one int64 would be silent corruption, and the
	// service layer only validates the ISO-4217 shape, not consistency.
	get := func(o Operation) (*Position, error) {
		p, ok := positions[*o.InstrumentID]
		if !ok {
			p = &Position{InstrumentID: *o.InstrumentID, Currency: o.Currency}
			positions[*o.InstrumentID] = p
			return p, nil
		}
		if o.Currency != p.Currency {
			return nil, badOp(o, fmt.Sprintf("currency %s does not match position currency %s for instrument %s",
				o.Currency, p.Currency, o.InstrumentID))
		}
		return p, nil
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

		p, err := get(o)
		if err != nil {
			return nil, err
		}
		switch o.Type {
		case TypeBuy:
			if o.AmountMinor >= 0 {
				return nil, badOp(o, "buy amount must be negative")
			}
			p.addLot(*o.Quantity, -o.AmountMinor+o.FeeMinor, o.OccurredOn)
			p.FeesMinor += o.FeeMinor
		case TypeSell:
			if o.AmountMinor <= 0 {
				return nil, badOp(o, "sell amount must be positive")
			}
			pieces, err := p.releaseFIFO(*o.Quantity)
			if err != nil {
				return nil, fmt.Errorf("%s %s %s: %w", o.Type, o.InstrumentID, o.OccurredOn.Format("2006-01-02"), err)
			}
			p.RealizedPnLMinor += o.AmountMinor - releasedCost(pieces) - o.FeeMinor
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
			// the carried basis is a snapshot without the source lots'
			// dates, so the transfer date is the best available answer
			p.addLot(*o.Quantity, o.AmountMinor, o.OccurredOn)
		case TypeSplit:
			ratio := *o.SplitRatio
			// a split rewrites quantities only: neither the cost basis of a
			// lot nor the day it was acquired changes
			for i := range p.Lots {
				p.Lots[i].Quantity = p.Lots[i].Quantity.Mul(ratio)
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
	for i := range p.Lots {
		if amount == 0 {
			return
		}
		take := min(amount, p.Lots[i].CostMinor)
		p.Lots[i].CostMinor -= take
		amount -= take
	}
}

// ReleasedLots computes the FIFO lot breakdown that releasing qty units of
// the instrument would consume, after folding the given journal, without
// mutating anything: which source lots, in what quantity/cost pieces, in
// FIFO order (see ReleasedLot). A future task will use this to carry the
// consumed lots' own dates onto a transfer's destination account instead of
// collapsing them into the transfer date, as ReleasedCost's single number
// forces today.
func ReleasedLots(ops []Operation, instrumentID uuid.UUID, qty decimal.Decimal) ([]ReleasedLot, error) {
	positions, err := Compute(ops)
	if err != nil {
		return nil, err
	}
	p, ok := positions[instrumentID]
	if !ok {
		return nil, fmt.Errorf("%w: no position", ErrOversell)
	}
	return p.releaseFIFO(qty)
}

// ReleasedCost computes the FIFO cost basis of qty units of the instrument
// after folding the given journal, without mutating anything. It is used by
// the transfer service to capture the carried basis at creation time. The
// caller is expected to persist the returned basis on the transfer_in
// operation to maintain the snapshot semantics described in the package doc.
// It is a thin sum over ReleasedLots so the two can never drift apart.
func ReleasedCost(ops []Operation, instrumentID uuid.UUID, qty decimal.Decimal) (int64, error) {
	pieces, err := ReleasedLots(ops, instrumentID, qty)
	if err != nil {
		return 0, err
	}
	return releasedCost(pieces), nil
}
