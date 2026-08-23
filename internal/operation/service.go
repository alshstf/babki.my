package operation

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/currency"
	"babki.my/babki/internal/platform/dates"
	"babki.my/babki/internal/platform/money"
	"babki.my/babki/internal/portfolio"
)

// ErrInconsistent means applying an operation (or removing one) would make
// the account's journal fail to replay through the portfolio engine — e.g.
// an oversell or a broken transfer chain.
var ErrInconsistent = errors.New("journal would become inconsistent")

// pgForeignKeyViolation and pgUniqueViolation are the SQLSTATE codes Postgres
// returns for the constraints Service.Create can trip.
const (
	pgForeignKeyViolation = "23503"
	pgUniqueViolation     = "23505"
)

// THE MONEY CAP THIS FILE BOUNDS EVERYTHING ELSE AGAINST is
// money.MaxAmountMinor: 10^15 minor units, ≈10 trillion roubles — far above any
// real portfolio, yet far enough from math.MaxInt64 that summing a whole journal
// of such values cannot overflow. Without it, a single amount_minor =
// math.MinInt64 poisons the FIFO cost basis and wraps realized P&L.
//
// It sat here as this package's own constant until the balance a user records
// for an account needed the very same cap (#89). The two figures meet — the
// accounts screen sums balances, the journal sums amounts, both are money in an
// account's currency, and the same screen shows them — so there is one constant
// in one place instead of two that eventually differ. Its doc comment carries
// the whole argument.

// minorUnitScale is how many decimal places a major currency unit is split into
// for storage: amount_minor and fee_minor are counts of those, and a price is
// on the wire in MAJOR units, so it is shifted by this before the two are
// compared. Same two-decimal convention as marketdata.Converter and
// portfolio.centsPerUnit.
const minorUnitScale = 2

// maxQuantity, maxPrice and maxSplitRatio bound the three decimal fields an
// operation carries. Not one of them is money, and all three decide how much
// money the row stands for, so each comes from what has to fit — an int64 of
// minor units, or the column that holds the value — rather than from a guess at
// how many shares a person might sensibly own.
//
// THE QUANTITY IS THE ONE THE SCREEN BREAKS ON, so it is the one to read first.
// Everything that later reads the journal multiplies a quantity by a QUOTE's
// price and then by an fx rate, and the result has to be an int64 of minor
// units. A quantity this program accepted and no ordinary quote can value is a
// positions screen answering 500 for as long as the row exists (#84) — the whole
// reason there is a write-time bound at all.
//
//   - maxQuantity is the money cap read as a COUNT of units: at one MAJOR unit
//     apiece — a whole rouble or dollar for a single unit — 10^13 units are
//     worth exactly money.MaxAmountMinor. What the bound LEAVES is the number that
//     matters: math.MaxInt64 / 100 / 10^13 ≈ 9223 units of money per unit of
//     instrument can still be published at the largest quantity accepted here,
//     which is above an ordinary share, bond or ETF unit. Read as a count of
//     MINOR units instead — one kopeck apiece, 10^15 units — the same cap leaves
//     92.24, BELOW the price of most securities: the bound would then admit a
//     quantity that breaks the screen at an entirely ordinary quote, which is
//     the failure it exists to prevent rather than a smaller version of it.
//     What the read side actually refuses is a VALUATION above ~9.2×10^16 major
//     units of money, however the price and the quantity divide it, and no
//     bound on one factor can promise anything about that product (a share of
//     the priciest kind there is, ~7×10^5 dollars, still reaches it at 10^13
//     units — a holding worth 7×10^18 dollars). The bound's job is narrower and
//     achievable: that the largest quantity accepted here is not by itself the
//     reason an ordinary quote cannot be valued.
//   - maxPrice is the money cap read as a PRICE per unit: one unit may not cost
//     more than a whole operation is allowed to be for. It comes out at the same
//     10^13, both being the cap expressed in major units. They are two constants
//     and not one because they are two different quantities — a count of units
//     and a sum of money per unit — that happen to coincide, and nothing may
//     rest on the coincidence. Their refusals do print the same digits today,
//     which is exactly why the tests compare refusals WHOLE rather than looking
//     for a bound inside a message (see service_bounds_test.go).
//   - maxSplitRatio is a ratio read as a quantity — a ratio of R turns one unit
//     into R units — which would put it at maxQuantity as well, except that
//     split_ratio is NUMERIC(20,10) and keeps ten integer digits: 10^10 is the
//     first value the column cannot hold. The narrower ceiling wins. A ratio at
//     or above it is refused as a named field instead of arriving as a database
//     error about numeric overflow, which no importer can act on.
//
// THE PRODUCT of the two factors is bounded as well, and NOT as an overflow
// guard: an operation's own price is consumed in no money arithmetic anywhere in
// this program. Nothing computes with its VALUE except the checks below — the
// store carries it to and from its column, the handler echoes it back as a
// string (see toAPI in http.go), and the engine values a buy from amount_minor
// without ever reading a price at all. The product bound is a
// DATA-CONSISTENCY one — price × quantity is what the trade was for, which is
// the same money amount_minor carries on the very same row, so it is capped
// where that already is: two fields describing one sum of money must not
// disagree about how much money can exist.
//
// THE FACTORS ARE BOUNDED SEPARATELY TOO, because the product cannot always be
// checked: price is optional and many broker exports carry none, and the read
// path multiplies the quantity by a QUOTE's price rather than by this
// operation's, so a quantity commonly arrives at the screen with no companion at
// all. That is the shape #84 actually arrives in.
//
// WHAT THESE REFUSE THAT IS REAL: more than 10^13 units of an instrument worth
// less than a whole rouble apiece — the quantities meme tokens are held in, and
// what a hyperinflated currency's cash row would look like. The trade is
// deliberate and one-sided: this program prices neither of those today
// (portfolio.marketValue values shares, ETFs and bonds and nothing else), the
// holdings it is built for are shares, bonds and ETFs at Russian and global
// brokers, and a screen that cannot render is worse than a refusal a holder of
// sub-rouble units could hit. Should such a holding ever appear, maxQuantity is
// one line to revisit and the refusal already names the field and the number.
//
// NONE OF THIS LETS THE READ-SIDE GUARDS GO (portfolio.marketValue,
// rateLookup.applyTo, sumInBase, and money.Minor behind them all). What fitted
// when it was written can stop fitting later: a quote's price and an fx rate are
// both unbounded from above and both arrive after the fact, a position is the
// sum of many operations and can pass the bound one accepted buy at a time, a
// split multiplies a whole position by a ratio no per-operation bound can size,
// and the rows written before this bound existed are still in the journal — no
// migration comes with it (see TestRowsWrittenBeforeTheBoundAreStillWorkable).
// A write-time bound cannot promise the product fits; it only stops one factor
// from being the reason it does not.
//
// maxQuantity and maxPrice are DERIVED from money.MaxAmountMinor rather than written
// out beside it, so that moving the money cap moves them with it. maxSplitRatio
// is not: it is the column's ceiling, and the column is what would have to move
// first.
var (
	maxQuantity = decimal.NewFromInt(money.MaxAmountMinor).Shift(-minorUnitScale)
	maxPrice    = decimal.NewFromInt(money.MaxAmountMinor).Shift(-minorUnitScale)

	// maxSplitRatio is the first ratio split_ratio cannot store, so the check
	// against it refuses the value ON it rather than past it — unlike every
	// other bound here, which admits its own edge.
	maxSplitRatio = decimal.New(1, 10)

	// maxAmountMinorDec is money.MaxAmountMinor itself as a decimal, so the product
	// above is compared against it exactly rather than through an int64
	// conversion that would have to survive the very overflow being checked for.
	maxAmountMinorDec = decimal.NewFromInt(money.MaxAmountMinor)
)

