// Package portfolio computes positions from the operations journal.
//
// The engine is pure: it folds one account's operations, in journal order, into
// per-instrument positions and touches no database, so positions are always
// recomputable from the journal.
//
// Transfers. When a transfer pair is created, the lots it consumes are resolved
// once (ReleasedLots) and stored with the transfer_in. That breakdown is the
// record of what moved, and both legs fold from it: the receiving account
// rebuilds those lots with their purchase dates, and the sending account gives
// up exactly those lots (Position.releaseRecorded). Re-deriving the release on
// the sending side made pairs disagree once the queue rule changed (#60). A
// transfer without a breakdown creates one lot with an unknown acquisition
// date: the transfer's own date is not a purchase date (see Lot.AcquiredOn).
//
// FIFO is by acquisition date, not by arrival: a transfer carrying older shares
// takes its place among those already held (НК РФ ст. 214.1 п. 13 — «по
// стоимости первых по времени приобретений»; 26 CFR 1.1012-1(c)(1)(i)). Lots
// of unknown date lead; same-day lots keep journal order (see addLot).
//
// Quantities are kept at QuantityScale, the journal's own scale, so a split
// truncates rather than producing a quantity no entry can name. Truncation does
// not compose, so a full sale recorded by an older build after a
// reverse-then-forward split may no longer fit and is refused. A deep reverse
// split can leave a lot with no shares but real cost; it keeps its place in the
// queue and its money leaves with the position's last unit (sweepShareless).
//
// A position's cost is in one currency, settled by the first operation that
// touches cost, quantity or fees; income is kept per currency, since Russian
// brokers pay coupons and dividends in roubles on papers bought in other
// currencies (see Position.IncomeByCurrency).
package portfolio

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/platform/money"
)

var (
	ErrOversell = errors.New("not enough quantity")
	// ErrBadOperation means a journal entry violates the engine's invariants.
	ErrBadOperation = errors.New("invalid operation")
)

// QuantityScale is the decimal places the journal keeps for a quantity
// (NUMERIC(30,10)). Positions are kept at this scale so every holding can be
// named by a journal entry; applySplit is the only place a quantity could leave
// it.
const QuantityScale = 10

// Lot is one acquisition still (partly) held: its remaining quantity, the cost
// attributed to it, and the day it was acquired, which values it at that day's
// rate in another currency.
//
// AcquiredOn is nil when the date is not known — permanently, as for a transfer
// without a breakdown — never a placeholder. It is a pointer so "unknown"
// cannot pass for a real date (a zero time.Time sorts and converts like one);
// callers must decide what unknown means and never substitute a date. Such lots
// lead the queue (see addLot).
//
// Quantity can be zero after a deep reverse split; the lot keeps its cost and
// its day.
type Lot struct {
	// ID is the lot's permanent number (see LotID); zero when the operation
	// that brought it has no identity yet.
	ID         LotID
	Quantity   decimal.Decimal
	CostMinor  int64
	AcquiredOn *time.Time
	// RateOn is the day whose official rate prices the lot's cost: the purchase's
	// settlement day when known (decision Р-3, НК РФ ст. 210 п. 5), nil to use
	// AcquiredOn.
	RateOn *time.Time
}

// CurrencyMinor is an amount of minor units with their currency, for figures
// that may be in several currencies.
type CurrencyMinor struct {
	Currency string
	Minor    int64
}

// Position is the running state of one instrument within one account. Closed
// positions are kept for their realized result and income.
type Position struct {
	InstrumentID uuid.UUID
	// Currency is the currency of the position's cost and quantity: CostMinor, the
	// lots, and what Realizations are made of. Income may be in others.
	//
	// It is settled by the first operation touching those figures
	// (Type.mustMatchPositionCurrency), which every later one must repeat. A
	// position that has seen only income keeps a provisional currency — the lowest
	// code among its payments, IncomeByCurrency[0].Currency, chosen for
	// determinism. It then only labels the row: there is no cost for it to be the
	// currency of, and it must not be read as the paper's currency.
	Currency  string
	Quantity  decimal.Decimal
	CostMinor int64 // remaining FIFO cost basis (fees capitalized on buy)
	// IncomeByCurrency is what the paper paid — dividends and coupons less taxes
	// on them and the commissions charged on it outside a trade — one entry per
	// currency, ordered by code (deterministic, unlike map order). A yuan bond's
	// rouble coupon stays in roubles; the engine holds no rates. An entry may be
	// zero or negative (a tax on a payment before the journal starts). addIncome
	// is its only writer.
	IncomeByCurrency []CurrencyMinor
	// FeesByCurrency is every commission charged, per currency and ordered like
	// IncomeByCurrency: a sale of a yuan bond is charged in roubles. A purchase's
	// is also in the lot's cost, a sale's in its realization, and one charged
	// outside a trade in the income.
	FeesByCurrency []CurrencyMinor
	// Lots are the acquisitions still held, oldest acquisition first, which is
	// the release order; undated lots lead, ties keep journal order (see addLot).
	// Their quantities and costs sum exactly to Quantity and CostMinor.
	Lots []Lot
	// Realizations are the disposals behind the realized result, in journal order,
	// each recorded as what it was made of (see Realization). A transfer out is
	// not a disposal and produces none.
	Realizations []Realization
	// heldALot records that this account acquired the paper at some point (a
	// purchase or an arriving transfer). It is set by addLot, the only way a lot
	// enters, and decides whether an amortization is believable.
	heldALot bool
	// realizedPnLMinor and realizedInOneCurrency are what RealizedPnL returns,
	// written only by finishRealized.
	realizedPnLMinor      int64
	realizedInOneCurrency bool
}

