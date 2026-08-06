// Package portfolio computes positions from the operations journal.
// The engine is pure: it takes one account's operations ordered by
// (occurred_on, created_at) ascending and returns per-instrument positions.
// It never touches the database — determinism is the point: the journal is
// the single source of truth and positions are always recomputable.
//
// Transfer cost basis is a snapshot: when a transfer pair is created, the
// lots it consumes are resolved once (see ReleasedLots) and stored alongside
// the transfer_in operation, their summed cost on the operation itself. That
// breakdown is then THE RECORD OF WHAT LEFT, and BOTH legs are folded from it:
// the receiving account rebuilds those lots and the departing one gives up
// those very lots (see Position.releaseRecorded), so a pair is consistent by
// construction rather than because two independent derivations happen to agree.
//
// They did once, and then stopped. The departing leg used to release a FRESH
// FIFO slice of its own and throw the cost away, which reproduced the stored
// snapshot only for as long as the queue rule stayed put. It did not: ordering
// the queue by acquisition rather than by arrival made every already-recorded
// transfer resolve to DIFFERENT lots than the ones it had frozen, with no edit
// by anyone — the same parcel could then sit on both accounts at once while
// another vanished, and the family's basis was overstated by the difference,
// silently, because the integrity check only ever compared a leg against its
// own frozen numbers (issue #60). Reading the release off the record instead of
// re-deriving it makes the pair immune to any future change of that rule.
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
// A POSITION'S COST AND ITS INCOME ARE TWO FIGURES AND MAY BE IN TWO
// CURRENCIES. The cost currency is settled by the first operation that touches
// cost, quantity or fees, and every later such operation must repeat it: those
// figures are single int64s of minor units, and mixing two currencies inside
// one is corruption nothing downstream could detect. Income is under no such
// rule — it is kept per currency (see Position.IncomeByCurrency) — because a
// yuan bond pays its coupons in rubles and a dollar share's dividend and
// withheld tax arrive in rubles, which is what Russian brokers do rather than
// what broken data looks like. Minor amounts of different currencies are still
// never mixed into one int64; there is simply more than one int64 now.
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
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/platform/money"
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

// CurrencyMinor is an amount of minor units together with the currency they
// are units of. It exists because one figure of a position — its income — is
// not necessarily denominated in the position's own currency, and minor units
// of two currencies must never meet inside one int64 (see
// Position.IncomeByCurrency).
type CurrencyMinor struct {
	Currency string
	Minor    int64
}