// checkQuantityBound refuses a quantity past maxQuantity. Both doors into the
// quantity column go through it — an operation's own field (validate) and the
// quantity a transfer moves (CreateTransfer) — so the two cannot come to refuse
// at different sizes or in different words.
func checkQuantityBound(q decimal.Decimal) error {
	if q.Abs().GreaterThan(maxQuantity) {
		return fmt.Errorf("%w: quantity must be within ±%s", family.ErrValidation, maxQuantity)
	}
	return nil
}

// Service validates journal entries and guards journal consistency by
// replaying the account's operations through the portfolio engine.
type Service struct{ store *Store }

func NewService(store *Store) *Service { return &Service{store: store} }

// TransferParams describes an in-kind transfer of an instrument position
// between two accounts. The moved cost basis is either supplied explicitly
// (CostMinorOverride) or computed from the source account's FIFO history.
type TransferParams struct {
	FromAccountID     uuid.UUID
	ToAccountID       uuid.UUID
	InstrumentID      uuid.UUID
	Quantity          decimal.Decimal
	OccurredOn        time.Time
	CostMinorOverride *int64
	Note              string
}

// quantityScale is how many decimal places the journal keeps for a quantity:
// operations.quantity, operations.split_ratio and
// operation_transfer_lots.quantity all keep ten (see the migrations).
//
// It is defined by the engine, not here, because it is what a POSITION is
// bound to as well — see portfolio.QuantityScale for why that is the engine's
// business. This package's use of it is the other half of the same contract:
// a number on its way into one of those columns must be brought onto the scale
// deliberately, by code that can decide where the rounding goes, so that the
// value the consistency check replays is the value the row will hold. Letting
// Postgres round it silently is what turned a legitimate transfer into an
// unreadable account (see quantizeLots) and what let a sell be accepted for one
// quantity and recorded as another (see normalizeForStorage).
const quantityScale = portfolio.QuantityScale

// minOccurredOn is the oldest date an operation may carry.
//
// IT IS A TYPO GUARD AND NOTHING MORE, and the comment says so rather than
// dressing the number up as a rule from somewhere: no law, no broker and no
// data source here names 1900. It is in particular NOT a claim about the
// earliest date this program can value — an operation dated 1950 is accepted
// here, and whether its figures can be shown in the base currency is a
// separate question about which rates exist, one this bound does not answer
// and must not be read as answering.
//
// What it does buy is the one wrong answer a mistyped year gives silently. A
// year is four characters and the leading one is the easy one to fumble: 1026
// for 2026 is one keystroke. The journal's queue is ordered by acquisition date
// (see the package documentation and internal/family/taxresidency.go), so such
// a row does not land somewhere visibly odd — it lands at the FRONT, ahead of
// everything genuine, and the next sale releases it first. The cost basis that
// comes out is wrong, and nothing on any screen says a date was strange,
// because a date centuries old is a perfectly ordinary date to a comparison.
// The refusal is the only place that can notice.
//
// Set where no personal-finance journal reaches and every fumbled year lands:
// the mistake this catches produces a year in the first two digits' worth of
// wrongness (0226, 1026, 1226), never 1899.
var minOccurredOn = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)

// checkOccurredOn holds a date to both ends of the range an operation may be
// entered in. One function rather than two comparisons at each of the two write
// paths (an ordinary operation and a transfer): they had drifted to the point
// of checking different things already — the transfer path had the ceiling and
// not the floor — and a rule about when something happened should not depend on
// which endpoint recorded it.
func checkOccurredOn(d time.Time) error {
	if d.After(dates.LatestRecordable()) {
		return fmt.Errorf("%w: occurred_on must not be in the future", family.ErrValidation)
	}
	if d.Before(minOccurredOn) {
		return fmt.Errorf("%w: occurred_on must not be earlier than %s",
			family.ErrValidation, minOccurredOn.Format("2006-01-02"))
	}
	return nil
}

// validate checks operation fields that are cheap and local — i.e. don't
// require replaying the journal. See the package's task brief for the
// per-type contract; note the engine itself does not check amount sign or
// non-zero-ness for dividend/coupon/tax/fee/interest/deposit/withdrawal, so
// the service must, or silent corruption of income/fees becomes possible.
//
// It is THE HAND-ENTRY PATH'S rule and only that one. The importer has its own
// (validateImported), because the two differ on exactly two types and agree on
// everything else; what they agree on lives in the two helpers below, so that
// the import's arrival cannot quietly move a rule a person's own entry is
// checked against.
func validate(o Operation) error {
	if err := validateFields(o); err != nil {
		return err
	}
	if o.Type == TypeTransferIn || o.Type == TypeTransferOut {
		// Unchanged: hand entry writes a transfer through the endpoint that
		// records both legs at once, so that the shares and the basis they
		// carry cannot end up in different accounts. The import path admits a
		// leg on its own instead (see validateImported), because a broker
		// reports shares arriving without reporting where they came from.
		return fmt.Errorf("%w: use the transfer endpoint for %s", family.ErrValidation, o.Type)
	}
	if o.Type == TypeExchangeOut || o.Type == TypeExchangeIn {
		// A conversion has no hand-entry door AT ALL, and this is not the
		// transfer's "use the other endpoint" — there is no other endpoint. What
		// happened to a paper is a fact about the PAPER, true for everyone who
		// held it, so it is recorded once in the corporate-actions registry and
		// applied to every account from there (see Service.CreateExchange, whose
		// only caller that is). A leg typed in against one account would be a
		// second place the same fact lives, and the two would disagree the first
		// time one of them was edited.
		return fmt.Errorf("%w: a conversion is recorded in the corporate-actions registry, not entered against an account", family.ErrValidation)
	}
	return validateByType(o)
}