// realize records one disposal; the total is settled by finishRealized.
func (p *Position) realize(r Realization) {
	p.Realizations = append(p.Realizations, r)
}

// RealizedPnL is the position's realized result in its currency, and whether
// there is one: false when a disposal settled in another currency (a yuan
// bond redeemed for roubles), since the difference is a quantity of neither
// and the engine holds no rates. Callers holding rates use Realizations. No
// disposals is 0 and true.
func (p *Position) RealizedPnL() (minor int64, inOneCurrency bool) {
	return p.realizedPnLMinor, p.realizedInOneCurrency
}

// finishRealized settles the realized total after the fold, when
// Position.Currency is final; mid-fold it may still be provisional. The sum is
// overflow-checked.
func (p *Position) finishRealized() error {
	var sum int64
	for _, r := range p.Realizations {
		if r.Currency != p.Currency {
			// One foreign disposal makes the total inexpressible; a partial sum would
			// understate it.
			p.realizedPnLMinor, p.realizedInOneCurrency = 0, false
			return nil
		}
		next, err := money.Add(sum, r.PnLMinor())
		if err != nil {
			return fmt.Errorf("%w: realized result of instrument %s in %s, adding %d to %d",
				err, p.InstrumentID, p.Currency, r.PnLMinor(), sum)
		}
		sum = next
	}
	p.realizedPnLMinor, p.realizedInOneCurrency = sum, true
	return nil
}

// findInCurrencyList binary-searches a per-currency list and reports where the
// currency is, or where it would be inserted. Every lookup by currency uses
// it, so the ordering is defined once.
func findInCurrencyList(list []CurrencyMinor, currency string) (int, bool) {
	return slices.BinarySearchFunc(list, currency, func(e CurrencyMinor, c string) int {
		return strings.Compare(e.Currency, c)
	})
}

// addToCurrencyList books minor units into a per-currency list, ordered by code
// with one entry per currency, overflow-checked; what names the total in the
// error. It is the only writer of IncomeByCurrency and FeesByCurrency.
func addToCurrencyList(list []CurrencyMinor, currency string, minor int64, what string) ([]CurrencyMinor, error) {
	at, found := findInCurrencyList(list, currency)
	if !found {
		return slices.Insert(list, at, CurrencyMinor{Currency: currency, Minor: minor}), nil
	}
	sum, err := money.Add(list[at].Minor, minor)
	if err != nil {
		return nil, fmt.Errorf("%w: %s in %s, adding %d to %d", err, what, currency, minor, list[at].Minor)
	}
	list[at].Minor = sum
	return list, nil
}

// minorInCurrencyList is one currency's entry, zero if absent.
func minorInCurrencyList(list []CurrencyMinor, currency string) int64 {
	at, found := findInCurrencyList(list, currency)
	if !found {
		return 0
	}
	return list[at].Minor
}

// addIncome books income in the currency it arrived in. The addition is
// overflow-checked, and a refused payment leaves the list unchanged.
func (p *Position) addIncome(currency string, minor int64) error {
	list, err := addToCurrencyList(p.IncomeByCurrency, currency, minor, "income")
	if err != nil {
		return err
	}
	p.IncomeByCurrency = list
	return nil
}

// addFee books a commission in the currency it was charged in.
func (p *Position) addFee(currency string, minor int64) error {
	list, err := addToCurrencyList(p.FeesByCurrency, currency, minor, "fees")
	if err != nil {
		return err
	}
	p.FeesByCurrency = list
	return nil
}

// IncomeMinorIn is the income booked in one currency, zero if none (or if the
// payments cancel). Callers publishing it must name its currency.
func (p *Position) IncomeMinorIn(currency string) int64 {
	return minorInCurrencyList(p.IncomeByCurrency, currency)
}

// FeesMinorIn is the commission charged in one currency, zero if none.
func (p *Position) FeesMinorIn(currency string) int64 {
	return minorInCurrencyList(p.FeesByCurrency, currency)
}

func badOp(o Operation, msg string) error {
	return fmt.Errorf("%w: %s %s: %s", ErrBadOperation, o.Type, o.OccurredOn.Format("2006-01-02"), msg)
}