// Position is the running state of one instrument within one account.
// Closed positions (zero quantity) are kept: realized P&L and income
// remain meaningful history.
type Position struct {
	InstrumentID uuid.UUID
	// Currency is the currency of the position's COST AND QUANTITY: of
	// CostMinor, of every lot's cost, of FeesMinor, and of everything a
	// Realization is made of. It is NOT "the one currency of the position" —
	// income can arrive in another one, and on a Russian broker routinely does
	// (see IncomeByCurrency).
	//
	// It is settled by the first operation that touches any of those figures
	// (see Type.mustMatchPositionCurrency), and every later such operation must
	// repeat it. Until one arrives, a position that has seen nothing but income
	// carries a currency PROVISIONALLY, and the first such operation replaces
	// it.
	//
	// A POSITION THAT NEVER SEES ONE — every payment recorded, no purchase, no
	// transfer leg, no commission, which is how a paper bought before the
	// import window or received by transfer looks — KEEPS THAT PROVISIONAL
	// VALUE, AND THEN THIS FIELD IS A CONVENTION FOR DRAWING THE ROW, NOT A
	// FACT ABOUT WHAT WAS PAID. There is no cost, no lot, no fee and no
	// realization for it to be the currency OF — every operation that makes one
	// settles this field, so that is checkable rather than a matter of care —
	// and the paper's own currency is simply not in this journal to be found. A
	// caller must therefore not read it as the currency the paper is priced in,
	// and must not put a figure under it that is not itself in it: the income
	// beside it may be in that currency, in another, or in several at once, and
	// IncomeByCurrency is where that question is answered.
	//
	// The value chosen is the LOWEST CURRENCY CODE among the payments received,
	// which is the very order the income is kept in, so it is always
	// IncomeByCurrency[0].Currency. It is borrowed from the income and is no
	// summary of it. Determinism is the whole requirement: two accounts holding
	// the same payments listed in a different order must draw the same row, and
	// taking the currency from whichever payment the journal happened to list
	// first drew a dollar share under a ruble sign in one order and under a
	// dollar sign in the other.
	Currency         string
	Quantity         decimal.Decimal
	CostMinor        int64 // remaining FIFO cost basis (fees capitalized on buy)
	RealizedPnLMinor int64
	// IncomeByCurrency is what the paper PAID — dividends and coupons received,
	// less the taxes attributed to this instrument — KEPT PER CURRENCY, ordered
	// by currency code.
	//
	// Per currency because income and cost need not be denominated alike. A
	// yuan bond is bought for yuan and pays its coupons in rubles, converted by
	// the broker on the day of the payment; a dollar share's dividend and the
	// tax withheld on it arrive in rubles too. That is ordinary Russian
	// brokerage practice rather than damaged data, and adding the two into one
	// int64 of minor units — the shape this field replaced — is exactly the
	// silent corruption the currency rule exists to prevent. So the cost stays
	// in the currency the paper was paid for and the income stays in the
	// currency it arrived in, and neither is converted here: the engine knows no
	// exchange rates and must not (see the package doc).
	//
	// ONE ENTRY PER CURRENCY, ORDERED BY CURRENCY CODE — a property of the money
	// rather than of the journal. Two accounts holding the same payments listed
	// in a different order must render and compare identically, so the order
	// cannot be the arrival order; and it is a slice rather than a map because
	// Go's map iteration is deliberately random and these figures go onto a
	// screen. addIncome is the only thing that writes it, and it maintains both
	// properties.
	//
	// An entry can be zero or negative, and neither is a defect: a coupon and
	// the tax withheld from it cancelling exactly is not the same statement as
	// no income at all, and a tax on a payment made before this account's
	// journal begins leaves a negative one.
	IncomeByCurrency []CurrencyMinor
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
	// Realizations are the disposals that PRODUCED RealizedPnLMinor, each
	// recorded as what it was made of (see Realization), in journal order.
	//
	// Their results sum to RealizedPnLMinor exactly, and by construction rather
	// than by two derivations that happen to agree: realize is the only thing
	// that moves the total and it moves it by the event's own figure, so a
	// branch wanting to realize something without saying what it was made of
	// would have to go round that one method. This package has been bitten
	// before by a number and its breakdown maintained separately (see the
	// package doc on issue #60), and the lesson is the same one — the moment
	// they can disagree, they eventually do, silently.
	//
	// A TRANSFER OUT PRODUCES NONE. Moving shares between the family's own
	// accounts is a disposal in none of the jurisdictions this was researched
	// against, and the departing leg has no proceeds to record: its AmountMinor
	// is the basis that travelled, not money received. Nor is anything left
	// unaccounted for by leaving it out — that leg has never added to
	// RealizedPnLMinor either (see the transfer_out branch in Compute), so
	// there is no term to remove and the sum above is untouched by the
	// decision.
	Realizations []Realization
}

// realize records one disposal and moves the running total by that very event's
// result. Doing both here, and only here, is what makes RealizedPnLMinor a
// CONSEQUENCE of the events instead of a second number kept alongside them —
// see Position.Realizations for why this package will not keep two.
func (p *Position) realize(r Realization) {
	p.Realizations = append(p.Realizations, r)
	p.RealizedPnLMinor += r.PnLMinor()
}

// findIncome locates a currency in IncomeByCurrency by a binary search over the
// very order that slice is kept in, and reports where it is — or, when it is
// absent, where it would have to go to keep that order.
//
// BOTH LOOKUPS BY CURRENCY GO THROUGH IT — addIncome's and IncomeMinorIn's,
// which are all there are — and that is the point of it being a function:
// "where this currency belongs" and "where this currency is found" are then one
// answer rather than two that must agree, and the ordering is written down
// once. A position holds a handful of currencies at most, so this is for the
// invariant and not for speed.
func (p *Position) findIncome(currency string) (int, bool) {
	return slices.BinarySearchFunc(p.IncomeByCurrency, currency, func(e CurrencyMinor, c string) int {
		return strings.Compare(e.Currency, c)
	})
}