// validateFields is every check that looks at a field's own value rather than
// at what the type means — shape, bounds, and the two money fields agreeing
// with each other. Both write paths run it, unchanged, before they part ways.
func validateFields(o Operation) error {
	if !o.Type.Valid() {
		return fmt.Errorf("%w: invalid operation type", family.ErrValidation)
	}
	if !currency.Valid(o.Currency) {
		return fmt.Errorf("%w: currency must be ISO-4217 uppercase", family.ErrValidation)
	}
	if err := checkOccurredOn(o.OccurredOn); err != nil {
		return err
	}
	if o.FeeMinor < 0 {
		return fmt.Errorf("%w: fee_minor must be >= 0", family.ErrValidation)
	}
	// Bounds are checked with explicit comparisons rather than an abs() so
	// that math.MinInt64 (whose negation overflows) is rejected too.
	if o.AmountMinor > money.MaxAmountMinor || o.AmountMinor < -money.MaxAmountMinor {
		return fmt.Errorf("%w: amount_minor must be within ±%d", family.ErrValidation, money.MaxAmountMinor)
	}
	if o.FeeMinor > money.MaxAmountMinor {
		return fmt.Errorf("%w: fee_minor must be <= %d", family.ErrValidation, money.MaxAmountMinor)
	}
	// The two factors, their product, and the ratio (see maxQuantity). Checked
	// here rather than in the per-type branches below: nothing stops any other
	// type from carrying a quantity, a price or a ratio, and the columns are the
	// same columns whichever type wrote them. Magnitudes, not signed values,
	// because these bounds are about size — a sign that has no business being
	// negative is the business of the per-type rules below, which is where it is
	// already caught for buy, sell and split.
	//
	// A conversion is checked with everything else, though its row never reaches
	// a position: the engine skips the type before it touches one, so the product
	// check can only ever refuse there and cannot protect a valuation. It is kept
	// because the bound is not a claim about the engine — it is the row's two
	// money fields agreeing with each other, and the row is published in the
	// journal whether or not a position was built from it. Exempting one type
	// would also put back the per-type reasoning this block exists to avoid, on
	// the very type most likely to grow a price one day: a conversion's rate is a
	// price per unit in all but name. Keeping the check costs a refusal nothing
	// would have read; dropping it costs a special case to remember.
	if o.Quantity != nil {
		if err := checkQuantityBound(*o.Quantity); err != nil {
			return err
		}
	}
	if o.Price != nil && o.Price.Abs().GreaterThan(maxPrice) {
		return fmt.Errorf("%w: price must be within ±%s per unit", family.ErrValidation, maxPrice)
	}
	if o.Quantity != nil && o.Price != nil {
		if notional := o.Price.Mul(*o.Quantity).Abs().Shift(minorUnitScale); notional.GreaterThan(maxAmountMinorDec) {
			return fmt.Errorf("%w: price × quantity must be within ±%d minor units", family.ErrValidation, money.MaxAmountMinor)
		}
	}
	// The ratio is the one field whose damage is done to a position rather than
	// to its own row: applySplit multiplies the WHOLE holding, so a ratio no
	// larger than this can still carry a position built one accepted buy at a
	// time past anything the read side can value. That is not a reason to leave
	// it unbounded — it is the reason the read-side guards stay.
	if o.SplitRatio != nil && o.SplitRatio.Abs().GreaterThanOrEqual(maxSplitRatio) {
		return fmt.Errorf("%w: split_ratio must be less than %s", family.ErrValidation, maxSplitRatio)
	}
	return nil
}

// validateByType is the per-type contract for every type the two write paths
// agree about — which is all of them but transfer_in and transfer_out, where
// they disagree by design (see validate and validateImported).
//
// It has NO branch for those two, deliberately. Each path decides what a
// transfer leg means to it and says so before delegating here; a branch that
// also refused them would let one of those decisions be deleted without a
// single test noticing, since the fall-through would go on refusing in its own
// words. A type with no branch here is accepted by this function, which is why
// a caller must never hand it a type it has not already ruled on.
func validateByType(o Operation) error {
	switch o.Type {
	case TypeBuy, TypeSell, TypeRedemption:
		if o.InstrumentID == nil {
			return fmt.Errorf("%w: %s requires an instrument", family.ErrValidation, o.Type)
		}
		if o.Quantity == nil || !o.Quantity.IsPositive() {
			return fmt.Errorf("%w: %s requires positive quantity", family.ErrValidation, o.Type)
		}
		if o.Price != nil && !o.Price.IsPositive() {
			return fmt.Errorf("%w: price must be positive when given", family.ErrValidation)
		}
		if o.Type == TypeBuy && o.AmountMinor >= 0 {
			return fmt.Errorf("%w: buy amount_minor must be negative", family.ErrValidation)
		}
		if (o.Type == TypeSell || o.Type == TypeRedemption) && o.AmountMinor <= 0 {
			return fmt.Errorf("%w: %s amount_minor must be positive", family.ErrValidation, o.Type)
		}
	case TypeDeposit, TypeInterest, TypeDividend, TypeCoupon, TypeAmortization:
		if o.AmountMinor <= 0 {
			return fmt.Errorf("%w: %s amount_minor must be positive", family.ErrValidation, o.Type)
		}
	case TypeWithdrawal, TypeFee, TypeTax:
		if o.AmountMinor >= 0 {
			return fmt.Errorf("%w: %s amount_minor must be negative", family.ErrValidation, o.Type)
		}
	case TypeSplit:
		if o.InstrumentID == nil {
			return fmt.Errorf("%w: split requires an instrument", family.ErrValidation)
		}
		if o.SplitRatio == nil || !o.SplitRatio.IsPositive() {
			return fmt.Errorf("%w: split requires positive split_ratio", family.ErrValidation)
		}
		if o.AmountMinor != 0 {
			return fmt.Errorf("%w: split amount_minor must be 0", family.ErrValidation)
		}
		// A SPLIT IS WRITTEN FROM THE REGISTRY AND FROM NOWHERE ELSE, and this
		// rule used to say the opposite — source=manual only, importers
		// refused. Both halves were right about their own half and wrong
		// together. An importer must not invent a split, because no broker
		// reports one; a person must not enter one per account either, because
		// a split happened to the PAPER and typing it into one account leaves
		// every other holder of the same paper wrong, with nothing to say so.
		// So the fact is recorded once, against the ISIN, and carried into
		// every account that held it (see internal/corporateaction).
		//
		// The two paths reach this same line: the hand-entry one refuses
		// "manual" here, and the import path refuses every source but this one
		// before it delegates (see validateImported). Nothing else in the
		// program writes a split.
		if o.Source != SourceRegistry {
			return fmt.Errorf(
				"%w: a split is recorded once for the security in the corporate-actions registry, not entered on an account",
				family.ErrValidation)
		}
	case TypeExchangeOut, TypeExchangeIn:
		// Reachable only through the import path's validateImported: the hand
		// path refuses both types before it delegates here (see validate), and
		// CreateExchange builds its legs itself. It is written all the same, so
		// that a future importer teaching itself to project a broker's
		// conversion is refused by a rule rather than by nobody: a conversion
		// is the registry's to write, and a leg arriving from anywhere else
		// would be a second, unsynchronised record of one corporate action.
		if o.InstrumentID == nil {
			return fmt.Errorf("%w: %s requires an instrument", family.ErrValidation, o.Type)
		}
		if o.Quantity == nil || !o.Quantity.IsPositive() {
			return fmt.Errorf("%w: %s requires positive quantity", family.ErrValidation, o.Type)
		}
		if o.AmountMinor < 0 {
			return fmt.Errorf("%w: %s amount_minor (cost basis) must be >= 0", family.ErrValidation, o.Type)
		}
		if o.Source != SourceRegistry {
			return fmt.Errorf("%w: %s is only supported for source=%s", family.ErrValidation, o.Type, SourceRegistry)
		}
	case TypeConversion:
		// cash-level: any sign is legitimate (buying vs. selling currency).
	}
	// Instrument requirement for cash-level types beyond buy/sell/split is
	// intentionally not enforced here: dividend/coupon/tax/fee may be
	// recorded at the cash level without an instrument.
	return nil
}