// ReleasedLot is one piece of a FIFO release: the quantity taken from one lot,
// its cost, and the lot's acquisition day (nil copied as nil). A release
// across several lots yields pieces in consumption order.
type ReleasedLot struct {
	// From is the lot the piece was taken from; zero on a piece recorded before
	// lots had numbers, which is then matched by its acquisition day.
	From       LotID
	Quantity   decimal.Decimal
	CostMinor  int64
	AcquiredOn *time.Time
	// RateOn is the source lot's (see Lot.RateOn).
	RateOn *time.Time
}

// Realization is one disposal recorded by what it was made of, so it can be
// valued in another currency: proceeds and fee at the disposal's rate, each
// released piece at the rate of its own purchase (НК РФ ст. 210 п. 5).
//
// Released may be empty (an amortization after the basis is spent is pure
// gain), and a piece may have no acquisition date; callers decide what to
// publish then, and nothing substitutes a date.
type Realization struct {
	// OccurredOn is the disposal's day. It dates the proceeds and fee, not the
	// released basis.
	OccurredOn time.Time
	// RateOn is the day whose rate prices the proceeds and fee: the sale's
	// settlement day when known (decision Р-3), nil to use OccurredOn.
	RateOn *time.Time
	// ProceedsMinor is what came in; positive.
	ProceedsMinor int64
	// Currency is what the proceeds and fee arrived in, not necessarily the
	// position's: a yuan bond is redeemed for roubles. An amortization must be in
	// the position's currency, since it retires basis by amount, which would need
	// a rate; a sale retires by quantity (see Operation.mustMatchPositionCurrency).
	Currency string
	// FeeMinor is the disposal's fee in Currency; zero for an amortization.
	FeeMinor int64
	// Released is the basis given up, one piece per source lot in queue order.
	// An amortization's pieces carry no quantity.
	Released []ReleasedLot
}

// PnLMinor is the disposal's result in the position's currency, the only
// definition of it.
func (r Realization) PnLMinor() int64 {
	return r.ProceedsMinor - r.FeeMinor - LotsCost(r.Released)
}

// releaseFIFO takes qty from the head of the queue and returns the pieces in
// order. A partial piece takes the floor of its proportional cost; a whole lot
// takes its remaining cost, so piece costs sum exactly to the lots'.
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
			// A wholly consumed lot yields a piece even at zero cost, unlike drainLotsCost:
			// these pieces become a transfer's breakdown, whose quantities
			// CheckTransferLots sums. The cost: an undated zero-cost piece makes the
			// disposal's rouble figure unavailable, though it needs no rate.
			pieces = append(pieces, ReleasedLot{From: l.ID, Quantity: l.Quantity, CostMinor: l.CostMinor, AcquiredOn: l.AcquiredOn, RateOn: l.RateOn})
			released += l.CostMinor
			remaining = remaining.Sub(l.Quantity)
			p.Lots = p.Lots[1:]
			continue
		}
		// Partial piece: floor share; the rest stays in the lot with its date.
		share := lotShare(*l, remaining)
		pieces = append(pieces, ReleasedLot{From: l.ID, Quantity: remaining, CostMinor: share, AcquiredOn: l.AcquiredOn, RateOn: l.RateOn})
		l.CostMinor -= share
		l.Quantity = l.Quantity.Sub(remaining)
		released += share
		remaining = decimal.Zero
	}
	p.Quantity = p.Quantity.Sub(qty)
	p.CostMinor -= released
	if p.Quantity.IsZero() {
		pieces = append(pieces, p.sweepShareless()...)
	}
	return pieces, nil
}

// sweepShareless empties a position that just lost its last unit: remaining
// lots hold no shares but may hold money (see applySplit), which leaves with
// this release, each piece under its own day.
func (p *Position) sweepShareless() []ReleasedLot {
	var pieces []ReleasedLot
	for _, l := range p.Lots {
		if l.CostMinor == 0 {
			continue
		}
		pieces = append(pieces, ReleasedLot{From: l.ID, Quantity: decimal.Zero, CostMinor: l.CostMinor, AcquiredOn: l.AcquiredOn, RateOn: l.RateOn})
		p.CostMinor -= l.CostMinor
	}
	p.Lots = nil
	return pieces
}

// lotShare is the cost taken with qty units of a lot: all of it when qty covers
// the lot (including a shareless lot), the floor of the proportional share
// otherwise, so a release never takes money that is not there. Both
// releaseFIFO and releaseRecorded use it, so a recorded breakdown replays
// exactly.
func lotShare(l Lot, qty decimal.Decimal) int64 {
	if !qty.LessThan(l.Quantity) {
		return l.CostMinor
	}
	return decimal.NewFromInt(l.CostMinor).Mul(qty).Div(l.Quantity).Floor().IntPart()
}

// recordAndReplayDisagree ends releaseRecorded's refusals. It names both
// causes: this build's writes cannot reach the refusal (every write replays
// first), so the usual cause is a transfer written by an earlier build, not an
// edit.
const recordAndReplayDisagree = "either this account's history was edited after the transfer was recorded, " +
	"or the transfer was recorded by a build whose release queue picked lots by a different rule; " +
	"delete the transfer and record it again either way"