// addIncome books minor units of income in the currency they arrived in. It is
// the only thing that writes IncomeByCurrency, and it keeps that slice ordered
// by currency code with one entry per currency — see the field for why the
// order is not the journal's.
//
// THE ADDITION IS GUARDED, and Go's int64 + is what it is guarded against: it
// wraps silently past the range, and a wrapped total is a plausible-looking sum
// of money of the wrong magnitude and often the wrong sign. Every payment
// arriving here is an ordinary figure — it had to survive money.Minor to be an
// int64 at all — and a total of ordinary figures need not be, so the guard
// belongs on the total (the same argument money.Add itself is written on).
// Whether a journal can actually reach it is not the test: the alternative is a
// silently wrong figure, and this one is a named refusal.
//
// Nothing is half-booked by a refusal. The only path that can fail is the one
// adding to an entry that already exists, and it fails before the assignment —
// a currency seen for the first time is simply inserted, its amount being an
// int64 already — so a refused payment leaves the slice exactly as it found it.
func (p *Position) addIncome(currency string, minor int64) error {
	at, found := p.findIncome(currency)
	if !found {
		p.IncomeByCurrency = slices.Insert(p.IncomeByCurrency, at, CurrencyMinor{Currency: currency, Minor: minor})
		return nil
	}
	sum, err := money.Add(p.IncomeByCurrency[at].Minor, minor)
	if err != nil {
		// money.Add names no figure by design, so the context that says WHICH
		// total failed is added here.
		return fmt.Errorf("%w: income in %s, adding %d to %d", err, currency, minor, p.IncomeByCurrency[at].Minor)
	}
	p.IncomeByCurrency[at].Minor = sum
	return nil
}