// checkJournal replays the account's journal — minus removeIDs, plus add —
// through the portfolio engine and reports whether it stays consistent.
// Candidates in add get CreatedAt = time.Now() before sorting, so within
// their occurred_on date they sort after any existing operation.
//
// It takes the store rather than reading through the Service's own because
// every caller runs it inside Store.WithAccountsLocked and must read through
// THAT transaction's store: a journal read on the pool while a decision is being
// made in a transaction is the very gap the lock was taken to close.
func checkJournal(ctx context.Context, st *Store, spaceID, accountID uuid.UUID,
	add []Operation, removeIDs map[uuid.UUID]bool,
) error {
	ops, err := st.ListForEngine(ctx, spaceID, accountID)
	if err != nil {
		return err
	}
	return checkJournalOps(ops, add, removeIDs)
}

// journalWith assembles the journal as it would stand with add appended and
// removeIDs gone, in the order the engine folds it. Candidates in add get
// CreatedAt = time.Now(), so within their occurred_on date they sort after any
// existing operation.
func journalWith(ops []Operation, add []Operation, removeIDs map[uuid.UUID]bool) []Operation {
	journal := make([]Operation, 0, len(ops)+len(add))
	for _, o := range ops {
		if !removeIDs[o.ID] {
			journal = append(journal, o)
		}
	}
	for _, o := range add {
		o.CreatedAt = time.Now()
		journal = append(journal, o)
	}
	sortJournal(journal)
	return journal
}

// sortJournal puts operations in the order the engine folds them, which is the
// order ListForEngine reads them back in: by the day they happened, then by the
// rank their source gives them within that day, then by when they were
// recorded. One function because two paths assemble a journal to fold — this
// one and the import's (see ApplyImportDelta and prepareCandidate, which sort
// journals whose rows already carry a created_at) — and an order that differs
// between the check and the read is the fault this package spends most of its
// care on.
//
// The rank comes from foldRank, which is the same rule the SQL reads (see
// foldorder.go): a row the corporate-actions registry materialized folds at the
// START of its day, ahead of trades dated the same day, because it is stamped
// years after them and would otherwise multiply a quantity that already counts
// the split.
func sortJournal(journal []Operation) {
	sort.SliceStable(journal, func(i, j int) bool {
		if !journal[i].OccurredOn.Equal(journal[j].OccurredOn) {
			return journal[i].OccurredOn.Before(journal[j].OccurredOn)
		}
		if ri, rj := foldRank(journal[i].Source), foldRank(journal[j].Source); ri != rj {
			return ri < rj
		}
		return journal[i].CreatedAt.Before(journal[j].CreatedAt)
	})
}

// checkJournalOps is checkJournal over an already-loaded journal, so a caller
// that has fetched the account's operations for another reason (see
// CreateTransfer) does not pay for a second round trip.
func checkJournalOps(ops []Operation, add []Operation, removeIDs map[uuid.UUID]bool) error {
	if _, err := portfolio.Compute(journalWith(ops, add, removeIDs)); err != nil {
		return fmt.Errorf("%w: %v", ErrInconsistent, err)
	}
	return nil
}

// journalUpTo returns the prefix of ops that occurred on or before day —
// the state of the journal a transfer dated day is replayed against.
// Same-day operations are kept: within a date the engine orders by
// created_at, and a newly created operation is the youngest of its date.
func journalUpTo(ops []Operation, day time.Time) []Operation {
	out := make([]Operation, 0, len(ops))
	for _, o := range ops {
		if !o.OccurredOn.After(day) {
			out = append(out, o)
		}
	}
	return out
}

// quantizeLots brings every piece of a transfer's FIFO breakdown onto the
// quantity scale the journal can actually store, so that what the write path
// computes is what the read path gets back. total is the quantity the transfer
// moves, itself already on that scale.
//
// A piece's quantity is a fraction of a lot, and a lot's quantity used not to
// be bound to ten decimal places: a split multiplied it by a ratio, and a
// reverse split by 0.3333333333 turned two whole shares into 1.16666666655
// apiece. Storing such pieces as they were meant Postgres rounded each one on
// its own, independently, and two pieces rounding up pushed the stored sum
// 1e-10 above the stored quantity of the transfer itself. The engine checks
// that sum on every read (portfolio.CheckTransferLots), so the transfer was
// accepted and the receiving account's positions screen then failed forever —
// for data the application wrote itself.
//
// Since the engine keeps lots on the journal's scale (portfolio.QuantityScale
// and Position.applySplit), a release of those lots cannot produce a piece off
// the scale in the first place, and the arithmetic below normally changes
// nothing. It stays because the two properties it enforces are not the engine's
// to promise: that the pieces sum to the quantity the OPERATION claims to move,
// exactly, and that nothing unstorable reaches the table. Cheap insurance on
// the one write that both a position and a journal row are later derived from.
//
// The allocation is the one releaseFIFO already uses for costs, applied to
// quantities: truncate the RUNNING TOTAL to the scale and give each piece the
// difference from the previous piece's running total. Every piece is then
// exactly representable, no piece can exceed its exact share by more than the
// last digit, and the final running total is total itself, so the pieces sum
// to the moved quantity exactly rather than approximately. Truncating each
// piece on its own instead would leave a remainder unaccounted for.
//
// A piece can come out of this with nothing left — either because its share was
// finer than the scale, or because the lot behind it holds no shares at all,
// which is what a deep enough reverse split leaves (see portfolio.Lot). Such a
// piece is dropped rather than stored as a zero — the table rejects zero
// quantities, and a piece of nothing did not move — but its COST is real money
// and is carried onto the next surviving piece, whose acquisition date it then
// shares. Cost is never invented and never lost: the pieces still sum to the
// same basis, which is why CreateTransfer can go on summing them for the
// operation's own amount. The last piece always survives (its quantity is total
// minus everything already placed, total is positive, and a shareless lot is
// never the last thing a release consumes — the loop only stops when it has
// taken everything it needs), so there is always somewhere for a carry to land.
func quantizeLots(pieces []portfolio.ReleasedLot, total decimal.Decimal) []portfolio.ReleasedLot {
	out := make([]portfolio.ReleasedLot, 0, len(pieces))
	exact := decimal.Zero  // running total of the pieces as computed
	placed := decimal.Zero // running total of the pieces as they will be stored
	var carry int64        // cost of pieces too small to be stored at all
	for i, pc := range pieces {
		exact = exact.Add(pc.Quantity)
		upTo := exact.Truncate(quantityScale)
		if i == len(pieces)-1 {
			// The pieces of a release sum to the released quantity exactly, and
			// that quantity is already on the scale, so this is what truncating
			// the final running total yields anyway. Saying so outright means a
			// breakdown that somehow did not add up is caught by the write-time
			// check as a mismatch instead of being quietly trimmed here.
			upTo = total
		}
		qty := upTo.Sub(placed)
		if qty.IsZero() {
			carry += pc.CostMinor
			continue
		}
		placed = upTo
		out = append(out, portfolio.ReleasedLot{
			Quantity: qty, CostMinor: pc.CostMinor + carry, AcquiredOn: pc.AcquiredOn,
		})
		carry = 0
	}
	return out
}