// releaseRecorded gives up the lots a transfer's breakdown says left, rather
// than a fresh FIFO release, so both legs of a pair agree by construction.
//
// A breakdown whose pieces name their lots is released by number
// (releaseNumbered). One recorded before lots had numbers, and not yet given
// them (operation.NumberLegacyMoves), is matched by acquisition day: each
// piece takes units front-to-back among lots of its day, and each
// lot gives up the money that goes with its units (lotShare). Basis a piece
// carries beyond that — a shareless lot's money folded into the next piece by
// operation.quantizeLots — is drained from the head of the queue afterwards.
// Proportioning matters: clamping by a lot's whole cost would take a shareless
// lot's money from an innocent parcel of the same day.
//
// A piece whose day has no shares left is refused loudly (see
// recordAndReplayDisagree): taking units from another day would re-date them.
//
// A split entered after a transfer but dated before it still replays, and the
// recorded basis then comes off twice the shares it was struck against. The
// family's totals are right; re-entering the transfer evens the pair.
//
// Quantity and cost are conserved exactly (#60).
func (p *Position) releaseRecorded(o Operation) error {
	if o.Quantity.GreaterThan(p.Quantity) {
		return fmt.Errorf("%s %s %s: %w: have %s, need %s",
			o.Type, o.InstrumentID, o.OccurredOn.Format("2006-01-02"),
			ErrOversell, p.Quantity, *o.Quantity)
	}
	if numbered(o.TransferLots) {
		return p.releaseNumbered(o)
	}
	var carried int64 // recorded cost its own lots could not cover
	for i, pc := range o.TransferLots {
		qty, cost := pc.Quantity, pc.CostMinor
		if qty.IsZero() {
			// A piece of no units is a shareless parcel; its money comes only from
			// shareless lots of its own day.
			for j := range p.Lots {
				l := &p.Lots[j]
				if !l.Quantity.IsZero() || !sameAcquisition(l.AcquiredOn, pc.AcquiredOn) {
					continue
				}
				take := min(l.CostMinor, cost)
				l.CostMinor, p.CostMinor, cost = l.CostMinor-take, p.CostMinor-take, cost-take
			}
			if cost > 0 {
				return badOp(o, fmt.Sprintf(
					"transfer lot %d moved %d minor of basis held by a parcel with no units acquired %s, and replaying this account leaves %d of it with no such parcel to come from: %s",
					i, pc.CostMinor, acquisitionText(pc.AcquiredOn), cost, recordAndReplayDisagree))
			}
			continue
		}
		for j := range p.Lots {
			l := &p.Lots[j]
			if !sameAcquisition(l.AcquiredOn, pc.AcquiredOn) {
				continue
			}
			takeQty := decimal.Min(l.Quantity, qty)
			share := lotShare(*l, takeQty)
			// The record gives these units less money than their parcel holds: it was
			// struck against a cheaper parcel of the same day, now gone. Taking them
			// anyway would move them at the wrong price.
			if takeQty.IsPositive() && cost < share {
				return badOp(o, fmt.Sprintf(
					"transfer lot %d gives %s units acquired %s a basis of %d, but the parcel replaying this account finds for them holds %d for those units: %s",
					i, takeQty, acquisitionText(pc.AcquiredOn), cost, share, recordAndReplayDisagree))
			}
			takeCost := min(share, cost)
			l.Quantity, l.CostMinor = l.Quantity.Sub(takeQty), l.CostMinor-takeCost
			p.Quantity, p.CostMinor = p.Quantity.Sub(takeQty), p.CostMinor-takeCost
			qty, cost = qty.Sub(takeQty), cost-takeCost
			if qty.IsZero() {
				break
			}
		}
		if qty.IsPositive() {
			return badOp(o, fmt.Sprintf(
				"transfer lot %d moved %s units acquired %s, but replaying this account leaves %s of them with no such lot to come from: %s",
				i, pc.Quantity, acquisitionText(pc.AcquiredOn), qty, recordAndReplayDisagree))
		}
		carried += cost
	}
	if carried > 0 {
		if carried > p.CostMinor {
			return badOp(o, fmt.Sprintf(
				"the breakdown moves %d minor more basis than this account still holds (%d): %s",
				carried, p.CostMinor, recordAndReplayDisagree))
		}
		drainLotsCost(p, carried)
		p.CostMinor -= carried
	}
	// A lot with neither shares nor money is spent; one with money but no shares
	// stays (see applySplit).
	p.Lots = slices.DeleteFunc(p.Lots, func(l Lot) bool { return l.Quantity.IsZero() && l.CostMinor == 0 })
	return nil
}

// numbered reports a breakdown whose every piece names its lot; one recorded
// before lots had numbers is matched by day instead (releaseRecorded).
func numbered(pieces []ReleasedLot) bool {
	if len(pieces) == 0 {
		return false
	}
	for _, pc := range pieces {
		if pc.From.IsZero() {
			return false
		}
	}
	return true
}