// IncomeMinorIn is the income booked in one currency, and zero when the
// position received none in it.
//
// Zero is therefore two answers at once — no payment ever arrived in this
// currency, or the payments that did cancel out — and a caller that must tell
// them apart reads IncomeByCurrency itself. Callers that publish this figure
// have to say which currency it is in; it is not "the position's income" unless
// the position received income in nothing else.
func (p *Position) IncomeMinorIn(currency string) int64 {
	at, found := p.findIncome(currency)
	if !found {
		return 0
	}
	return p.IncomeByCurrency[at].Minor
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

// Realization is one disposal recorded as WHAT IT WAS MADE OF rather than as
// the single number it contributes to Position.RealizedPnLMinor.
//
// In the position's own currency the two carry the same information and the
// number is enough. In rubles they do not. A settled result is converted at the
// rates of the days it actually happened on — the proceeds and the fee at the
// day of the disposal, each released parcel of basis at the day THAT parcel was
// bought (НК РФ ст. 210 п. 5) — so an accumulated figure in dollars cannot be
// converted at all: it has no one date, and the gap between those rates is part
// of the result rather than a rounding of it. What has to survive the fold is
// therefore the breakdown, one record per disposal: when it happened, what came
// in, what it cost in fees, and which parcels of basis went out under which
// purchase dates.
//
// This is also why a realization is FINAL in a way an unrealized gain never is.
// Both of its ends are past events with dates of their own, so the ruble figure
// struck from it will never move again; an open position's two ends both float
// with today's quote and today's rate, and "in rubles" there can only ever mean
// "valued today".
//
// Released may be EMPTY — an amortization arriving after the basis is spent
// returns principal that is pure gain — and a piece in it may not know when it
// was acquired, because a parcel that reached this account through a transfer
// with no recoverable dates keeps that absence through every later disposal
// (see Lot.AcquiredOn). Both are legitimate and are recorded as they are. No
// ruble expense can be struck for an undated piece, and deciding what to
// publish then belongs to the caller: substituting the disposal's own date here
// would produce a figure nothing downstream could tell from a real one, which
// is the invention this package exists to refuse.
type Realization struct {
	// OccurredOn is the day of the disposal. It dates the proceeds and the fee,
	// and it dates NONE of the released basis — those days are the pieces' own.
	OccurredOn time.Time
	// ProceedsMinor is what came in: a sale's amount, an amortization's returned
	// principal. Positive, in the position's currency.
	ProceedsMinor int64
	// FeeMinor is what this disposal cost to make, valued at the same day as
	// the proceeds. Zero for an amortization: the engine attributes no fee to
	// one anywhere (see the amortization branch in Compute).
	FeeMinor int64
	// Released is the basis given up, one piece per source lot, in the order
	// the queue gave them up (see ReleasedLot). An amortization's pieces carry
	// no quantity — it returns principal without moving a single share.
	Released []ReleasedLot
}

// PnLMinor is this one event's settled result in the position's currency.
//
// It is the ONLY definition of that number anywhere: Position.realize moves the
// running total by exactly this, so the total cannot come to say something the
// events do not (see Position.Realizations).
func (r Realization) PnLMinor() int64 {
	return r.ProceedsMinor - r.FeeMinor - LotsCost(r.Released)
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
			// A lot that is entirely consumed here produces a piece EVEN WHEN
			// its CostMinor is 0 — unlike drainLotsCost, which skips a lot
			// that gives up nothing (see drainLotsCost). The two look like
			// they should agree, and they deliberately do not.
			//
			// drainLotsCost's pieces never carry a quantity — an amortization
			// moves no shares, see its own doc — so a zero-cost piece there is
			// empty on both axes and skipping it discards nothing. A piece
			// here always carries the REAL quantity taken from the lot,
			// because these pieces are not only a sale's own record
			// (Realization.Released) but, via ReleasedLots, the very rows a
			// transfer stores as its FIFO breakdown (Operation.TransferLots) —
			// and CheckTransferLots later reconstructs the operation's
			// quantity by summing exactly those rows. Dropping a whole
			// zero-cost lot's piece would leave its quantity unaccounted for,
			// and a transfer moving nothing but such a lot would then read as
			// journal corruption ("transfer lots sum to quantity 0, but the
			// operation moves N") for a perfectly legitimate, zero-basis
			// parcel — confirmed by temporarily mirroring the guard here and
			// watching CheckTransferLots reject exactly that transfer.
			//
			// The cost of keeping it: a sale (or transfer) that empties an
			// undated zero-cost lot puts an undated, zero-cost piece into
			// Realization.Released, and realizedTerms (see http.go) will then
			// decline to strike a ruble figure for the whole disposal — even
			// though a zero-cost term needs no fx rate to be valued at all.
			// That reads as a silence the number did not have to pay, but the
			// piece is not a lie the way an empty drainLotsCost piece would
			// have been: real shares, from a real lot, really left, and the
			// lot really has no date. Suppressing it here to avoid that
			// silence would trade an honest gap for a corrupted transfer.
			pieces = append(pieces, ReleasedLot{Quantity: l.Quantity, CostMinor: l.CostMinor, AcquiredOn: l.AcquiredOn})
			released += l.CostMinor
			remaining = remaining.Sub(l.Quantity)
			p.Lots = p.Lots[1:]
			continue
		}
		// partial piece: floor share, remainder stays in the lot together
		// with its acquisition date — what is left was bought on the same
		// day as the part just released.
		share := lotShare(*l, remaining)
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

// lotShare is the cost basis that goes with taking qty units out of a lot: the
// lot's WHOLE cost when the whole lot goes, the floor of its proportional share
// otherwise. Flooring means a release never takes more money than the shares it
// takes are worth, and what stays in the lot keeps the difference — a ledger may
// leave a minor unit behind, but it must never hand out one that was not there.
//
// BOTH ways of releasing go through it: the queue-driven one (releaseFIFO) and
// the record-driven one (Position.releaseRecorded). That is not tidiness. The
// breakdown a transfer froze was computed by the first, and the second has to
// reproduce it lot for lot when the journal has not moved — so the two must
// answer the same for the same lot and the same quantity, and the way to be sure
// of that is to have one answer rather than two that agree today.
//
// "The whole lot goes" is written as "qty is not less than the lot's quantity"
// rather than "equal" so that a SHARELESS lot gives up all its money: taking
// nothing out of a lot that holds nothing is taking the whole of it, and a lot
// whose shares a reverse split rounded away still holds real basis (see
// applySplit). A proportional share would be zero there, and the money would be
// stranded in a lot no release can ever reach.
func lotShare(l Lot, qty decimal.Decimal) int64 {
	if !qty.LessThan(l.Quantity) {
		return l.CostMinor
	}
	return decimal.NewFromInt(l.CostMinor).Mul(qty).Div(l.Quantity).Floor().IntPart()
}

// recordAndReplayDisagree is the tail both of releaseRecorded's refusals end
// with, and it names TWO possible causes because naming one would name the wrong
// one nearly every time.
//
// Rows this build writes cannot reach either refusal through the API at all:
// every write path — recording an operation, recording a transfer, deleting
// either — replays the account's journal first and turns the request down before
// anything is stored (see operation.Service). So the reachable case is a journal
// written by an EARLIER build, one whose release queue picked lots by another
// rule, and in that case nobody edited anything. Stating "its history was edited
// after the transfer was recorded" as a fact would accuse the owner of something
// they did not do and send them looking for an edit that does not exist, while
// the screen they came for stays blank. Both possibilities are named, and so is
// the way out, which is the same one either way — the same recovery a quantity
// that no longer fits after a split already has (see the package doc).
const recordAndReplayDisagree = "either this account's history was edited after the transfer was recorded, " +
	"or the transfer was recorded by a build whose release queue picked lots by a different rule; " +
	"delete the transfer and record it again either way"

// releaseRecorded gives up the lots a transfer's stored breakdown says left
// this account, instead of deriving a fresh release from the queue as it stands
// now. The breakdown IS the record of what went (see Operation.TransferLots);
// re-deriving it means the two legs of one pair are two independent guesses
// that agree only while nothing about the guessing changes, and the moment the
// queue rule changed they stopped agreeing for every transfer already written
// (see the package doc). Reading it off the record instead is what makes a pair
// consistent by construction.
//
// PIECES ARE MATCHED TO LOTS BY THE DAY OF ACQUISITION, and by nothing else.
// That day is the only durable identity a lot has: quantity and cost are
// whatever is left of it after the releases and amortizations that came before,
// so they name no lot on their own, and a lot's POSITION in the queue is
// exactly the thing that just proved unstable — matching on it would rebuild
// the bug being fixed here in a new place. The day, by contrast, is the fact
// the breakdown was created to carry, it is what every later figure is struck
// at (the ruble basis above all), and it is what decides the queue, so two lots
// that share it are interchangeable for every purpose this package has.
//
// Each piece is taken from the matching lots front-to-back, and only until its
// QUANTITY is satisfied, so a piece never reaches into a lot a later piece
// needs. Its cost comes out of those same lots, and each of them gives up
// exactly the money that goes with the shares it gives up — all of its cost only
// when all of its shares go, the floor of its proportional share otherwise,
// which is the very allocation the release that built this breakdown used (see
// lotShare). Whatever the piece still carries after that is drained from the
// front of the queue once every piece has been served. The leftover is not an
// oddity to be tolerated but a case with a name: a lot whose entire holding was
// rounded away by a reverse split has no quantity and real money still in it,
// the release that built this breakdown consumed it as a piece of nothing, and
// operation.quantizeLots — unable to store a piece with no quantity — folded
// its cost into the next piece along. So a piece can legitimately carry more
// basis than the lot its date points at, and the money is sitting in a
// shareless lot ahead of it. Refusing that would refuse a transfer this program
// itself wrote, which is the one thing a loud check must never do.
//
// PROPORTIONING THE COST IS WHAT MAKES THAT LEFTOVER ARRIVE, and the obvious
// alternative fails silently. Clamping a piece by the whole cost of the lot it
// lands in — "take whatever the lot still holds" — is correct only while the
// piece consumes that lot entirely; the moment it takes the lot in part, the
// lot's whole cost is more than the piece asks for, the clamp never binds,
// nothing is left over, and the shareless lot's money comes out of an innocent
// parcel of the same day instead. The shareless lot then stays on the account
// holding money the destination already holds. Every total still balances — the
// account gives up the basis the record names, the family holds what it paid —
// so nothing anywhere notices; only the parcels are wrong, which is precisely
// the failure this whole mechanism exists to make impossible.
//
// WHAT CANNOT BE MATCHED IS REFUSED, LOUDLY. A piece whose acquisition day has
// no shares left behind it means the journal, replayed under today's rules, does
// not contain the parcel the record says departed: either the source's history
// was edited after the transfer, or the transfer was written down by a build
// whose queue rule picked other lots than today's (see
// recordAndReplayDisagree). There is no quiet answer to that: taking the
// quantity from some other day's lot would re-date shares that are still held
// and reprice them at a rate from a day they were never bought on, and taking
// nothing would leave the family holding a basis twice. The account's positions
// then fail to compute until the transfer is deleted and re-entered, which is
// the same recovery a quantity that no longer fits already has (see the package
// doc on truncation).
//
// ONE SKEW SURVIVES ALL OF THIS, and it is worth naming rather than leaving to
// be discovered. The record is honoured even when the account's shares have been
// multiplied underneath it: a split entered AFTER a transfer but dated BEFORE it
// doubles the lot the breakdown points at, so the recorded basis comes off twice
// the shares it was struck against, and the source is left holding shares with
// none of it while the destination holds all of it. The family still holds
// exactly what it paid, and the money sits where the record says it went, so
// this is strictly better than the re-derivation it replaced — but it is a
// lopsided pair, and it arrives quietly, because a backdated split still
// replays. Re-deriving the release would even it out, at the price of throwing
// the record away, which is the bug this exists to prevent. Deleting the
// transfer, recording the split, and recording the transfer again is the answer,
// as it is for every other way a history can move underneath a record.
//
// Quantities and costs are conserved exactly: the position loses the pieces'
// summed quantity and their summed cost, which CheckTransferLots has already
// established are the operation's own. So whatever the two accounts hold
// afterwards adds up to what was actually spent, which is the property issue
// #60 found broken.
func (p *Position) releaseRecorded(o Operation) error {
	if o.Quantity.GreaterThan(p.Quantity) {
		return fmt.Errorf("%s %s %s: %w: have %s, need %s",
			o.Type, o.InstrumentID, o.OccurredOn.Format("2006-01-02"),
			ErrOversell, p.Quantity, *o.Quantity)
	}
	var carried int64 // recorded cost its own lots could not cover
	for i, pc := range o.TransferLots {
		qty, cost := pc.Quantity, pc.CostMinor
		for j := range p.Lots {
			l := &p.Lots[j]
			if !sameAcquisition(l.AcquiredOn, pc.AcquiredOn) {
				continue
			}
			takeQty := decimal.Min(l.Quantity, qty)
			takeCost := min(lotShare(*l, takeQty), cost)
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
	// A lot with neither shares nor money in it is spent — the same thing
	// releaseFIFO expresses by dropping a lot it consumed whole. One with a
	// quantity of zero and a cost still in it is NOT spent and stays (see
	// applySplit).
	p.Lots = slices.DeleteFunc(p.Lots, func(l Lot) bool { return l.Quantity.IsZero() && l.CostMinor == 0 })
	return nil
}

// acquisitionText renders a lot's acquisition day for an error message,
// including the case where there is none to render.
func acquisitionText(t *time.Time) string {
	if t == nil {
		return "on an unknown day"
	}
	return "on " + t.Format("2006-01-02")
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
// An UNKNOWN acquisition comes before every known one. The reason is this
// package's own and needs no outside support: the head of the queue is the ONLY
// placement that does not require inventing a date. Anywhere else is defined by
// comparing the unknown against real days, which means quietly choosing a day
// for it — the very thing Lot.AcquiredOn exists to refuse. It also has a
// practical edge: sales drain the undated lots out of an account first, after
// which the position can be valued in another currency again
// (Handler.positionInBase publishes nothing while a single lot is undated).
//
// US practice points the same way BY ANALOGY, and only by analogy: 26 CFR
// 1.6045A-1(b)(10) has a transferring broker report securities of unknown
// acquisition date as the earliest acquired ones. That is a rule about what a
// broker must REPORT when part of a position moves, not about which shares
// count as sold — the default lot-relief rule (1.1012-1(c)(1)(i)) has no
// "unknown date" tier at all. So it corroborates the ordering and does not
// license it; the argument above is the one that carries this function.
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

// sameAcquisition reports whether two lots were acquired on the same day, an
// unknown day counting as the same as another unknown one — which is what
// matching a transfer's recorded pieces against the queue needs (see
// Position.releaseRecorded).
//
// It is DERIVED from acquiredBefore rather than written out again, so "equal"
// and "neither one before the other" cannot drift apart: whatever
// acquiredBefore treats as one position in the queue, this treats as one
// acquisition. Spelling out a nil check and a t.Equal here would be the same
// answer today and a second, forgettable place to keep in step tomorrow.
func sameAcquisition(a, b *time.Time) bool {
	return !acquiredBefore(a, b) && !acquiredBefore(b, a)
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
	// settled names the instruments whose currency has been fixed, i.e. that
	// have already seen an operation touching cost, quantity or fees. It is the
	// fold's own state rather than a field on Position: it says how far this
	// walk has got, not what the position is.
	settled := make(map[uuid.UUID]bool)
	// get returns the position for the operation's instrument, creating it on
	// first sight, and enforces the currency rule.
	//
	// THE RULE IS ABOUT COST, NOT ABOUT THE PAPER. Minor amounts of two
	// currencies summed into one int64 would be silent corruption, and the
	// service layer only validates the ISO-4217 shape, not consistency — so
	// every operation whose amount lands in CostMinor, in a lot, in FeesMinor or
	// in a realization must repeat the currency of the first such operation
	// (Type.mustMatchPositionCurrency lists them, and says why a fee is among
	// them). Income does not have to: a dividend, a coupon or the tax withheld
	// from either may arrive in any currency and is booked in the currency it
	// arrived in (see Position.IncomeByCurrency).
	//
	// INCOME DOES NOT SETTLE THE CURRENCY EITHER, which is the same rule seen
	// from the other end and not a separate kindness. A ruble coupon on a yuan
	// bond is no statement that the position is in rubles; taking it for one
	// would refuse the yuan purchase that follows — the very refusal this
	// removes, merely moved to another journal order, and journals do open with
	// a payment (the paper was bought before the import window, or arrived by
	// transfer). So a position that has seen only income carries a currency
	// provisionally — which one is the next paragraph's business — and the first
	// cost-touching operation replaces it.
	//
	// Nothing computed can be invalidated by that replacement, and the reason is
	// checkable rather than a matter of care: until it happens the position has
	// no cost, no lot, no fee and no realization, because every operation that
	// could make one settles the currency itself.
	//
	// AND WHILE IT IS PROVISIONAL IT IS STILL THE LOWEST CURRENCY CODE SEEN, not
	// the first one listed. A journal that never settles it is an ordinary
	// journal — a paper bought before the import window pays its dividends, has
	// tax withheld from them, and is never purchased here at all — so that
	// provisional value is what its row ends up being drawn under (see
	// Position.Currency). Taking it from the earliest payment made the same two
	// payments draw two different rows depending on which of them the journal
	// listed first; the lowest code is the order the income itself is kept in,
	// so there is one order here and not a second one to keep in step with it.
	get := func(o Operation) (*Position, error) {
		p, ok := positions[*o.InstrumentID]
		if !ok {
			p = &Position{InstrumentID: *o.InstrumentID, Currency: o.Currency}
			positions[*o.InstrumentID] = p
		}
		if !o.Type.mustMatchPositionCurrency() {
			// While unsettled, and only there. Past that point this field is
			// the currency of CostMinor, of the lots and of FeesMinor, and a
			// payment arriving in a lower-coded one would rename those figures
			// without touching them.
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
			p.realize(Realization{
				OccurredOn:    o.OccurredOn,
				ProceedsMinor: o.AmountMinor,
				FeeMinor:      o.FeeMinor,
				Released:      pieces,
			})
			p.FeesMinor += o.FeeMinor
		case TypeDividend, TypeCoupon, TypeTax:
			// In the currency the payment ARRIVED in, which need not be the one
			// the paper is priced in — see Position.IncomeByCurrency. A tax
			// arrives here too, through its negative amount, and in its own
			// currency like any other payment.
			//
			// The refusal this can return says a TOTAL left the int64 range,
			// not that the entry is bad, so it is wrapped the way a release's
			// is (see the sell branch): the type, instrument and date that
			// name the entry which reached the edge, over the error that says
			// what the edge was.
			if err := p.addIncome(o.Currency, o.AmountMinor); err != nil {
				return nil, fmt.Errorf("%s %s %s: %w", o.Type, o.InstrumentID, o.OccurredOn.Format("2006-01-02"), err)
			}
		case TypeFee:
			p.FeesMinor += -o.AmountMinor // amount negative → positive fee
		case TypeAmortization:
			// Return of principal: it retires cost basis, and in THIS currency
			// only the excess over what is left of that basis is a result.
			//
			// It is nonetheless a disposal and is recorded as one, even when
			// that result is zero, because in rubles the covered part is not
			// neutral: the principal comes back at the rate of the day it was
			// paid while the basis it retires was struck at the rates of the
			// days those lots were bought, and that difference is as much of a
			// result as any sale's (see Realization). Its released pieces are
			// the lot costs the drain took, which carry dates and cost but no
			// quantity — nothing was sold.
			//
			// No fee is attributed, here or to FeesMinor: the engine has never
			// modelled a fee on an amortization, and inventing one on the event
			// alone would put a number in it that the running total does not
			// contain.
			if o.AmountMinor <= 0 {
				return nil, badOp(o, "amortization amount must be positive")
			}
			reduce := min(o.AmountMinor, p.CostMinor)
			p.CostMinor -= reduce
			p.realize(Realization{
				OccurredOn:    o.OccurredOn,
				ProceedsMinor: o.AmountMinor,
				Released:      drainLotsCost(p, reduce),
			})
		case TypeTransferOut:
			if len(o.TransferLots) == 0 {
				// Nothing was recorded about which lots left, so there is
				// nothing to give up but a fresh slice of the queue, and the
				// released cost is discarded: the pair's transfer_in carries a
				// basis that was named by hand and has no source lots behind it
				// (or predates the breakdown entirely). This is the one case
				// where the two legs are NOT reconciled — the owner said what
				// the parcel was worth and the journal cannot contradict them —
				// and it is legitimate rather than a gap to be closed.
				if _, err := p.releaseFIFO(*o.Quantity); err != nil {
					return nil, fmt.Errorf("%s %s %s: %w", o.Type, o.InstrumentID, o.OccurredOn.Format("2006-01-02"), err)
				}
				break
			}
			// A breakdown exists, so the account gives up exactly what it says
			// went (see releaseRecorded), not what today's queue rule would
			// pick. The same guard the arriving leg applies runs first: the two
			// legs read one set of rows (see Operation.TransferLots), and a set
			// that no longer sums to the operation carrying it is damage on
			// both.
			if err := CheckTransferLots(o); err != nil {
				return nil, err
			}
			if err := p.releaseRecorded(o); err != nil {
				return nil, err
			}
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
// keeps quantities intact; only the cost basis shrinks) and REPORTS which lots
// gave up which part of it, in queue order.
//
// The report is what lets a return of principal be valued in another currency
// at all: the money retired belongs to the day each lot was bought, exactly as
// a sale's released basis does, and a caller that only learned the total would
// have no date to convert it at (see Realization). The pieces carry NO
// quantity, which is not an omission — an amortization moves no shares, and
// writing the lot's remaining quantity there would claim it did.
//
// A lot with nothing left to give produces no piece at all: an empty piece
// would record a lot as having taken part in an event it took no part in, and
// every reader would then have to know to discount it.
//
// The caller that ignores the report is releaseRecorded, which drains basis a
// transfer's breakdown carried beyond its own lots. That is not a realization
// of anything — see Position.Realizations — so it has nothing to do with the
// pieces.
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
			Quantity: decimal.Zero, CostMinor: take, AcquiredOn: p.Lots[i].AcquiredOn,
		})
	}
	return pieces
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