// rescaleLots restates a FIFO breakdown in another paper's units: the pieces
// that gave up `from` units of the old instrument come back describing `to`
// units of the new one, each keeping its cost basis and its acquisition date
// untouched.
//
// ONLY THE QUANTITIES MOVE, and that is the whole of what a conversion does to a
// parcel (see portfolio.TypeExchangeOut). Scaling the cost as well would be the
// mistake this function is easiest to write: the money did not change, only the
// number of certificates it is spread over, and a basis that shrank with a
// 1-for-10 conversion would hand the owner a nine-tenths loss the law says did
// not happen.
//
// THE ALLOCATION IS quantizeLots' AND IS NOT REIMPLEMENTED HERE. Each piece is
// multiplied out at full precision and the running total is what gets truncated
// to the journal's scale, so the pieces sum to `to` EXACTLY rather than to
// whatever a piece-by-piece rounding happens to leave — the same rule
// Position.applySplit follows for the lots of a position, and for the same
// reason: a breakdown that misses the quantity of the row carrying it is
// refused by portfolio.CheckTransferLots on every later read, which is the
// "accepted on write, refused on every read" shape this package has been bitten
// by twice. Reusing the function rather than copying its five lines is what
// keeps the two from drifting.
//
// The multiplication is per piece — quantity × to ÷ from — rather than by a
// ratio computed once, so no piece is scaled by a pre-rounded factor. Division
// is inexact by nature and each piece may land a hair off; the last piece is
// pinned to `to` by quantizeLots regardless, and the truncation of every running
// total before it is downward, so no piece can come out negative.
func rescaleLots(pieces []ReleasedLot, from, to decimal.Decimal) []ReleasedLot {
	scaled := make([]ReleasedLot, 0, len(pieces))
	for _, pc := range pieces {
		scaled = append(scaled, ReleasedLot{
			Quantity:   pc.Quantity.Mul(to).Div(from),
			CostMinor:  pc.CostMinor,
			AcquiredOn: pc.AcquiredOn,
		})
	}
	return quantizeLots(scaled, to)
}

// mapWriteError translates pgconn constraint violations from Store.Create
// into domain errors the caller can act on.
func mapWriteError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch {
	case pgErr.Code == pgForeignKeyViolation && pgErr.ConstraintName == "operations_instrument_id_fkey":
		return fmt.Errorf("%w: instrument not found", family.ErrValidation)
	case pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == "operations_dedup_idx":
		return fmt.Errorf("%w: duplicate external id", family.ErrValidation)
	}
	return err
}

// normalizeForStorage brings the decimal fields the engine replays onto the
// scale the journal stores them at, BEFORE anything is validated or checked,
// so that the operation the consistency check folds is bit for bit the row the
// database will hold.
//
// Without it the check and the row are two different operations. A request may
// carry any number of decimal places; the columns keep ten and Postgres rounds
// the rest away — to NEAREST, so a quantity can land ABOVE the one that was
// checked. Selling a whole position of 0.116666666655 was accepted against
// that exact figure and recorded as 0.1166666667, and from then on every read
// of the account compared the recorded 0.1166666667 against the position it was
// supposed to empty and answered "not enough quantity": a 201 followed by a
// positions screen broken forever, for a row this program wrote itself. The
// same divergence on split_ratio is subtler and rarer — a ratio rounded up or
// down changes every later quantity in the position by a factor — but it is the
// same fault, so both are normalized here.
//
// The rounding is DOWN, matching CreateTransfer: rounding a "sell everything I
// hold" up past the position it empties would answer a perfectly good request
// with an oversell, which is a loud refusal of healthy data. The response
// returns the stored row, so the client is always told exactly what was
// recorded. Down does not always mean "slightly less", though: a split_ratio
// truncated down shrinks every later quantity in the position, so a release
// already recorded against the untruncated position can end up larger than
// what is left and be refused outright. That refusal is loud and the row is
// recoverable by deleting and re-entering it, which is the trade being made
// against a rounding that would break the screen instead.
//
// Price is deliberately left alone: it is never replayed, never compared
// against anything, and the row that comes back is the stored one, so a price
// rounded by the column misstates nothing.
func normalizeForStorage(op *Operation) error {
	onScale := func(v *decimal.Decimal, field string) (*decimal.Decimal, error) {
		if v == nil {
			return nil, nil
		}
		t := v.Truncate(quantityScale)
		if v.IsPositive() && !t.IsPositive() {
			return nil, fmt.Errorf("%w: %s is finer than the %d decimal places the journal records",
				family.ErrValidation, field, quantityScale)
		}
		return &t, nil
	}
	quantity, err := onScale(op.Quantity, "quantity")
	if err != nil {
		return err
	}
	splitRatio, err := onScale(op.SplitRatio, "split_ratio")
	if err != nil {
		return err
	}
	op.Quantity, op.SplitRatio = quantity, splitRatio
	return nil
}

// Create validates op, checks that appending it keeps the account's journal
// consistent under the portfolio engine, and persists it.
//
// THE READ, THE CHECK AND THE WRITE ARE ONE TRANSACTION HOLDING THE ACCOUNT'S
// JOURNAL LOCK (see Store.WithAccountsLocked). They were three separate steps
// on the pool until issue #17: two sells of one holding submitted at the same
// moment each replayed a journal without the other, each was told it fitted,
// and both were written — leaving a journal that replays for nobody and a
// positions screen answering 422 for good, out of two requests that were each
// perfectly fine on their own. Under the lock the second request reads the
// first one's row and is refused exactly as it would have been a second later.
//
// Inside it the journal is read once and folded twice: for the check that
// decides whether the request is accepted, and — with the row as the database
// actually stored it — for the confirmation that runs before the commit. That
// second replay is what makes "what was checked is what was written" a property
// of the code rather than an argument about rounding: if the two ever diverge
// again, the write is rolled back on the request that caused it instead of
// silently breaking every later read by someone else. It is the same guard
// CreatePair applies to a transfer's breakdown.
func (s *Service) Create(ctx context.Context, spaceID uuid.UUID, op Operation) (Operation, error) {
	if err := normalizeForStorage(&op); err != nil {
		return Operation{}, err
	}
	if err := validate(op); err != nil {
		return Operation{}, err
	}
	var created Operation
	err := s.store.WithAccountsLocked(ctx, spaceID, []uuid.UUID{op.AccountID}, func(st *Store) error {
		journal, err := st.ListForEngine(ctx, spaceID, op.AccountID)
		if err != nil {
			return err
		}
		if err := checkJournalOps(journal, []Operation{op}, nil); err != nil {
			return err
		}
		created, err = st.Create(ctx, spaceID, op, func(stored Operation) error {
			// Not wrapped in ErrInconsistent: the caller's journal was fine a
			// moment ago and their request was accepted, so reaching this is a
			// bug in this program, not something they did. It must read as a
			// server failure rather than as "your history contradicts itself".
			// A COMPETING WRITE can no longer arrive here now that the check runs
			// under the lock — it is refused by the check above instead. What is
			// left for this to catch is the fault it was written for: a row the
			// columns stored differently from the one that was checked.
			if _, err := portfolio.Compute(journalWith(journal, []Operation{stored}, nil)); err != nil {
				return fmt.Errorf("the operation as stored no longer replays: %v", err)
			}
			return nil
		})
		return err
	})
	if err != nil {
		return Operation{}, mapWriteError(err)
	}
	return created, nil
}

