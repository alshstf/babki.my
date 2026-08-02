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
// produces one lot that DOES NOT KNOW when it was acquired: the original dates
// do not exist to be restored, and the transfer's own date is not one of them.
// Dating such a lot on the day the shares changed brokers would state, in the
// same field and the same format as a real purchase date, something nobody
// ever recorded — and every figure struck at that date afterwards (the ruble
// basis, above all) would be an invention indistinguishable from a fact. An
// unknown date is therefore carried as unknown, all the way through (see
// Lot.AcquiredOn).
//
// FIFO here means first by ACQUISITION, not first by arrival. The release queue
// is ordered by the day each lot was bought, wherever in the journal that lot
// is mentioned, so a transfer carrying older shares takes its place among the
// ones already held instead of queuing behind them. This is what every
// jurisdiction the owner files in actually says — НК РФ ст. 214.1 п. 13
// releases "по стоимости первых по времени приобретений", 26 CFR
// 1.1012-1(c)(1)(i) names "the earliest lot the taxpayer purchased or
// acquired" — and it follows from the paragraph above: if moving shares between
// one's own accounts is not a purchase, it cannot decide what is sold first
// either. Lots whose acquisition is unknown lead the queue, and lots acquired
// on the same day keep the order they entered the account (see addLot for both
// rules and for why the tie-break is spelled out at all).
//
// Quantities are tracked to QuantityScale decimal places, the same scale the
// journal stores them at, so a split truncates its product rather than letting
// a position hold a quantity that could never be written down. Two consequences
// are worth knowing. Truncation does not compose: a reverse split followed by a
// forward one can land a hair below what the pre-truncation engine computed, so
// a full-position sell recorded by an older build may no longer fit the
// position it was meant to empty — the refusal is loud, and deleting and
// re-entering that operation resolves it. And a deep enough reverse split can
// leave a lot with no quantity but a live cost basis; it keeps its place in
// the ACQUISITION queue exactly as if it still held shares — first only when
// the lot itself has no acquisition date, otherwise a release still has to
// work through every lot acquired earlier before reaching it — and if nothing
// follows it, the position keeps a basis it can no longer sell. Writing that
// basis off would treat a split as a disposal, which it is not.
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
	"slices"
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

// QuantityScale is how many decimal places the journal keeps for a quantity:
// operations.quantity and operation_transfer_lots.quantity are both
// NUMERIC(30,10) (see the migrations).
//
// It lives in the engine rather than next to the SQL because it is not merely a
// column width. It is the precision the ledger itself works at, and a position
// is nothing but a fold of ledger entries — so a position holding a quantity
// finer than the ledger can name is a position no entry can ever fully release:
// "sell everything I hold" cannot be written down, and whatever IS written down
// is not the number that was checked. Keeping positions on this scale is
// therefore a property of the journal-to-positions contract, and it is the
// engine that has to hold it (see Position.applySplit, the only place a
// quantity can leave the scale).
//
// Naming a scale is not knowing about a database: Compute still takes a journal
// and returns positions, and touches nothing else.
const QuantityScale = 10