// lotIndex is where the lot numbered id sits in the queue, -1 when it is gone.
func (p *Position) lotIndex(id LotID) int {
	return slices.IndexFunc(p.Lots, func(l Lot) bool { return l.ID == id })
}

// releaseNumbered gives up exactly the units and the money each piece says it
// took from the lot it names. The history changed under the record — refused,
// never taken from a neighbour — when the lot is gone, was acquired on another
// day than the piece says, holds less than the piece took, or holds more money
// for those units than the piece moves (that money would stay behind, as a
// restated purchase price does under a move recorded before it).
//
// A piece moving more money than its units' share is taken as recorded, from
// the same lot: a split entered after the move but dated before it leaves the
// piece's units a fraction of the lot's. The family's totals stay right;
// re-entering the move evens the pair.
func (p *Position) releaseNumbered(o Operation) error {
	for i, pc := range o.TransferLots {
		at := p.lotIndex(pc.From)
		if at < 0 {
			return badOp(o, fmt.Sprintf(
				"transfer lot %d moved %s units of parcel %s, which replaying this account no longer holds: %s",
				i, pc.Quantity, pc.From, recordAndReplayDisagree))
		}
		l := &p.Lots[at]
		if !sameAcquisition(l.AcquiredOn, pc.AcquiredOn) {
			return badOp(o, fmt.Sprintf(
				"transfer lot %d names parcel %s as acquired %s, and replaying this account finds it acquired %s: %s",
				i, pc.From, acquisitionText(pc.AcquiredOn), acquisitionText(l.AcquiredOn), recordAndReplayDisagree))
		}
		if pc.Quantity.GreaterThan(l.Quantity) || pc.CostMinor > l.CostMinor {
			return badOp(o, fmt.Sprintf(
				"transfer lot %d moved %s units and %d of basis from parcel %s, which replaying this account leaves holding %s units and %d: %s",
				i, pc.Quantity, pc.CostMinor, pc.From, l.Quantity, l.CostMinor, recordAndReplayDisagree))
		}
		if share := lotShare(*l, pc.Quantity); pc.CostMinor < share {
			return badOp(o, fmt.Sprintf(
				"transfer lot %d gives %s units of parcel %s a basis of %d, but the parcel holds %d for them: %s",
				i, pc.Quantity, pc.From, pc.CostMinor, share, recordAndReplayDisagree))
		}
		l.Quantity, l.CostMinor = l.Quantity.Sub(pc.Quantity), l.CostMinor-pc.CostMinor
		p.Quantity, p.CostMinor = p.Quantity.Sub(pc.Quantity), p.CostMinor-pc.CostMinor
	}
	p.Lots = slices.DeleteFunc(p.Lots, func(l Lot) bool { return l.Quantity.IsZero() && l.CostMinor == 0 })
	return nil
}

// acquisitionText renders an acquisition day, or its absence, for an error.
func acquisitionText(t *time.Time) string {
	if t == nil {
		return "on an unknown day"
	}
	return "on " + t.Format("2006-01-02")
}

// SpinoffPieces allocates a spin-off's moving basis: floor(total × share),
// divided among the lots by largest remainders so the pieces sum exactly. One
// piece per lot, in queue order, including lots giving nothing: the pieces
// record the lot list the allocation was struck against (applySpinoffOut).
// Floor keeps a half-unit with the original paper, as lotShare does.
// Remainders are compared exactly (decimal.QuoRem), ties to the earlier lot,
// so the allocation is deterministic.
func SpinoffPieces(lots []Lot, share decimal.Decimal) []ReleasedLot {
	pieces := make([]ReleasedLot, len(lots))
	var total int64
	for i, l := range lots {
		pieces[i] = ReleasedLot{From: l.ID, Quantity: l.Quantity, CostMinor: 0, AcquiredOn: l.AcquiredOn, RateOn: l.RateOn}
		total += l.CostMinor
	}
	if total <= 0 {
		// Nothing to divide: shares received with no basis move no money.
		return pieces
	}
	moved := decimal.NewFromInt(total).Mul(share).Floor().IntPart()
	if moved <= 0 {
		return pieces
	}
	totalDec := decimal.NewFromInt(total)
	movedDec := decimal.NewFromInt(moved)

	type remainder struct {
		at  int
		rem decimal.Decimal
	}
	rems := make([]remainder, 0, len(lots))
	var placed int64
	for i, l := range lots {
		if l.CostMinor <= 0 {
			continue
		}
		q, r := movedDec.Mul(decimal.NewFromInt(l.CostMinor)).QuoRem(totalDec, 0)
		pieces[i].CostMinor = q.IntPart()
		placed += pieces[i].CostMinor
		rems = append(rems, remainder{at: i, rem: r})
	}
	// The leftover — at most a unit per lot — goes to the largest remainders,
	// earlier lots first among equals.
	sort.SliceStable(rems, func(i, j int) bool { return rems[i].rem.GreaterThan(rems[j].rem) })
	for i := 0; placed < moved && i < len(rems); i++ {
		pieces[rems[i].at].CostMinor++
		placed++
	}
	return pieces
}

