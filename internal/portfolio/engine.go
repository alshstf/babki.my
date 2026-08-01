// Package portfolio computes positions from the operations journal.
// The engine is pure: it takes one account's operations ordered by
// (occurred_on, created_at) ascending and returns per-instrument positions.
// It never touches the database — determinism is the point: the journal is
// the single source of truth and positions are always recomputable.
//
// Transfer cost basis is a snapshot: when a transfer pair is created, the
// lots it consumes are resolved once (see ReleasedLots) and stored alongside
// the transfer_in operation, their summed cost on the operation itself.
// Editing the source account's earlier history later does not retroactively
// adjust an existing transfer's basis — a known and accepted MVP
// simplification.
//
// The receiving account rebuilds those very lots (see Compute's transfer_in
// branch), each keeping the day it was bought: moving shares between the
// family's own accounts is not a purchase, so it must change neither the basis
// nor the dates that basis is later valued at. A transfer with no stored
// breakdown — basis given by hand, or recorded before breakdowns were kept —
// still produces one lot dated on the transfer day, because for those the
// original dates do not exist to be restored.
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
// a transfer_in rebuilds one lot per piece of its stored breakdown, each
// keeping the day it was bought, and only a transfer WITHOUT a breakdown
// yields a single lot dated on the transfer itself (see the package doc).
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
	// Lots are the acquisitions still held, in the order they entered this
	// account — which is the order releases consume them (FIFO). That is
	// usually oldest-purchase-first, but not always: a transfer_in brings in
	// lots bought before ones this account already holds, and they queue up
	// behind them rather than being re-sorted, because a release must depend
	// only on replaying the journal in its own order.
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
// partial piece inherits its lot's date, and a lot that itself arrived by
// transfer passes on whatever date it carries). A release that spans several
// lots yields several pieces, in the order the lots are consumed.
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

// LotsCost sums the pieces' costs — the one number most callers of a FIFO
// release actually need. It is exported so a caller that already holds the
// breakdown (see ReleasedLots) derives the total from those very pieces
// instead of computing the same quantity a second, independent way.
func LotsCost(pieces []ReleasedLot) int64 {
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
			p.RealizedPnLMinor += o.AmountMinor - LotsCost(pieces) - o.FeeMinor
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
			if len(o.TransferLots) == 0 {
				// No breakdown: the basis was given by hand (nothing was
				// released, so there are no source lots behind that number) or
				// the transfer was recorded before breakdowns were kept. Either
				// way the original purchase dates do not exist to be restored,
				// and the transfer's own date is the honest best answer —
				// inventing dates would be worse than an admittedly rough one.
				p.addLot(*o.Quantity, o.AmountMinor, o.OccurredOn)
				break
			}
			if err := checkTransferLots(o); err != nil {
				return nil, err
			}
			// Rebuild what the source account released, piece by piece, in the
			// FIFO order it was released in: each moved lot keeps its own
			// quantity, its own cost and the day it was actually bought. A
			// transfer between the family's own accounts is not a purchase, so
			// it must not reprice anything — and a lot's date is what later
			// values it at the fx rate of its own purchase day.
			for _, pc := range o.TransferLots {
				p.addLot(pc.Quantity, pc.CostMinor, pc.AcquiredOn)
			}
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

// checkTransferLots verifies that a transfer_in's stored FIFO breakdown and
// the operation carrying it describe the same event: every piece is a real one
// (positive quantity, non-negative cost), the pieces' quantities sum to the
// quantity that moved, and their costs sum to the basis that moved.
//
// A breakdown that does not add up means a corrupted journal, and the engine
// refuses the whole computation rather than working around it. The write path
// derives the operation's own basis by summing these very pieces (see
// operation.Service.CreateTransfer and LotsCost), so the two can only disagree
// if the stored rows were damaged afterwards — at which point neither reading
// is trustworthy: the pieces may be wrong, or the total may be, and nothing
// here can tell which. The tempting alternative, quietly falling back to a
// single lot dated on the transfer day, would replace damaged data with a
// plausible number that looks exactly like a normal old-style transfer, so
// nobody would ever learn the journal is broken — and the basis behind every
// figure derived from it would be silently wrong. Loud is the point.
func checkTransferLots(o Operation) error {
	qty := decimal.Zero
	var cost int64
	for i, pc := range o.TransferLots {
		if !pc.Quantity.IsPositive() {
			return badOp(o, fmt.Sprintf("transfer lot %d has quantity %s: every piece must be a positive quantity", i, pc.Quantity))
		}
		if pc.CostMinor < 0 {
			return badOp(o, fmt.Sprintf("transfer lot %d has cost %d: a piece's cost basis cannot be negative", i, pc.CostMinor))
		}
		qty = qty.Add(pc.Quantity)
		cost += pc.CostMinor
	}
	if !qty.Equal(*o.Quantity) {
		return badOp(o, fmt.Sprintf("transfer lots sum to quantity %s, but the operation moves %s", qty, *o.Quantity))
	}
	if cost != o.AmountMinor {
		return badOp(o, fmt.Sprintf("transfer lots sum to cost %d, but the operation carries %d", cost, o.AmountMinor))
	}
	return nil
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
// FIFO order (see ReleasedLot). The transfer service stores this breakdown
// with the destination account's operation, so the moved lots keep their own
// purchase dates instead of collapsing into the transfer date — which is
// what a single carried number forces.
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
// after folding the given journal, without mutating anything, for callers
// that need the total and not the breakdown. The transfer service is not one
// of them any more: it stores the pieces and sums them itself. Kept as a thin
// sum over ReleasedLots, so the two can never drift apart.
func ReleasedCost(ops []Operation, instrumentID uuid.UUID, qty decimal.Decimal) (int64, error) {
	pieces, err := ReleasedLots(ops, instrumentID, qty)
	if err != nil {
		return 0, err
	}
	return LotsCost(pieces), nil
}