// Lot is one acquisition that is still (partly) held: the quantity left of
// it, the cost basis still attributed to that quantity, and the day it was
// acquired. AcquiredOn is what lets a caller value each lot at the exchange
// rate of its own purchase date instead of one rate for the whole position;
// a transfer_in rebuilds one lot per piece of its stored breakdown, each
// keeping the day it was bought.
//
// AcquiredOn IS NIL WHEN THE DATE IS NOT KNOWN, which is a real and permanent
// state, not a temporary gap: a transfer with no stored breakdown carries a
// basis whose purchase dates were never recorded (see the package doc), and
// nothing can recover them later. A buy always knows its date, so nil never
// means "not filled in yet". It is also what places the lot in the release
// queue, and an unknown date places it at the head — see addLot.
//
// The absence is a pointer rather than a zero time.Time on purpose. A zero
// time.Time is a perfectly usable date — the first of January, year 1 — so
// every date operation accepts it and answers confidently: it sorts before
// every real purchase, formats as a plausible-looking string, and asks the fx
// tables for a rate as if year 1 were a Tuesday. "Unknown" would then travel
// disguised as "very long ago", and each place that forgot to check would be
// silently, unfalsifiably wrong. A nil pointer cannot be quietly mistaken for
// a date: code that ignores the case panics on the spot, at the line that
// ignored it, instead of publishing a number nobody can tell apart from a
// correct one. Every caller must decide what an unknown date means for it, and
// the answer is never a substitute date (see Handler.positionInBase, which
// publishes nothing rather than a partial basis).
//
// Quantity is positive for every lot an acquisition creates, but can be zero
// afterwards: a reverse split deep enough to round a lot's whole holding away
// leaves it with no shares and its cost basis intact (see applySplit). The lot
// stays in the queue — the money is real and belongs to the day it was spent.
type Lot struct {
	Quantity   decimal.Decimal
	CostMinor  int64
	AcquiredOn *time.Time
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
	// Lots are the acquisitions still held, ordered by the day each was
	// ACQUIRED — oldest first — which is the order releases consume them
	// (FIFO). Not by the order they entered this account: a transfer_in brings
	// in lots bought before ones already held, and they take their place among
	// them. Moving shares between one's own accounts is not a purchase and does
	// not restart anything, so the queue must not be built out of when the
	// paperwork happened (see addLot, which maintains this order, for the rule
	// and for the law behind it). Lots that do not know when they were acquired
	// stand at the head, ahead of every dated one; ties keep the order the lots
	// entered the account.
	//
	// Their quantities sum to Quantity and their costs sum to CostMinor
	// exactly. A position closed by selling everything has none; one whose
	// shares were rounded away by a reverse split keeps the shareless lots that
	// still hold the money spent on them (see Lot and applySplit).
	Lots []Lot
}

func badOp(o Operation, msg string) error {
	return fmt.Errorf("%w: %s %s: %s", ErrBadOperation, o.Type, o.OccurredOn.Format("2006-01-02"), msg)
}

// ReleasedLot is one piece of a FIFO release: the quantity taken from a
// single source lot, the cost basis attributed to that quantity, and the day
// the source lot was acquired (see Lot.AcquiredOn — the same rules apply: a
// partial piece inherits its lot's date, a lot that itself arrived by transfer
// passes on whatever date it carries, and nil means the source lot does not
// know when it was acquired, which a release copies rather than resolves). A
// release that spans several lots yields several pieces, in the order the lots
// are consumed.
type ReleasedLot struct {
	Quantity   decimal.Decimal
	CostMinor  int64
	AcquiredOn *time.Time
}

// releaseFIFO removes qty from the position's lots front-to-back and returns
// the pieces released, in queue order — which is acquisition order, oldest
// first, undated lots ahead of all of them (see Position.Lots; addLot is what
// keeps the queue in that order, so nothing has to be sorted here). Partial lot
// pieces use floor proportioning; the final piece of a lot takes the lot's
// remaining cost so that the sum of released piece costs always equals the
// original lot cost exactly.
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

// applySplit multiplies every lot by ratio — a split rewrites quantities and
// nothing else, neither a lot's cost basis nor the day it was acquired — and
// brings the results back onto the journal's quantity scale.
//
// Multiplying is the one thing the engine does that can carry a quantity off
// that scale: a reverse split by 0.3333333333 turns 3.5 shares into
// 1.16666666655, eleven decimal places for a lot that arrived with one. Left
// there, the extra digit is not a display detail but a trap. The position would
// hold a quantity no journal entry can name, so "sell all of it" would be
// checked against the exact number in memory, recorded as the rounded one, and
// every later read would compare that recorded quantity against the exact
// position and find an oversell: a 201 followed by a positions screen that
// answers 422 forever, for rows the application wrote itself. The same fault
// reached the transfer breakdown first and was patched there
// (operation.quantizeLots); this is where it comes from, and fixing it here
// means no quantity anywhere in the system is finer than the ledger — the
// journal's own entries never are, so nothing else can introduce one.
//
// The allocation is the one releaseFIFO already uses for costs: truncate the
// RUNNING TOTAL to the scale and give each lot the difference from the previous
// lot's running total, the last lot taking whatever is left of the position's
// own new quantity. Every lot is then exactly representable, the lots still sum
// to the position exactly rather than approximately, and the rounding is always
// DOWN — a ledger may lose a ten-billionth of a share to arithmetic it cannot
// express, but it must never invent one.
//
// A lot whose entire share rounds away is kept, with a quantity of zero, rather
// than dropped: its COST is real money, and the day it was bought is what
// values that money in another currency (see Lot.AcquiredOn). Dropping it would
// have to move that cost onto some other lot's day, which is exactly the
// re-dating this package exists to prevent. It holds no shares and waits in the
// queue until a release consumes it.
func (p *Position) applySplit(ratio decimal.Decimal) {
	total := p.Quantity.Mul(ratio).Truncate(QuantityScale)
	exact, placed := decimal.Zero, decimal.Zero
	for i := range p.Lots {
		exact = exact.Add(p.Lots[i].Quantity.Mul(ratio))
		upTo := exact.Truncate(QuantityScale)
		if i == len(p.Lots)-1 {
			// The lots sum to the position, so truncating the final running
			// total yields exactly this anyway. Saying it outright keeps the two
			// equal by construction instead of by argument.
			upTo = total
		}
		p.Lots[i].Quantity = upTo.Sub(placed)
		placed = upTo
	}
	p.Quantity = total
}