// CheckSpinoffLots checks that a spin-off leg's pieces are non-negative and
// sum to the row's basis. Quantities are not summed against the row: the
// departing leg moves no shares, and applySpinoffOut matches the pieces'
// counts against the lots. The arriving leg is checked by CheckTransferLots.
func CheckSpinoffLots(o Operation) error {
	if len(o.TransferLots) == 0 {
		return badOp(o, "a spin-off must carry the breakdown of the lots whose basis it moved")
	}
	var cost int64
	for i, pc := range o.TransferLots {
		if pc.Quantity.IsNegative() {
			return badOp(o, fmt.Sprintf("spin-off lot %d names a quantity of %s: a lot holds no negative number of units", i, pc.Quantity))
		}
		if pc.CostMinor < 0 {
			return badOp(o, fmt.Sprintf("spin-off lot %d gives up %d: a piece's cost basis cannot be negative", i, pc.CostMinor))
		}
		if pc.AcquiredOn != nil && pc.AcquiredOn.After(o.OccurredOn) {
			return badOp(o, fmt.Sprintf("spin-off lot %d was acquired on %s, after the spin-off on %s: a lot cannot give up basis before it exists",
				i, pc.AcquiredOn.Format("2006-01-02"), o.OccurredOn.Format("2006-01-02")))
		}
		cost += pc.CostMinor
	}
	if cost != o.AmountMinor {
		return badOp(o, fmt.Sprintf("spin-off lots sum to cost %d, but the operation moves %d", cost, o.AmountMinor))
	}
	return nil
}

// applySpinoffOut takes the recorded basis out of exactly the lots the record
// names, position by position, leaving quantities alone. A spin-off names the
// whole lot list, so a journal that has since gained, lost or changed a lot is
// refused (see recordAndReplayDisagree) rather than silently reallocated.
// Shareless lots are matched and drained like any other.
func (p *Position) applySpinoffOut(o Operation) error {
	if numbered(o.TransferLots) {
		return p.spinoffNumbered(o)
	}
	if len(o.TransferLots) != len(p.Lots) {
		return badOp(o, fmt.Sprintf(
			"the spin-off was struck against %d parcels and replaying this account leaves %d: %s",
			len(o.TransferLots), len(p.Lots), recordAndReplayDisagree))
	}
	for i, pc := range o.TransferLots {
		l := &p.Lots[i]
		if !sameAcquisition(l.AcquiredOn, pc.AcquiredOn) || !l.Quantity.Equal(pc.Quantity) {
			return badOp(o, fmt.Sprintf(
				"spin-off lot %d names %s units acquired %s and replaying this account leaves %s units acquired %s in its place: %s",
				i, pc.Quantity, acquisitionText(pc.AcquiredOn), l.Quantity, acquisitionText(l.AcquiredOn),
				recordAndReplayDisagree))
		}
		if pc.CostMinor > l.CostMinor {
			return badOp(o, fmt.Sprintf(
				"spin-off lot %d moves %d minor of basis out of a parcel that replaying this account leaves holding %d: %s",
				i, pc.CostMinor, l.CostMinor, recordAndReplayDisagree))
		}
		l.CostMinor -= pc.CostMinor
		p.CostMinor -= pc.CostMinor
	}
	return nil
}

// spinoffNumbered takes each piece's basis out of the lot it names. A lot that
// is gone is refused when the piece moves money from it, and passed over when
// it moves none.
func (p *Position) spinoffNumbered(o Operation) error {
	for i, pc := range o.TransferLots {
		at := p.lotIndex(pc.From)
		if at < 0 {
			if pc.CostMinor == 0 {
				continue
			}
			return badOp(o, fmt.Sprintf(
				"spin-off lot %d moves %d minor of basis out of parcel %s, which replaying this account no longer holds: %s",
				i, pc.CostMinor, pc.From, recordAndReplayDisagree))
		}
		l := &p.Lots[at]
		if !sameAcquisition(l.AcquiredOn, pc.AcquiredOn) {
			return badOp(o, fmt.Sprintf(
				"spin-off lot %d names parcel %s as acquired %s, and replaying this account finds it acquired %s: %s",
				i, pc.From, acquisitionText(pc.AcquiredOn), acquisitionText(l.AcquiredOn), recordAndReplayDisagree))
		}
		if pc.CostMinor > l.CostMinor {
			return badOp(o, fmt.Sprintf(
				"spin-off lot %d moves %d minor of basis out of parcel %s, which replaying this account leaves holding %d: %s",
				i, pc.CostMinor, pc.From, l.CostMinor, recordAndReplayDisagree))
		}
		l.CostMinor -= pc.CostMinor
		p.CostMinor -= pc.CostMinor
	}
	return nil
}

// LotsCost sums the pieces' costs, so callers derive the total from the
// breakdown rather than computing it separately.
func LotsCost(pieces []ReleasedLot) int64 {
	var total int64
	for _, pc := range pieces {
		total += pc.CostMinor
	}
	return total
}