// CreateTransfer moves an in-kind position between two accounts as an
// atomic transfer_out/transfer_in pair sharing the moved cost basis.
//
// Everything that reads a journal, decides on it or writes runs inside ONE
// transaction holding BOTH accounts' journal locks (see
// Store.WithAccountsLocked) — the same closure of the check-then-write window
// Create takes, and it needs it more: the source's FIFO release is resolved
// against the journal as it stood on the transfer's date, and a sell landing
// under that read would leave the pair naming lots that are no longer there.
func (s *Service) CreateTransfer(ctx context.Context, spaceID uuid.UUID, p TransferParams) (out, in Operation, err error) {
	if p.FromAccountID == p.ToAccountID {
		return Operation{}, Operation{}, fmt.Errorf("%w: from and to accounts must differ", family.ErrValidation)
	}
	// A missing instrument_id is a missing field, and it has to be refused as
	// one HERE, before the source journal is searched. The search is what used
	// to answer instead: a request without the field decodes to the nil UUID,
	// no operation in any journal carries that instrument, and the refusal came
	// back «no source history for instrument» (#19) — which names a plausible
	// and entirely different mistake (you hold nothing of this paper on that
	// account) and sends the reader looking through a journal that is fine.
	// The status was already right; only the sentence was wrong.
	if p.InstrumentID == uuid.Nil {
		return Operation{}, Operation{}, fmt.Errorf("%w: instrument_id is required", family.ErrValidation)
	}
	if !p.Quantity.IsPositive() {
		return Operation{}, Operation{}, fmt.Errorf("%w: quantity must be positive", family.ErrValidation)
	}
	// Work with the quantity the journal can actually hold, from here on and
	// everywhere: the column keeps ten decimal places, and a request with more
	// of them would otherwise be released and broken down at full precision
	// while the row landed rounded — leaving the stored breakdown summing to
	// something the stored operation does not claim (see quantizeLots).
	// Rounding DOWN, not to nearest: the alternative can round a "move
	// everything I hold" up past the position it is emptying and answer a
	// perfectly good request with an oversell.
	quantity := p.Quantity.Truncate(quantityScale)
	if !quantity.IsPositive() {
		return Operation{}, Operation{}, fmt.Errorf("%w: quantity is finer than the %d decimal places the journal records",
			family.ErrValidation, quantityScale)
	}
	// The same bound the operation's own quantity gets (see maxQuantity), because
	// this writes the same column and validate never sees these two rows. It is
	// reachable rather than symmetry for its own sake: the bound is per
	// operation, a position is the sum of many, so a holding can grow past it one
	// accepted buy at a time and then be moved in a single transfer.
	if err := checkQuantityBound(quantity); err != nil {
		return Operation{}, Operation{}, err
	}
	if err := checkOccurredOn(p.OccurredOn); err != nil {
		return Operation{}, Operation{}, err
	}

	var cOut, cIn Operation
	err = s.store.WithAccountsLocked(ctx, spaceID, []uuid.UUID{p.FromAccountID, p.ToAccountID}, func(st *Store) error {
		sourceJournal, err := st.ListForEngine(ctx, spaceID, p.FromAccountID)
		if err != nil {
			return err
		}

		currency := ""
		for i := len(sourceJournal) - 1; i >= 0; i-- {
			o := sourceJournal[i]
			if o.InstrumentID != nil && *o.InstrumentID == p.InstrumentID {
				currency = o.Currency
				break
			}
		}
		if currency == "" {
			return fmt.Errorf("%w: no source history for instrument", family.ErrValidation)
		}

		cost := int64(0)
		var lots []ReleasedLot
		if p.CostMinorOverride != nil {
			// A basis given by hand is not a release of anything: there are no
			// source lots behind it and therefore no acquisition dates to carry.
			// The destination lot gets no date at all — not the transfer's own —
			// because a lot's date claims to say when its shares were bought, and
			// nobody recorded that here (see portfolio.Lot.AcquiredOn and
			// Compute's TypeTransferIn branch, which is where that lot is actually
			// built). Inventing pieces, or a date, here would fabricate history.
			//
			// THE DEPARTING LEG CARRIES THIS NUMBER TOO, and it is not the basis
			// that leaves the source: the engine has no breakdown to release, so
			// it gives up a fresh FIFO slice of the source's own queue and throws
			// its cost away (see Compute's transfer_out branch). The two legs are
			// deliberately not reconciled here — the owner said what the parcel
			// was worth and the journal does not contradict them — and the API
			// contract says so on both `TransferRequest.cost_minor` and
			// `Operation.amount_minor` rather than describing every transfer's
			// amount as one the queue produced (issue #17).
			cost = *p.CostMinorOverride
			if cost < 0 || cost > money.MaxAmountMinor {
				return fmt.Errorf("%w: cost_minor must be within 0..%d", family.ErrValidation, money.MaxAmountMinor)
			}
		} else {
			// The basis must come from the journal as it stood on the transfer's
			// own date, not from the end state: a backdated transfer is replayed
			// by the engine at its chronological place, where the FIFO front is
			// different. Folding the whole journal here would capture the basis
			// of lots bought (or left over after sells) *after* the transfer and
			// mint cost out of thin air. Same-date operations count as preceding,
			// matching checkJournalOps, where the candidate sorts last within its
			// own date.
			//
			// The pieces are taken, not just their total: the destination needs
			// the day each one was bought to value it at that day's exchange
			// rate. The carried basis is then the sum of these very pieces — it
			// is never computed a second way, so the two cannot drift apart.
			lots, err = portfolio.ReleasedLots(journalUpTo(sourceJournal, p.OccurredOn), p.InstrumentID, quantity)
			if err != nil {
				return fmt.Errorf("%w: %v", ErrInconsistent, err)
			}
			// Quantized before the basis is summed, not after: quantizing can merge
			// a piece too small to store into its neighbour, and the operation's
			// amount must be the sum of the pieces that are actually written.
			lots = quantizeLots(lots, quantity)
			cost = portfolio.LotsCost(lots)
		}

		outOp := Operation{
			AccountID: p.FromAccountID, InstrumentID: &p.InstrumentID, Type: TypeTransferOut,
			OccurredOn: p.OccurredOn, Quantity: &quantity, AmountMinor: cost,
			Currency: currency, Note: p.Note,
			// The departing leg carries the breakdown as well, even though only
			// the arriving one stores it (CreatePair writes in.TransferLots, and
			// Store.attachTransferLots hands the same rows to both legs at every
			// later read). It matters here and not merely for symmetry: the engine
			// releases the lots this breakdown names rather than a fresh FIFO slice
			// (see portfolio.Position.releaseRecorded), so a candidate without it
			// is not the row the check below is supposed to be checking.
			TransferLots: lots,
		}
		inOp := Operation{
			AccountID: p.ToAccountID, InstrumentID: &p.InstrumentID, Type: TypeTransferIn,
			OccurredOn: p.OccurredOn, Quantity: &quantity, AmountMinor: cost,
			Currency: currency, Note: p.Note,
			// The breakdown rides on the arriving leg: its account is the one
			// that would otherwise lose the acquisition dates. CreatePair writes
			// it in the same transaction as the pair itself.
			TransferLots: lots,
		}

		// The two legs touch different accounts' journals, so each is checked
		// independently: the source loses the position (checked against its own
		// history), the destination gains a fresh lot (always consistent on its
		// own, but checked for uniformity and to catch a same-account edge case
		// earlier logic might have missed).
		if err := checkJournalOps(sourceJournal, []Operation{outOp}, nil); err != nil {
			return err
		}
		if err := checkJournal(ctx, st, spaceID, p.ToAccountID, []Operation{inOp}, nil); err != nil {
			return err
		}

		// And the same pair once more, as the database actually kept it — the guard
		// Create has always had for a single operation, now that the departing leg
		// replays the STORED pieces rather than a fresh slice of the queue (see
		// portfolio.Position.releaseRecorded). What was checked above is an
		// in-memory candidate; what every later read of the source account folds is
		// the row below, with the quantities the columns rounded to. Twice already a
		// difference between those two was accepted with a 201 and then refused on
		// every later read by someone else (see quantizeLots and normalizeForStorage),
		// and the second leg's release is a third way in. Not wrapped in
		// ErrInconsistent: the caller's journal was fine and their request was
		// accepted, so reaching this is a bug in this program, not something they did.
		cOut, cIn, err = st.CreatePair(ctx, spaceID, outOp, inOp, func(storedOut, _ Operation) error {
			if _, err := portfolio.Compute(journalWith(sourceJournal, []Operation{storedOut}, nil)); err != nil {
				return fmt.Errorf("the transfer as stored no longer replays on the source account: %v", err)
			}
			return nil
		})
		return err
	})
	if err != nil {
		return Operation{}, Operation{}, mapWriteError(err)
	}
	return cOut, cIn, nil
}