// acquiredBefore reports whether a lot acquired on a must leave the queue
// ahead of one acquired on b, for two dates that are not equal.
//
// An UNKNOWN acquisition comes before every known one. That is not a
// convenience: it is the rule 26 CFR 1.6045A-1(b)(10) settles for exactly this
// case — transferred securities that arrived without their acquisition dates
// are treated as disposed of first, ahead of every dated lot. It is also the
// only placement that does not require inventing a date. Anywhere else in the
// queue is defined by comparing the unknown against real days, which means
// quietly choosing a day for it — the very thing Lot.AcquiredOn exists to
// refuse. And it has a practical edge: sales drain the undated lots out of an
// account first, after which the position can be valued in another currency
// again (Handler.positionInBase publishes nothing while a single lot is
// undated).
//
// Dates are calendar days at UTC midnight — occurred_on and acquired_on are
// both DATE columns — so comparing instants compares days, as CheckTransferLots
// already does.
func acquiredBefore(a, b *time.Time) bool {
	switch {
	case a == nil:
		return b != nil
	case b == nil:
		return false
	default:
		return a.Before(*b)
	}
}

// addLot puts one acquisition into the queue AT ITS PLACE BY ACQUISITION DATE,
// which is what makes the queue a FIFO over purchases rather than over
// paperwork (see Position.Lots). A nil acquiredOn is not a missing argument but
// an answer: this lot's acquisition date is not knowable (see Lot.AcquiredOn),
// and such a lot goes to the head (see acquiredBefore).
//
// The order is maintained here, as an invariant of the queue, rather than
// established by sorting somewhere later. addLot is the only door a lot can
// enter a position through — a buy and both branches of a transfer_in all come
// through it — so making it the place the order is decided means no caller can
// build an out-of-order queue, and no future caller can forget to. Sorting at
// release time instead would have to be repeated in releaseFIFO, in
// ReleasedLots and in every path a later reader adds that takes the head of the
// queue; the one that forgot would silently fall back to arrival order, which
// is precisely the bug this replaced. Nothing else here reorders: releaseFIFO
// takes from the head and shrinks a lot in place, applySplit rewrites
// quantities in place, drainLotsCost rewrites costs in place — front-to-back,
// the same head this queue now hands to releaseFIFO. So an amortization
// drains whichever lot is oldest by ACQUISITION, not whichever lot the
// journal happened to mention first (see
// TestAmortizationDrainsTheOlderTransferredLotFirst) — not because
// drainLotsCost was taught the new rule, but because it never had a rule of
// its own: it just walks p.Lots from index 0, so whatever order addLot
// leaves the queue in is the order amortization drains it in too.
//
// TIES KEEP THE ORDER THE LOTS ENTERED THE ACCOUNT. The law names no rule finer
// than the day — НК РФ ст. 214.1 п. 13 says "по стоимости первых по времени
// приобретений", 26 CFR 1.1012-1(c)(1)(i) says "the earliest lot the taxpayer
// purchased or acquired", and neither says anything about two lots bought on
// one day — so the tie-break is ours to pick, and it must be picked explicitly:
// these figures go into a tax return, and a number that depends on which
// sorting algorithm the build happened to use is not a number anybody can
// defend. Journal order is the choice because it is total and already recorded
// rather than derived — Compute is fed operations ordered by (occurred_on,
// created_at), both facts in the table — and because it is what the queue did
// before dates entered it, so this change moves exactly the lots the
// acquisition rule requires to move and no others.
//
// The insertion point is found by walking back from the tail while the lot
// behind is strictly later, so the new lot lands AFTER every lot acquired no
// later than itself. That is exactly where a STABLE sort by acquisition date
// would put it, which means the invariant and "stable sort of the arrival
// sequence" are the same order, reached two ways. The walk costs nothing in the
// ordinary case: the journal arrives in date order, so a purchase stops at the
// first comparison and appends. Only a lot that arrives out of order — a
// transfer carrying older shares, the case this exists for — walks, and only as
// far as it must.
func (p *Position) addLot(qty decimal.Decimal, costMinor int64, acquiredOn *time.Time) {
	at := len(p.Lots)
	for at > 0 && acquiredBefore(acquiredOn, p.Lots[at-1].AcquiredOn) {
		at--
	}
	p.Lots = slices.Insert(p.Lots, at, Lot{Quantity: qty, CostMinor: costMinor, AcquiredOn: acquiredOn})
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
			// A purchase always knows its date: it is the day the operation
			// itself records. The local copy is what the lot points at, so the
			// lot never aliases the journal entry it came from.
			boughtOn := o.OccurredOn
			p.addLot(*o.Quantity, -o.AmountMinor+o.FeeMinor, &boughtOn)
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
			//
			// o.TransferLots is deliberately unused here even though this leg
			// carries it too (see Operation.TransferLots): the departing
			// account holds the real lots and must release its own, replaying
			// its own history. The breakdown rides along for readers that need
			// to know what the moved basis is made of — the journal converts
			// this row into the base currency piece by piece — and folding it
			// into the position here would double-count the very lots being
			// released.
		case TypeTransferIn:
			if o.AmountMinor < 0 {
				return nil, badOp(o, "transfer_in amount (cost basis) must be >= 0")
			}
			if len(o.TransferLots) == 0 {
				// No breakdown: the basis was given by hand (nothing was
				// released, so there are no source lots behind that number) or
				// the transfer was recorded before breakdowns were kept. Either
				// way the original purchase dates do not exist to be restored,
				// so the lot is created WITHOUT one.
				//
				// The transfer's own date used to be put here as "the honest
				// best answer". It is not an answer at all: shares that changed
				// brokers on that day were not bought on it, and the field says
				// bought. Written down, that guess became indistinguishable from
				// the real dates beside it — the ruble basis converted it at the
				// transfer day's fx rate and published the product as fact, and
				// the release queue sorted the parcel by a day that describes
				// paperwork rather than a purchase. Absence is the only truthful
				// value here, and everything downstream now has to face it.
				p.addLot(*o.Quantity, o.AmountMinor, nil)
				break
			}
			if err := CheckTransferLots(o); err != nil {
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
			// A split rewrites quantities only — see applySplit, which also
			// keeps the rewritten quantities expressible in the journal they
			// will be compared against.
			p.applySplit(*o.SplitRatio)
		default:
			return nil, badOp(o, "type not applicable to instrument operations")
		}
	}
	return positions, nil
}