// applySplit multiplies every lot's quantity by ratio — cost and dates are
// untouched — and brings the results back to QuantityScale.
//
// Without that, a reverse split by 0.3333333333 leaves quantities no journal
// entry can name, so "sell everything" is checked against one number and
// recorded as another, and every later read finds an oversell. The running
// total is truncated and each lot takes the difference, the last lot the rest,
// so lots sum exactly and rounding is always down. A lot rounded to no shares
// is kept with its cost and day.
func (p *Position) applySplit(ratio decimal.Decimal) {
	total := p.Quantity.Mul(ratio).Truncate(QuantityScale)
	exact, placed := decimal.Zero, decimal.Zero
	for i := range p.Lots {
		exact = exact.Add(p.Lots[i].Quantity.Mul(ratio))
		upTo := exact.Truncate(QuantityScale)
		if i == len(p.Lots)-1 {
			// The lots sum to the position; this keeps them equal by construction.
			upTo = total
		}
		p.Lots[i].Quantity = upTo.Sub(placed)
		placed = upTo
	}
	p.Quantity = total
}

// acquiredBefore reports whether a lot acquired on a leaves the queue before
// one acquired on b, for unequal dates. An unknown date comes first: the head
// is the only place that needs no invented date, and selling clears undated
// lots first. (26 CFR 1.6045A-1(b)(10) reports unknown-date securities as
// earliest, which corroborates but does not decide this.) Dates are UTC days.
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

// sameAcquisition reports whether two lots share an acquisition day, unknown
// equal to unknown; derived from acquiredBefore so the two cannot drift.
func sameAcquisition(a, b *time.Time) bool {
	return !acquiredBefore(a, b) && !acquiredBefore(b, a)
}

// addLot inserts a lot at its place by acquisition date (nil first, see
// acquiredBefore). It is the only way a lot enters a position, so the queue
// order is an invariant kept here rather than sorted later; releases,
// amortizations (drainLotsCost) and splits all work from the head in place.
//
// Ties keep journal order: the law says nothing finer than the day, and a
// tax figure must not depend on a sort algorithm. Walking back from the tail
// while the previous lot is strictly later gives exactly a stable sort's
// order, and costs nothing for a journal in date order.
func (p *Position) addLot(id LotID, qty decimal.Decimal, costMinor int64, acquiredOn, rateOn *time.Time) {
	at := len(p.Lots)
	for at > 0 && acquiredBefore(acquiredOn, p.Lots[at-1].AcquiredOn) {
		at--
	}
	p.Lots = slices.Insert(p.Lots, at, Lot{ID: id, Quantity: qty, CostMinor: costMinor, AcquiredOn: acquiredOn, RateOn: rateOn})
	p.Quantity = p.Quantity.Add(qty)
	p.CostMinor += costMinor
	p.heldALot = true
}