// ExchangeParams describes a securities conversion on ONE account: `Quantity`
// units of `FromInstrumentID` become `ToQuantity` units of `ToInstrumentID` on
// `OccurredOn`.
//
// Both counts are given rather than a ratio. A corporate action is announced as
// "N old for M new", the registry records exactly that (two whole numbers, so
// nothing is pre-rounded), and this is the one place where those two numbers
// meet the account's actual holding — which is neither of them.
type ExchangeParams struct {
	AccountID        uuid.UUID
	FromInstrumentID uuid.UUID
	ToInstrumentID   uuid.UUID
	Quantity         decimal.Decimal
	ToQuantity       decimal.Decimal
	OccurredOn       time.Time
	Source           string
	Note             string
}

// CreateExchange records a securities conversion as an atomic
// exchange_out/exchange_in pair on one account: the old paper gives up the very
// lots named in the breakdown, and the new paper is built from those same lots
// with their costs and acquisition dates intact and only their quantities
// restated (see portfolio.TypeExchangeOut for why the law says nothing else may
// change).
//
// IT IS THE REGISTRY'S ENTRY POINT AND NOBODY ELSE'S. What happened to a paper
// is true for every account that held it, so it is recorded once in the
// corporate-actions registry and applied from there; a Source other than
// SourceRegistry is refused here as well as in validateByType, because this
// function does not route through validate at all (CreateTransfer does not
// either — a pair builds its own legs) and a rule only the other path enforces
// is a rule this path does not have.
//
// THE SHAPE IS CreateTransfer'S, with one difference that matters at every
// step: both legs land on the SAME account. So one lock is taken rather than
// two, one journal is read rather than two, and — the part that would be easy to
// get wrong — the two candidates are checked TOGETHER against that one journal.
// Checking them one at a time would fold a journal in which the old paper had
// left and the new one had not yet arrived, which is not a state the account is
// ever in.
func (s *Service) CreateExchange(ctx context.Context, spaceID uuid.UUID, p ExchangeParams) (out, in Operation, err error) {
	if p.Source != SourceRegistry {
		return Operation{}, Operation{}, fmt.Errorf("%w: a conversion is only written by source=%s", family.ErrValidation, SourceRegistry)
	}
	if p.FromInstrumentID == uuid.Nil || p.ToInstrumentID == uuid.Nil {
		return Operation{}, Operation{}, fmt.Errorf("%w: from and to instruments are required", family.ErrValidation)
	}
	// THE SAME PAPER ON BOTH SIDES IS NOT A CONVERSION, it is a split written
	// with extra steps — and it would fold as one position releasing lots and
	// immediately re-adding them, whose result depends on the order of two rows
	// sharing a date. A split is the type for "the same paper, a different
	// count" and it already exists.
	if p.FromInstrumentID == p.ToInstrumentID {
		return Operation{}, Operation{}, fmt.Errorf("%w: from and to instruments must differ; the same paper in a new count is a split", family.ErrValidation)
	}
	if !p.Quantity.IsPositive() || !p.ToQuantity.IsPositive() {
		return Operation{}, Operation{}, fmt.Errorf("%w: both quantities must be positive", family.ErrValidation)
	}
	// Truncated for the same reason a transfer's quantity is (see
	// CreateTransfer): what the columns can hold is what the check must be run
	// against, and rounding DOWN so that "convert everything I hold" can never
	// round up past the position it is emptying.
	quantity := p.Quantity.Truncate(quantityScale)
	toQuantity := p.ToQuantity.Truncate(quantityScale)
	if !quantity.IsPositive() || !toQuantity.IsPositive() {
		return Operation{}, Operation{}, fmt.Errorf("%w: a quantity is finer than the %d decimal places the journal records",
			family.ErrValidation, quantityScale)
	}
	if err := checkQuantityBound(quantity); err != nil {
		return Operation{}, Operation{}, err
	}
	if err := checkQuantityBound(toQuantity); err != nil {
		return Operation{}, Operation{}, err
	}
	if err := checkOccurredOn(p.OccurredOn); err != nil {
		return Operation{}, Operation{}, err
	}

	var cOut, cIn Operation
	err = s.store.WithAccountsLocked(ctx, spaceID, []uuid.UUID{p.AccountID}, func(st *Store) error {
		journal, err := st.ListForEngine(ctx, spaceID, p.AccountID)
		if err != nil {
			return err
		}

		// The currency the old paper's cost is denominated in, read off the
		// account's own history exactly as a transfer reads it. The new paper
		// inherits it, and must: what arrives is the money that was paid, and
		// that money has a currency of its own regardless of what the new paper
		// is quoted in. If the account already holds the new paper in a
		// different currency the engine refuses the pair, loudly, rather than
		// mixing two currencies inside one basis.
		currency := ""
		for i := len(journal) - 1; i >= 0; i-- {
			o := journal[i]
			if o.InstrumentID != nil && *o.InstrumentID == p.FromInstrumentID {
				currency = o.Currency
				break
			}
		}
		if currency == "" {
			return fmt.Errorf("%w: no history for the instrument being converted", family.ErrValidation)
		}

		// Resolved against the journal as it stood on the conversion's own date,
		// not against the end state: a backdated conversion is replayed at its
		// chronological place, where the FIFO front is a different one.
		lots, err := portfolio.ReleasedLots(journalUpTo(journal, p.OccurredOn), p.FromInstrumentID, quantity)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInconsistent, err)
		}
		lots = quantizeLots(lots, quantity)
		cost := portfolio.LotsCost(lots)
		arriving := rescaleLots(lots, quantity, toQuantity)
		// NOT AN ARGUMENT ABOUT rescaleLots BUT A CHECK ON IT. The two legs
		// carry one and the same amount_minor, and portfolio.CheckTransferLots
		// holds each leg's pieces to the amount of the row carrying them — so a
		// basis lost or invented in the restating would surface as a refusal on
		// every later read of this account rather than here. Saying it outright,
		// on the write that caused it, is the difference between a bug reported
		// against the request that made it and one reported against whoever next
		// opens the screen.
		if got := portfolio.LotsCost(arriving); got != cost {
			return fmt.Errorf("restating the breakdown in the new paper's units changed the basis from %d to %d minor units", cost, got)
		}

		outOp := Operation{
			AccountID: p.AccountID, InstrumentID: &p.FromInstrumentID, Type: TypeExchangeOut,
			OccurredOn: p.OccurredOn, Quantity: &quantity, AmountMinor: cost,
			Currency: currency, Note: p.Note, Source: p.Source,
			TransferLots: lots,
		}
		inOp := Operation{
			AccountID: p.AccountID, InstrumentID: &p.ToInstrumentID, Type: TypeExchangeIn,
			OccurredOn: p.OccurredOn, Quantity: &toQuantity, AmountMinor: cost,
			Currency: currency, Note: p.Note, Source: p.Source,
			TransferLots: arriving,
		}

		// BOTH LEGS AT ONCE, against the one journal they both land in — see
		// this function's doc. The order inside the slice is the order the
		// engine will fold them in, the departing leg first, which is the order
		// CreatePair then writes them in (clock_timestamp() per row, see
		// insertSQL).
		if err := checkJournalOps(journal, []Operation{outOp, inOp}, nil); err != nil {
			return err
		}

		cOut, cIn, err = st.CreatePair(ctx, spaceID, outOp, inOp, func(storedOut, storedIn Operation) error {
			// The pair as the database actually kept it, folded once more before
			// the commit — the guard every write path here has, and the one that
			// matters most for a pair whose legs replay the pieces the columns
			// rounded rather than the ones checked in memory.
			if _, err := portfolio.Compute(journalWith(journal, []Operation{storedOut, storedIn}, nil)); err != nil {
				return fmt.Errorf("the conversion as stored no longer replays: %v", err)
			}
			return nil
		})
		return err
	})
	if err != nil {
		return Operation{}, Operation{}, mapWriteError(err)
	}
	return cOut, cIn, nil
}