// CheckTransferLots verifies that a transfer_in's stored FIFO breakdown and
// the operation carrying it describe the same event: every piece is a real one
// (positive quantity, non-negative cost), the pieces' quantities sum to the
// quantity that moved, and their costs sum to the basis that moved.
//
// A breakdown that does not add up means a corrupted journal, and the engine
// refuses the whole computation rather than working around it. The tempting
// alternative, quietly falling back to a single lot dated on the transfer day,
// would replace damaged data with a plausible number that looks exactly like a
// normal old-style transfer, so nobody would ever learn the journal is broken
// — and the basis behind every figure derived from it would be silently wrong.
// Loud is the point.
//
// Loud on damage, though, is only defensible if healthy data can never trip
// it, and getting there took more than summing the pieces. The costs are
// int64 and the write path derives the operation's basis by summing these very
// pieces (see operation.Service.CreateTransfer and LotsCost), so that half has
// always been exact. The QUANTITIES were not: they are stored with ten decimal
// places, while a piece computed in memory had no such limit — a reverse split
// multiplied lot quantities by a ratio like 0.3333333333 and landed well past
// the tenth digit. Each piece was then rounded on its own on the way into the
// table, and two pieces rounding up put the stored sum a whole 1e-10 above the
// stored quantity: a perfectly legitimate transfer, accepted with a 201, after
// which this function failed forever and took the receiving account's entire
// positions screen down with it.
//
// That is now closed at the source rather than patched per path: applySplit
// keeps every lot on the journal's own scale (see QuantityScale), so a release
// of scale-bound lots yields scale-bound pieces and there is nothing left to
// round. Three guards remain behind it, each cheap and each a different kind of
// insurance: the breakdown is quantized as it is built (operation.quantizeLots),
// the moved quantity is normalized to the scale on the way in, and the store
// re-reads its own rows and runs them through this very function before
// committing (see operation.Store.CreatePair). What is written is therefore
// exactly what is read back, and a mismatch here now genuinely means the rows
// were damaged after the fact — at which point neither reading is trustworthy:
// the pieces may be wrong, or the total may be, and nothing here can tell
// which.
//
// An operation with no breakdown at all is not this function's business — see
// the transfer_in branch in Compute for why that case is legitimate — and
// callers must not pass one; it would be reported as a mismatch against a
// non-zero quantity.
func CheckTransferLots(o Operation) error {
	qty := decimal.Zero
	var cost int64
	for i, pc := range o.TransferLots {
		if !pc.Quantity.IsPositive() {
			return badOp(o, fmt.Sprintf("transfer lot %d has quantity %s: every piece must be a positive quantity", i, pc.Quantity))
		}
		if pc.CostMinor < 0 {
			return badOp(o, fmt.Sprintf("transfer lot %d has cost %d: a piece's cost basis cannot be negative", i, pc.CostMinor))
		}
		// The acquisition date is the one field this whole mechanism exists to
		// carry, and it is the only one the table does not constrain: the
		// columns bound quantity and cost, this function checks their sums, and
		// the date travels guarded only from here.
		//
		// A piece with NO date is legitimate and must pass. It says the source
		// lot did not know when it was acquired — which happens whenever the
		// parcel contains shares that themselves arrived by a transfer with no
		// recoverable dates (see the transfer_in branch in Compute), and moving
		// them on a second time cannot conjure the dates the first move already
		// lacked. Refusing it, as this function once did, would have forced the
		// write path to supply some date for such a piece, and the only date on
		// hand is the transfer's own — the very invention the absence exists to
		// avoid.
		//
		// A date that IS given may not postdate the transfer that moved it: the
		// source lots are resolved against the journal as it stood on the
		// transfer's own date, so anything later is damage. Unknown is not
		// "later" and not "earlier"; it is simply outside what this check can
		// speak about.
		if pc.AcquiredOn != nil && pc.AcquiredOn.After(o.OccurredOn) {
			return badOp(o, fmt.Sprintf("transfer lot %d was acquired on %s, after the transfer on %s: a lot cannot move before it exists",
				i, pc.AcquiredOn.Format("2006-01-02"), o.OccurredOn.Format("2006-01-02")))
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
// that need the total and not the breakdown.
//
// NOTHING IN PRODUCTION CALLS IT any more: the transfer service was its one
// caller and now stores the pieces and sums them itself (see ReleasedLots and
// LotsCost). It survives, and stays exported, purely as a test oracle: it is
// the "what should the whole release cost" side of
// TestReleasedLotsSumMatchesReleasedCost, which pins that a breakdown never
// drifts from the total it must add up to. Today it cannot drift, because this
// is deliberately a thin sum over ReleasedLots; the test earns its keep the
// day anyone computes either side a second, independent way — exactly the
// change that would otherwise slip through. Do not mistake it for a live path
// and do not build one on it.
func ReleasedCost(ops []Operation, instrumentID uuid.UUID, qty decimal.Decimal) (int64, error) {
	pieces, err := ReleasedLots(ops, instrumentID, qty)
	if err != nil {
		return 0, err
	}
	return LotsCost(pieces), nil
}