// Compute folds the journal into positions. See the package doc.
func Compute(ops []Operation) (map[uuid.UUID]*Position, error) {
	positions := make(map[uuid.UUID]*Position)
	// settled holds the instruments whose currency is fixed: the walk's state,
	// not the position's.
	settled := make(map[uuid.UUID]bool)
	// get returns the operation's position, creating it, and applies the currency
	// rule: every operation touching cost, lots, fees or realizations must repeat
	// the first such one's currency (Type.mustMatchPositionCurrency), since two
	// currencies in one int64 would be undetectable corruption. Income does not
	// settle the currency: a journal may open with a rouble coupon on a yuan bond.
	// Until settled, the provisional currency is the lowest code seen; nothing
	// computed depends on it, since anything that would settles it.
	get := func(o Operation) (*Position, error) {
		p, ok := positions[*o.InstrumentID]
		if !ok {
			p = &Position{InstrumentID: *o.InstrumentID, Currency: o.Currency}
			positions[*o.InstrumentID] = p
		}
		if !o.mustMatchPositionCurrency() {
			// Only while unsettled: afterwards the field names CostMinor's currency.
			if !settled[*o.InstrumentID] {
				p.Currency = min(p.Currency, o.Currency)
			}
			return p, nil
		}
		if !settled[*o.InstrumentID] {
			p.Currency = o.Currency
			settled[*o.InstrumentID] = true
			return p, nil
		}
		if o.Currency != p.Currency {
			return nil, badOp(o, fmt.Sprintf("currency %s does not match the %s this position's cost is in, for instrument %s: only a dividend, a coupon or a tax may arrive in another currency",
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
		if err := checkShape(o); err != nil {
			return nil, err
		}

		// Conversions do not touch positions.
		if o.Type == TypeConversion {
			continue
		}

		p, err := get(o)
		if err != nil {
			return nil, err
		}
		apply, ok := appliers[o.Type]
		if !ok {
			return nil, badOp(o, "type not applicable to instrument operations")
		}
		if err := apply(p, o); err != nil {
			return nil, err
		}
	}
	// Realized totals are settled now that every Position.Currency is final.
	for _, p := range positions {
		if err := p.finishRealized(); err != nil {
			return nil, err
		}
	}
	return positions, nil
}

// CheckTransferLots verifies that a transfer's stored breakdown matches its
// row: every piece non-negative, quantities summing to the quantity moved and
// costs to the basis moved. A mismatch is a corrupted journal and the whole
// computation is refused rather than worked around.
//
// Healthy data cannot trip it: costs are summed from these very pieces when
// written, and quantities stay on the journal's scale (applySplit), with the
// breakdown quantized as built and re-read and re-checked by the store before
// commit. Do not pass an operation without a breakdown.
func CheckTransferLots(o Operation) error {
	qty := decimal.Zero
	var cost int64
	for i, pc := range o.TransferLots {
		if pc.Quantity.IsNegative() {
			return badOp(o, fmt.Sprintf("transfer lot %d has quantity %s: a piece cannot move a negative quantity", i, pc.Quantity))
		}
		if pc.CostMinor < 0 {
			return badOp(o, fmt.Sprintf("transfer lot %d has cost %d: a piece's cost basis cannot be negative", i, pc.CostMinor))
		}
		// No units is legitimate only with money: a shareless parcel from a reverse
		// split.
		if pc.Quantity.IsZero() && pc.CostMinor == 0 {
			return badOp(o, fmt.Sprintf("transfer lot %d has neither units nor cost: it describes nothing", i))
		}
		// A piece's acquisition date may be absent (shares that arrived by a transfer
		// without dates), but a given date may not postdate the transfer: the source
		// lots are resolved as of the transfer's day.
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

// settledCopy is the operation's settlement day as a value of its own, or nil
// when unknown.
func settledCopy(o Operation) *time.Time {
	if o.SettledOn == nil {
		return nil
	}
	day := *o.SettledOn
	return &day
}

// RateDay is the day whose official rate prices an operation's money: its
// settlement day when known (decision Р-3), else its own day.
func RateDay(o Operation) time.Time {
	if o.SettledOn != nil {
		return *o.SettledOn
	}
	return o.OccurredOn
}

// amortizedShare is the fraction of the holding's outstanding principal a
// repayment returns: amount / (face value per unit before it × units held), at
// most 1. Unknown without a face value or with nothing held.
func amortizedShare(o Operation, p *Position) (decimal.Decimal, bool) {
	if o.FaceBeforeMinor == nil || *o.FaceBeforeMinor <= 0 || !p.Quantity.IsPositive() {
		return decimal.Zero, false
	}
	principal := decimal.NewFromInt(*o.FaceBeforeMinor).Mul(p.Quantity)
	share := decimal.NewFromInt(o.AmountMinor).Div(principal)
	if share.GreaterThan(decimal.NewFromInt(1)) {
		share = decimal.NewFromInt(1)
	}
	return share, true
}

// takeLotsShare retires share of every lot's cost (the spin-off allocation, so
// pieces sum to the floor of the whole) and returns dated pieces without
// units.
func takeLotsShare(p *Position, share decimal.Decimal) []ReleasedLot {
	var out []ReleasedLot
	for i, piece := range SpinoffPieces(p.Lots, share) {
		if piece.CostMinor <= 0 {
			continue
		}
		p.Lots[i].CostMinor -= piece.CostMinor
		out = append(out, ReleasedLot{From: piece.From, Quantity: decimal.Zero, CostMinor: piece.CostMinor, AcquiredOn: piece.AcquiredOn, RateOn: piece.RateOn})
	}
	return out
}

// drainLotsCost subtracts amount from lot costs front-to-back, leaving
// quantities alone, and reports the pieces taken in queue order — dated, so a
// return of principal can be valued in another currency, and without units. A
// lot giving nothing yields no piece. releaseRecorded ignores the report.
func drainLotsCost(p *Position, amount int64) []ReleasedLot {
	var pieces []ReleasedLot
	for i := range p.Lots {
		if amount == 0 {
			break
		}
		take := min(amount, p.Lots[i].CostMinor)
		if take == 0 {
			continue
		}
		p.Lots[i].CostMinor -= take
		amount -= take
		pieces = append(pieces, ReleasedLot{
			From: p.Lots[i].ID, Quantity: decimal.Zero, CostMinor: take, AcquiredOn: p.Lots[i].AcquiredOn, RateOn: p.Lots[i].RateOn,
		})
	}
	return pieces
}

// ReleasedLots returns the FIFO breakdown that releasing qty units would
// consume after folding ops, without changing anything. Transfers store it so
// moved lots keep their purchase dates.
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

// ReleasedCost is the cost of releasing qty units after folding ops: the sum
// of ReleasedLots.
func ReleasedCost(ops []Operation, instrumentID uuid.UUID, qty decimal.Decimal) (int64, error) {
	pieces, err := ReleasedLots(ops, instrumentID, qty)
	if err != nil {
		return 0, err
	}
	return LotsCost(pieces), nil
}