// Delete removes an operation (or, if it belongs to a transfer group, the
// whole group) after confirming every affected account's journal stays
// consistent without it.
//
// AN IMPORTED ROW IS NOT ITS TO DELETE. Rows whose source is not "manual" are
// a projection of records held elsewhere — the broker's own, kept in the
// mirror — and the projection is rebuilt from those records rather than from
// what the journal happens to contain. A row deleted here is therefore written
// again the next time it is rebuilt, without a word, and "deleted" was a lie
// this program told. Refusing says who owns the row instead; removing it for
// real means removing what it is projected from, which goes through the
// importer's own path (see ApplyImportDelta).
//
// IT TAKES THE SAME JOURNAL LOCK THE WRITE PATHS DO, and for the same reason
// (issue #17 names Create; this is the identical shape). "Would this account
// still replay without the row" is a question about the journal at a moment, and
// answering it on the pool let a sell be recorded against the very buy being
// deleted, each request seeing a journal the other had not touched yet and each
// being told it was fine. Reading the affected journals under the lock makes the
// second of the two see the first.
//
// The row and its group are read TWICE: once on the pool, only to learn which
// accounts to lock, and then again inside the lock, where the answers are the
// ones acted on. The first read decides nothing — a group's legs cannot change
// accounts, so it can only name too few accounts if the group itself vanished
// meanwhile, and then the second read finds nothing to delete either.
func (s *Service) Delete(ctx context.Context, spaceID, id uuid.UUID) error {
	accountIDs, err := s.deletionAccounts(ctx, spaceID, id)
	if err != nil {
		return err
	}
	return s.store.WithAccountsLocked(ctx, spaceID, accountIDs, func(st *Store) error {
		op, err := st.ByID(ctx, spaceID, id)
		if err != nil {
			return err
		}
		if op.Source != "manual" {
			return fmt.Errorf("%w: imported operations are managed by the importer", family.ErrValidation)
		}

		accounts := map[uuid.UUID]bool{op.AccountID: true}
		removeIDs := map[uuid.UUID]bool{op.ID: true}
		if op.TransferGroupID != nil {
			// A transfer group's two legs live on two different accounts; both
			// must be re-validated, and both must be excluded from either
			// account's replayed journal.
			group, err := st.ByTransferGroup(ctx, spaceID, *op.TransferGroupID)
			if err != nil {
				return err
			}
			for _, o := range group {
				accounts[o.AccountID] = true
				removeIDs[o.ID] = true
			}
		}

		for accountID := range accounts {
			if err := checkJournal(ctx, st, spaceID, accountID, nil, removeIDs); err != nil {
				return err
			}
		}

		_, err = st.Delete(ctx, spaceID, id)
		return err
	})
}

// deletionAccounts names every account Delete has to lock: the row's own, and —
// when the row is one leg of a transfer — the other leg's as well. It is a read
// with no decision in it, which is what lets it run outside the lock: locking
// requires knowing which accounts to lock, and that cannot itself be learned
// under the lock it is choosing.
//
// A row that is already gone yields no accounts and no error. The refusal for
// that belongs to Delete's own read inside the lock, so that "no such operation"
// is answered once, by the read whose answer is acted on.
func (s *Service) deletionAccounts(ctx context.Context, spaceID, id uuid.UUID) ([]uuid.UUID, error) {
	op, err := s.store.ByID(ctx, spaceID, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ids := []uuid.UUID{op.AccountID}
	if op.TransferGroupID == nil {
		return ids, nil
	}
	group, err := s.store.ByTransferGroup(ctx, spaceID, *op.TransferGroupID)
	if err != nil {
		return nil, err
	}
	for _, o := range group {
		ids = append(ids, o.AccountID)
	}
	return ids, nil
}
