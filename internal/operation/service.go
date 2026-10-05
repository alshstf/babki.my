package operation

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

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

// ErrInconsistent means writing or removing an operation would leave the
// account's journal unable to replay through the portfolio engine: an oversell,
// a broken transfer chain.
var ErrInconsistent = errors.New("journal would become inconsistent")

// pgForeignKeyViolation and pgUniqueViolation are the SQLSTATE codes Postgres
// returns for the constraints Service.Create can trip.
const (
	pgForeignKeyViolation = "23503"
	pgUniqueViolation     = "23505"
)

// Every money bound here is money.MaxAmountMinor, shared with account
// balances (#89) because the accounts screen adds the two together.

// minorUnitScale is how many decimal places a major currency unit is split
// into for storage. A price travels in major units, so it is shifted by this
// before it is compared with amount_minor.
const minorUnitScale = 2

// maxQuantity, maxPrice and maxSplitRatio bound the decimal fields that decide
// how much money a row stands for, each by what has to fit rather than by a guess
// at how many shares a person might own.
//
//   - maxQuantity is the money cap read as a count of units at one major unit
//     apiece. The read side multiplies a quantity by a quote and an fx rate into
//     an int64 of minor units; at this bound a quote of up to ~9223 per unit
//     still fits, so the largest accepted quantity is not by itself why an
//     ordinary quote cannot be valued (#84). Read as minor units the same cap
//     would leave 92.24 per unit, below most share prices.
//   - maxPrice is the money cap read as a price per unit. It equals maxQuantity
//     by coincidence; nothing may rest on that, which is why tests compare
//     refusals whole.
//   - maxSplitRatio is split_ratio's own ceiling: NUMERIC(20,10) keeps ten
//     integer digits.
//
// price × quantity is bounded too, as a consistency rule rather than an overflow
// guard: it is what the trade was for, the same money amount_minor carries.
// The factors are bounded separately because price is often absent and the read
// side multiplies by a quote, not by this price.
//
// What this refuses that is real: more than 10^13 units of something worth
// under a rouble apiece (meme tokens, hyperinflated cash). Nothing here values
// those today; maxQuantity is the line to revisit if one appears.
//
// None of this replaces the read-side guards (portfolio.marketValue,
// rateLookup.applyTo, sumInBase, money.Minor): quotes and rates arrive later,
// positions sum many rows, splits multiply whole positions, and rows written
// before the bound are still there.
//
// maxQuantity and maxPrice derive from money.MaxAmountMinor so they move with it;
// maxSplitRatio follows the column.
var (
	maxQuantity = decimal.NewFromInt(money.MaxAmountMinor).Shift(-minorUnitScale)
	maxPrice    = decimal.NewFromInt(money.MaxAmountMinor).Shift(-minorUnitScale)

	// maxSplitRatio is the first ratio the column cannot store, so it is refused
	// on the value itself, unlike the other bounds, which admit their edge.
	maxSplitRatio = decimal.New(1, 10)

	// maxAmountMinorDec lets the product be compared exactly, without an int64
	// conversion that would have to survive the overflow being checked for.
	maxAmountMinorDec = decimal.NewFromInt(money.MaxAmountMinor)
)

// checkQuantityBound refuses a quantity past maxQuantity. An operation's own
// quantity and the quantity a transfer moves both go through it.
func checkQuantityBound(q decimal.Decimal) error {
	if q.Abs().GreaterThan(maxQuantity) {
		return fmt.Errorf("%w: quantity must be within ±%s", family.ErrValidation, maxQuantity)
	}
	return nil
}

// Service validates journal entries and guards journal consistency by
// replaying the account's operations through the portfolio engine.
type Service struct {
	store *Store
	// afterManualWrite is told which accounts a hand entry changed, after the
	// commit. See OnManualWrite.
	afterManualWrite func(ctx context.Context, spaceID uuid.UUID, accountIDs []uuid.UUID)
}

func NewService(store *Store) *Service { return &Service{store: store} }

// OnManualWrite registers what runs after every committed hand entry (create,
// transfer, replacement, delete). The corporate-actions registry hangs here, so a
// purchase dated before a known split gets the split at once rather than at the
// next daily sweep. The import door does not call it: the registry writes through
// that door. Set once during wiring; fn must not fail the request, the entry is
// already committed.
func (s *Service) OnManualWrite(fn func(ctx context.Context, spaceID uuid.UUID, accountIDs []uuid.UUID)) {
	s.afterManualWrite = fn
}

func (s *Service) manualWriteDone(ctx context.Context, spaceID uuid.UUID, accountIDs ...uuid.UUID) {
	if s.afterManualWrite != nil {
		s.afterManualWrite(ctx, spaceID, accountIDs)
	}
}

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

// quantityScale is how many decimal places the journal keeps for a quantity
// (operations.quantity, split_ratio, operation_transfer_lots.quantity). A number
// headed for one of those columns is brought onto the scale here, by code that
// decides where the rounding goes, so the value the consistency check replays is
// the value the row holds (see quantizeLots and normalizeForStorage).
const quantityScale = portfolio.QuantityScale

// minOccurredOn is the oldest date an operation may carry. It is a typo guard
// and nothing more: no law or data source names 1900. A fumbled year (1026 for
// 2026) would otherwise land at the front of the acquisition-date queue and be
// released first by the next sale, with nothing on screen to say a date was
// odd.
var minOccurredOn = dates.EarliestRecordable()

// TradeAmountMinor is what a trade of quantity at price comes to in minor
// units: the product rounded once, half away from zero, negative for a buy and
// positive for a sell. The trade dialog previews the same figure with its own
// arithmetic, and web/src/lib/testdata/trade-amounts.json holds both to one
// table; what is recorded is this (#194).
func TradeAmountMinor(typ Type, quantity, price decimal.Decimal) (int64, error) {
	minor, err := money.Minor(quantity.Mul(price).Shift(minorUnitScale))
	if err != nil {
		return 0, err
	}
	if typ == TypeBuy {
		return -minor, nil
	}
	return minor, nil
}

// maxSettlementLag is how long after its trade an operation may settle.
// Markets settle in days; a year still catches a mistyped year.
const maxSettlementLag = 366 * 24 * time.Hour

// checkSettledOn holds settled_on, when given, to on or after the trade and
// within maxSettlementLag of it (#202).
func checkSettledOn(o Operation) error {
	if o.SettledOn == nil {
		return nil
	}
	if o.SettledOn.Before(o.OccurredOn) {
		return fmt.Errorf("%w: settled_on must not be earlier than occurred_on", family.ErrValidation)
	}
	if o.SettledOn.Sub(o.OccurredOn) > maxSettlementLag {
		return fmt.Errorf("%w: settled_on must be within a year of occurred_on", family.ErrValidation)
	}
	return nil
}

// checkOccurredOn holds a date between minOccurredOn and today. Both write
// paths (an operation and a transfer) use it so they cannot drift apart again.
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

// validate checks the fields of a hand entry that need no journal replay. The
// engine does not check amount sign or non-zeroness for cash-only types, so this
// must, or income and fees can be silently corrupted.
//
// The importer has its own rule (validateImported); the two differ on transfers
// and agree on everything else, which lives in validateFields and
// validateByType.
func validate(o Operation) error {
	if err := validateFields(o); err != nil {
		return err
	}
	if err := checkNote(o.Note); err != nil {
		return err
	}
	if o.Type == TypeTransferIn || o.Type == TypeTransferOut {
		// Hand entry writes a transfer through the endpoint that records both
		// legs at once. The import path admits a lone leg (see validateImported),
		// because a broker reports shares arriving without saying from where.
		return fmt.Errorf("%w: use the transfer endpoint for %s", family.ErrValidation, o.Type)
	}
	if o.Type == TypeSpinoffOut || o.Type == TypeSpinoffIn {
		// Like a conversion, a spin-off happened to the paper and is recorded
		// once in the registry (see Service.CreateSpinoff).
		return fmt.Errorf("%w: a spin-off is recorded in the corporate-actions registry, not entered against an account", family.ErrValidation)
	}
	if o.Type == TypeExchangeOut || o.Type == TypeExchangeIn {
		// A conversion has no hand-entry door at all. What happened to a paper
		// is true for everyone who held it, so it is recorded once in the
		// corporate-actions registry and applied from there (see
		// Service.CreateExchange); a leg typed into one account would be a second
		// copy of the same fact.
		return fmt.Errorf("%w: a conversion is recorded in the corporate-actions registry, not entered against an account", family.ErrValidation)
	}
	return validateByType(o)
}

// MaxNoteRunes is the longest note a hand entry takes, in code points, as
// openapi's maxLength counts them. An importer's notes are the broker's own
// wording and are not held to it: refusing a reported row for its wording would
// lose the row.
const MaxNoteRunes = 1000

// checkNote refuses a hand-entered note longer than MaxNoteRunes.
func checkNote(note string) error {
	if utf8.RuneCountInString(note) > MaxNoteRunes {
		return fmt.Errorf("%w: note must be at most %d characters", family.ErrValidation, MaxNoteRunes)
	}
	return nil
}

// validateFields checks each field's own value (shape, bounds, the two money
// fields agreeing) regardless of type. Both write paths run it.
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
	if err := checkSettledOn(o); err != nil {
		return err
	}
	if o.FeeMinor < 0 {
		return fmt.Errorf("%w: fee_minor must be >= 0", family.ErrValidation)
	}
	// Explicit comparisons rather than abs(), so math.MinInt64 is refused too.
	if o.AmountMinor > money.MaxAmountMinor || o.AmountMinor < -money.MaxAmountMinor {
		return fmt.Errorf("%w: amount_minor must be within ±%d", family.ErrValidation, money.MaxAmountMinor)
	}
	if o.FeeMinor > money.MaxAmountMinor {
		return fmt.Errorf("%w: fee_minor must be <= %d", family.ErrValidation, money.MaxAmountMinor)
	}
	// Checked for every type, conversions included: any type may carry these
	// columns, and the product rule is about the row's two money fields agreeing,
	// not about the engine. Magnitudes here; signs are the per-type rules' business.
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
	// A ratio this size can still carry a position, built one buy at a time,
	// past what the read side can value: applySplit multiplies the whole holding.
	// That is why the read-side guards stay.
	if o.SplitRatio != nil && o.SplitRatio.Abs().GreaterThanOrEqual(maxSplitRatio) {
		return fmt.Errorf("%w: split_ratio must be less than %s", family.ErrValidation, maxSplitRatio)
	}
	return nil
}

// validateByType is the per-type contract both write paths share. It has no
// branch for transfer_in and transfer_out on purpose: each path rules on those
// itself before delegating, and a refusal here would hide the deletion of one of
// those rulings. A caller must not hand it a type it has not ruled on.
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
		// A split is written by the corporate-actions registry only. No broker
		// reports one, and a person typing it into one account would leave every
		// other holder of the paper wrong; the registry records it once against
		// the ISIN and applies it everywhere. The hand path refuses "manual" here;
		// the import path refuses every other source before delegating.
		if o.Source != SourceRegistry {
			return fmt.Errorf(
				"%w: a split is recorded once for the security in the corporate-actions registry, not entered on an account",
				family.ErrValidation)
		}
	case TypeExchangeOut, TypeExchangeIn:
		// Reachable only from validateImported: the hand path refuses these
		// types earlier and CreateExchange builds its own legs. Kept so that an
		// importer projecting a broker's conversion is refused by a rule.
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
	case TypeSpinoffOut:
		// The departing leg moves money out of the parcels and leaves every
		// unit in place (see portfolio.TypeSpinoffOut). A count would be drawn as
		// units leaving, so it is refused rather than ignored.
		if o.InstrumentID == nil {
			return fmt.Errorf("%w: %s requires an instrument", family.ErrValidation, o.Type)
		}
		if o.Quantity != nil {
			return fmt.Errorf("%w: a spin-off moves no units, so %s carries no quantity", family.ErrValidation, o.Type)
		}
		if o.AmountMinor < 0 {
			return fmt.Errorf("%w: %s amount_minor (cost basis) must be >= 0", family.ErrValidation, o.Type)
		}
		if o.Source != SourceRegistry {
			return fmt.Errorf("%w: %s is only supported for source=%s", family.ErrValidation, o.Type, SourceRegistry)
		}
	case TypeSpinoffIn:
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
		// cash-level: buying and selling currency are both legitimate.
	}
	// dividend, coupon, tax and fee may be recorded without an instrument.
	return nil
}

// checkJournal replays the account's journal, minus removeIDs and plus add,
// through the portfolio engine. It takes the store because every caller runs it
// inside Store.WithAccountsLocked and must read through that transaction.
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
// removeIDs gone, in fold order.
func journalWith(ops []Operation, add []Operation, removeIDs map[uuid.UUID]bool) []Operation {
	journal := make([]Operation, 0, len(ops)+len(add))
	for _, o := range ops {
		if !removeIDs[o.ID] {
			journal = append(journal, o)
		}
	}
	// A row being added sorts after every stored row of its day. Stored rows
	// carry the database clock (insertSQL's clock_timestamp()), which may run
	// ahead of this process; using only the local clock once put a same-day
	// transfer in front of the purchase it moved.
	at := time.Now()
	for _, o := range journal {
		if !o.CreatedAt.Before(at) {
			at = o.CreatedAt.Add(time.Microsecond)
		}
	}
	for _, o := range add {
		o.CreatedAt = at
		at = at.Add(time.Microsecond)
		journal = append(journal, o)
	}
	sortJournal(journal)
	return journal
}

// sortJournal puts operations in the order the engine folds them, the order
// ListForEngine reads them back in (see foldsBefore). The write check and the
// import both sort through here, so the check and the later read cannot disagree
// about order.
func sortJournal(journal []Operation) {
	sort.SliceStable(journal, func(i, j int) bool { return foldsBefore(journal[i], journal[j]) })
}

// SortJournal puts an in-memory journal into fold order, for a caller that
// adds rows to a journal it read (the corporate-actions registry) and must hand
// the engine what a read of the stored rows would.
func SortJournal(journal []Operation) { sortJournal(journal) }

// FoldedBefore returns the part of journal that folds before a new row dated
// day, written by source and carrying no instant: everything earlier, plus the
// day's rows whose source ranks no later (see foldRank). A row's parcels must be
// worked out against exactly this, or they describe a holding the replay will not
// find.
func FoldedBefore(journal []Operation, day time.Time, source string) []Operation {
	rank := foldRank(source)
	out := make([]Operation, 0, len(journal))
	for _, o := range journal {
		if o.OccurredOn.Before(day) || (o.OccurredOn.Equal(day) && foldRank(o.Source) <= rank) {
			out = append(out, o)
		}
	}
	return out
}

// checkJournalOps is checkJournal over a journal the caller already holds.
func checkJournalOps(ops []Operation, add []Operation, removeIDs map[uuid.UUID]bool) error {
	if _, err := portfolio.Compute(journalWith(ops, add, removeIDs)); err != nil {
		return fmt.Errorf("%w: %v", ErrInconsistent, err)
	}
	return nil
}

// journalUpTo returns the rows of ops on or before day: the journal a
// hand-entered transfer dated day replays against. Same-day rows are kept, since
// a hand row has no instant and folds last on its date.
func journalUpTo(ops []Operation, day time.Time) []Operation {
	out := make([]Operation, 0, len(ops))
	for _, o := range ops {
		if !o.OccurredOn.After(day) {
			out = append(out, o)
		}
	}
	return out
}

// foldedAhead returns the rows of ops that fold before op, which for an
// imported row with an instant can be the middle of its day.
func foldedAhead(ops []Operation, op Operation) []Operation {
	out := make([]Operation, 0, len(ops))
	for _, o := range ops {
		if foldsBefore(o, op) {
			out = append(out, o)
		}
	}
	return out
}

// quantizeLots brings every piece of a breakdown onto the journal's quantity
// scale so that what is written is what is read back; total is the row's own
// quantity, already on that scale. The running total is truncated and each piece
// gets the difference, so the pieces sum to total exactly, as
// portfolio.CheckTransferLots demands on every read.
//
// A piece left with no units keeps its money and its day: its cost is real and
// belongs to the date it was spent (a reverse split rounded its shares away, or a
// share of new paper is finer than the scale). Only a piece with neither units
// nor money is dropped.
func quantizeLots(pieces []portfolio.ReleasedLot, total decimal.Decimal) []portfolio.ReleasedLot {
	// The remainder goes to the last piece that started with units, so a
	// shareless tail stays shareless.
	last := -1
	for i, pc := range pieces {
		if pc.Quantity.IsPositive() {
			last = i
		}
	}
	out := make([]portfolio.ReleasedLot, 0, len(pieces))
	exact := decimal.Zero  // running total of the pieces as computed
	placed := decimal.Zero // running total of the pieces as they will be stored
	for i, pc := range pieces {
		exact = exact.Add(pc.Quantity)
		upTo := exact.Truncate(quantityScale)
		if i == last {
			upTo = total
		}
		qty := upTo.Sub(placed)
		placed = upTo
		if qty.IsZero() && pc.CostMinor == 0 {
			continue
		}
		out = append(out, portfolio.ReleasedLot{Quantity: qty, CostMinor: pc.CostMinor, AcquiredOn: pc.AcquiredOn, RateOn: pc.RateOn})
	}
	return out
}

// rescaleLots restates a FIFO breakdown in another paper's units: pieces that
// gave up from units come back describing to units, each keeping its cost and
// acquisition date. Only quantities move in a conversion (see
// portfolio.TypeExchangeOut); scaling the cost would invent a loss.
//
// Each piece is scaled at full precision and quantizeLots does the allocation, so
// the pieces sum to exactly to and CheckTransferLots accepts the row on every
// read.
func rescaleLots(pieces []ReleasedLot, from, to decimal.Decimal) []ReleasedLot {
	scaled := make([]ReleasedLot, 0, len(pieces))
	for _, pc := range pieces {
		scaled = append(scaled, ReleasedLot{
			Quantity:   pc.Quantity.Mul(to).Div(from),
			CostMinor:  pc.CostMinor,
			AcquiredOn: pc.AcquiredOn,
			RateOn:     pc.RateOn,
		})
	}
	return quantizeLots(scaled, to)
}

// mapWriteError turns constraint violations from Store.Create into domain
// errors.
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

// normalizeForStorage truncates quantity and split_ratio to the stored scale
// before anything is validated, so the operation the consistency check folds is
// the row the database will hold. Postgres would otherwise round to nearest: a
// sell of a whole 0.116666666655 position was once checked at that figure, stored
// as 0.1166666667, and refused as an oversell on every later read.
//
// Down, as CreateTransfer does: rounding "sell everything" up would turn a good
// request into an oversell. A truncated split_ratio can still make a later release
// too large and be refused, which is loud and fixable by re-entering the row.
// Price is left alone: it is never replayed or compared.
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
// consistent, and persists it.
//
// The read, the check and the write run in one transaction holding the account's
// journal lock (see Store.WithAccountsLocked), so two concurrent sells of one
// holding cannot both be accepted (#17). The journal is folded twice: once to
// decide, once more with the row as stored, before the commit, so a stored row
// that differs from the checked one rolls the write back instead of breaking
// later reads.
func (s *Service) Create(ctx context.Context, spaceID uuid.UUID, op Operation) (Operation, error) {
	return s.CreateReplacing(ctx, spaceID, op, nil)
}

// CreateReplacing writes op in place of the imported rows named by replace,
// checking the journal that results in one transaction. With an empty replace it
// is Create.
//
// It exists because the broker sends no corporate actions: a real event arrives
// as whatever rows carried its money, some misread. A fund's partial redemption
// reached the owner's account as a transfer_out to another depositary; the
// owner's own redemption of those units was refused while that transfer_out still
// held them. Removing and then writing would leave a window with neither reading
// of the event.
//
// Refused: replacing a hand-entered row (the owner deletes those on the journal
// screen), half of a transfer group, and ids outside the space.
func (s *Service) CreateReplacing(ctx context.Context, spaceID uuid.UUID, op Operation, replace []uuid.UUID) (
	Operation, error,
) {
	if err := normalizeForStorage(&op); err != nil {
		return Operation{}, err
	}
	if err := validate(op); err != nil {
		return Operation{}, err
	}
	// The accounts to lock cannot be learned under the lock, so the
	// replaced rows are read on the pool for their accounts only and read again
	// inside, as Delete does. A row that vanishes meanwhile is refused by the
	// second read.
	accountIDs := []uuid.UUID{op.AccountID}
	if len(replace) > 0 {
		rows, err := s.store.ByIDs(ctx, spaceID, replace)
		if err != nil {
			return Operation{}, err
		}
		for _, o := range rows {
			accountIDs = append(accountIDs, o.AccountID)
		}
	}

	var created Operation
	err := s.store.WithOpenAccountsLocked(ctx, spaceID, accountIDs, func(st *Store) error {
		removeIDs, accounts, err := replacedRows(ctx, st, spaceID, replace)
		if err != nil {
			return err
		}
		accounts[op.AccountID] = true

		// The engine answers about one journal at a time, so each touched
		// account is asked separately; an account that only loses rows must still
		// replay.
		journals := make(map[uuid.UUID][]Operation, len(accounts))
		for accountID := range accounts {
			journal, err := st.ListForEngine(ctx, spaceID, accountID)
			if err != nil {
				return err
			}
			journals[accountID] = journal
			var add []Operation
			if accountID == op.AccountID {
				add = []Operation{op}
			}
			if err := checkJournalOps(journal, add, removeIDs); err != nil {
				return err
			}
		}

		stored, err := st.ApplyDelta(ctx, spaceID, []Operation{op}, replace, func(stored []Operation) error {
			// Not wrapped in ErrInconsistent: the request was accepted a moment ago,
			// so a failure here is this program's bug (a stored row differing from
			// the checked one), not the caller's history.
			for accountID := range accounts {
				var add []Operation
				if accountID == op.AccountID {
					add = stored
				}
				if _, err := portfolio.Compute(journalWith(journals[accountID], add, removeIDs)); err != nil {
					return fmt.Errorf("the operation as stored no longer replays on account %s: %v", accountID, err)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		created = stored[0]
		return nil
	})
	if err != nil {
		return Operation{}, mapWriteError(err)
	}
	s.manualWriteDone(ctx, spaceID, accountIDs...)
	return created, nil
}

// replacedRows reads the rows a replacement takes out and their accounts,
// refusing what must not be replaced. It runs inside the lock.
func replacedRows(ctx context.Context, st *Store, spaceID uuid.UUID, replace []uuid.UUID) (
	removeIDs map[uuid.UUID]bool, accounts map[uuid.UUID]bool, err error,
) {
	removeIDs = make(map[uuid.UUID]bool, len(replace))
	accounts = map[uuid.UUID]bool{}
	if len(replace) == 0 {
		return removeIDs, accounts, nil
	}
	rows, err := st.ByIDs(ctx, spaceID, replace)
	if err != nil {
		return nil, nil, err
	}
	if len(rows) != len(replace) {
		// An id of another space, an id named twice, or a row already gone: the
		// caller computed against another journal, so nothing is written.
		return nil, nil, fmt.Errorf("%w: asked to replace %d operations, found %d in this space",
			family.ErrValidation, len(replace), len(rows))
	}
	groups := map[uuid.UUID]bool{}
	for _, o := range rows {
		if o.Source == "manual" {
			return nil, nil, fmt.Errorf("%w: operation %s was entered by hand and is not an importer's to replace",
				family.ErrValidation, o.ID)
		}
		removeIDs[o.ID] = true
		accounts[o.AccountID] = true
		if o.TransferGroupID != nil {
			groups[*o.TransferGroupID] = true
		}
	}
	for group := range groups {
		siblings, err := st.ByTransferGroup(ctx, spaceID, group)
		if err != nil {
			return nil, nil, err
		}
		for _, o := range siblings {
			if !removeIDs[o.ID] {
				return nil, nil, fmt.Errorf("%w: replacing transfer group %s would leave its %s leg behind",
					family.ErrValidation, group, o.Type)
			}
		}
	}
	return removeIDs, accounts, nil
}

// CreateTransfer moves an in-kind position between two accounts as an atomic
// transfer_out/transfer_in pair sharing the moved cost basis. Everything runs in
// one transaction holding both accounts' journal locks: the source's FIFO release
// is resolved as of the transfer date, and a sell landing meanwhile would leave
// the pair naming lots that are gone.
func (s *Service) CreateTransfer(ctx context.Context, spaceID uuid.UUID, p TransferParams) (out, in Operation, err error) {
	if p.FromAccountID == p.ToAccountID {
		return Operation{}, Operation{}, fmt.Errorf("%w: from and to accounts must differ", family.ErrValidation)
	}
	// Refused here, before the source journal is searched; otherwise the
	// nil UUID finds no history and the error names the wrong mistake (#19).
	if p.InstrumentID == uuid.Nil {
		return Operation{}, Operation{}, fmt.Errorf("%w: instrument_id is required", family.ErrValidation)
	}
	if !p.Quantity.IsPositive() {
		return Operation{}, Operation{}, fmt.Errorf("%w: quantity must be positive", family.ErrValidation)
	}
	// Truncated to the stored scale up front so the breakdown sums to what
	// the row stores (see quantizeLots). Down, not nearest: rounding up could
	// turn "move everything" into an oversell.
	quantity := p.Quantity.Truncate(quantityScale)
	if !quantity.IsPositive() {
		return Operation{}, Operation{}, fmt.Errorf("%w: quantity is finer than the %d decimal places the journal records",
			family.ErrValidation, quantityScale)
	}
	// The bound an operation's own quantity gets (see maxQuantity): a
	// position grows past it one buy at a time and then moves in one
	// transfer.
	if err := checkQuantityBound(quantity); err != nil {
		return Operation{}, Operation{}, err
	}
	if err := checkOccurredOn(p.OccurredOn); err != nil {
		return Operation{}, Operation{}, err
	}
	if err := checkNote(p.Note); err != nil {
		return Operation{}, Operation{}, err
	}

	var cOut, cIn Operation
	err = s.store.WithOpenAccountsLocked(ctx, spaceID, []uuid.UUID{p.FromAccountID, p.ToAccountID}, func(st *Store) error {
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
			// A basis given by hand releases nothing, so there are no acquisition
			// dates to carry and the arriving lot gets none: inventing a date would
			// fabricate history (see portfolio.Lot.AcquiredOn).
			//
			// The departing leg carries the same number, though the engine gives up a
			// fresh FIFO slice of the source and discards its cost. The two are not
			// reconciled on purpose; the API documents it on
			// TransferRequest.cost_minor and Operation.amount_minor (#17).
			cost = *p.CostMinorOverride
			if cost < 0 || cost > money.MaxAmountMinor {
				return fmt.Errorf("%w: cost_minor must be within 0..%d", family.ErrValidation, money.MaxAmountMinor)
			}
		} else {
			// Released from the journal as it stood on the transfer date, where the
			// engine will replay it; the end state would carry the basis of lots
			// bought later. The pieces are kept, not just their total, because the
			// destination values each at its own day's rate, and the basis is their
			// sum.
			lots, err = portfolio.ReleasedLots(journalUpTo(sourceJournal, p.OccurredOn), p.InstrumentID, quantity)
			if err != nil {
				return fmt.Errorf("%w: %v", ErrInconsistent, err)
			}
			// Quantized before the basis is summed, so the amount is the sum of the
			// pieces actually written.
			lots = quantizeLots(lots, quantity)
			cost = portfolio.LotsCost(lots)
		}

		outOp := Operation{
			AccountID: p.FromAccountID, InstrumentID: &p.InstrumentID, Type: TypeTransferOut,
			OccurredOn: p.OccurredOn, Quantity: &quantity, AmountMinor: cost,
			Currency: currency, Note: p.Note,
			// The departing leg carries the breakdown too: the engine releases the
			// lots it names (see portfolio.Position.releaseRecorded), so without it
			// the candidate is not the row being checked. Only the arriving leg stores
			// it; Store.attachTransferLots hands it to both on read.
			TransferLots: lots,
		}
		inOp := Operation{
			AccountID: p.ToAccountID, InstrumentID: &p.InstrumentID, Type: TypeTransferIn,
			OccurredOn: p.OccurredOn, Quantity: &quantity, AmountMinor: cost,
			Currency: currency, Note: p.Note,
			// The breakdown is stored with the arriving leg, whose account would
			// otherwise lose the acquisition dates.
			TransferLots: lots,
		}

		// Each leg is checked against its own account's journal.
		if err := checkJournalOps(sourceJournal, []Operation{outOp}, nil); err != nil {
			return err
		}
		if err := checkJournal(ctx, st, spaceID, p.ToAccountID, []Operation{inOp}, nil); err != nil {
			return err
		}

		// The pair once more as stored, before the commit: the departing leg
		// replays the stored pieces, so a rounding difference would otherwise be
		// accepted now and refused on every later read. Not wrapped in
		// ErrInconsistent; reaching this is this program's bug.
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
	s.manualWriteDone(ctx, spaceID, p.FromAccountID, p.ToAccountID)
	return cOut, cIn, nil
}

// SpinoffParams describes a spin-off on one account: part of what was paid for
// FromInstrumentID moves onto ToInstrumentID, which appears with RatioTo units per
// RatioFrom of the original.
//
// The ratio is given, not the count: the arriving count depends on the holding,
// which is folded inside the lock, and a caller's count would be a second
// computation against a journal that may have moved.
//
// BasisShare is the fraction of the cost that moves: 0 by default, as the broker
// keeps it (decision Р-16), or what the holder's tax accounting states. It comes
// from the registry.
type SpinoffParams struct {
	AccountID        uuid.UUID
	FromInstrumentID uuid.UUID
	ToInstrumentID   uuid.UUID
	RatioFrom        decimal.Decimal
	RatioTo          decimal.Decimal
	BasisShare       decimal.Decimal
	OccurredOn       time.Time
	Source           string
	Note             string
}

// CreateSpinoff records a spin-off as an atomic spinoff_out/spinoff_in pair on
// one account: the original keeps every unit and gives up part of its money, and
// the new paper is built from those same parcels with their costs and days (see
// portfolio.TypeSpinoffOut). Only the registry calls it. The shape is
// CreateExchange's, except that nothing leaves and the arriving count is derived
// from the holding.
func (s *Service) CreateSpinoff(ctx context.Context, spaceID uuid.UUID, p SpinoffParams) (out, in Operation, err error) {
	var cOut, cIn Operation
	err = s.store.WithAccountsLocked(ctx, spaceID, []uuid.UUID{p.AccountID}, func(st *Store) error {
		journal, err := st.ListForEngine(ctx, spaceID, p.AccountID)
		if err != nil {
			return err
		}
		// Every figure comes from BuildSpinoff, which the registry's
		// materializer also uses.
		outOp, inOp, err := BuildSpinoff(journal, p)
		if err != nil {
			return err
		}

		if err := checkJournalOps(journal, []Operation{outOp, inOp}, nil); err != nil {
			return err
		}

		cOut, cIn, err = st.CreatePair(ctx, spaceID, outOp, inOp, func(storedOut, storedIn Operation) error {
			if _, err := portfolio.Compute(journalWith(journal, []Operation{storedOut, storedIn}, nil)); err != nil {
				return fmt.Errorf("the spin-off as stored no longer replays: %v", err)
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

// ExchangeParams describes a conversion on one account: Quantity units of
// FromInstrumentID become ToQuantity units of ToInstrumentID on OccurredOn. Both
// counts are given because the registry records "N old for M new" as two whole
// numbers.
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

// CreateExchange records a conversion as an atomic exchange_out/exchange_in pair
// on one account: the old paper gives up the lots in the breakdown, and the new
// paper is built from them with costs and dates intact and only quantities
// restated (see portfolio.TypeExchangeOut).
//
// Only the registry calls it, and Source is checked here because this path does
// not go through validate. Both legs land on one account, so one lock, one
// journal, and the two candidates are checked together: one at a time would fold
// a state the account is never in.
func (s *Service) CreateExchange(ctx context.Context, spaceID uuid.UUID, p ExchangeParams) (out, in Operation, err error) {
	var cOut, cIn Operation
	err = s.store.WithAccountsLocked(ctx, spaceID, []uuid.UUID{p.AccountID}, func(st *Store) error {
		journal, err := st.ListForEngine(ctx, spaceID, p.AccountID)
		if err != nil {
			return err
		}
		// BuildExchange holds the whole arithmetic, shared with the registry's
		// materializer.
		outOp, inOp, err := BuildExchange(journal, p)
		if err != nil {
			return err
		}

		// Both legs together, departing first: the order the engine folds them
		// and CreatePair writes them.
		if err := checkJournalOps(journal, []Operation{outOp, inOp}, nil); err != nil {
			return err
		}

		cOut, cIn, err = st.CreatePair(ctx, spaceID, outOp, inOp, func(storedOut, storedIn Operation) error {
			// The pair as stored, folded once more before the commit.
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

// Delete removes an operation (or its whole transfer group) after confirming
// every affected account's journal stays consistent without it.
//
// An imported row is refused: it is a projection of the broker's records and
// would be written again at the next rebuild. Removing it goes through the
// importer (see ApplyImportDelta).
//
// It takes the same journal lock as the write paths, so a sell cannot be
// recorded against a buy being deleted (#17). The row is read on the pool only to
// learn which accounts to lock, and again inside the lock, where the answer is
// acted on.
func (s *Service) Delete(ctx context.Context, spaceID, id uuid.UUID) error {
	accountIDs, err := s.deletionAccounts(ctx, spaceID, id)
	if err != nil {
		return err
	}
	err = s.store.WithOpenAccountsLocked(ctx, spaceID, accountIDs, func(st *Store) error {
		op, err := st.ByID(ctx, spaceID, id)
		if err != nil {
			return err
		}
		if !OwnedByHand(op.Source) {
			return fmt.Errorf("%w: imported operations are managed by the importer", family.ErrValidation)
		}

		accounts := map[uuid.UUID]bool{op.AccountID: true}
		removeIDs := map[uuid.UUID]bool{op.ID: true}
		if op.TransferGroupID != nil {
			// Both legs' accounts are re-validated, each without either leg.
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
	if err != nil {
		return err
	}
	s.manualWriteDone(ctx, spaceID, accountIDs...)
	return nil
}

// deletionAccounts names every account Delete must lock: the row's own and,
// for a transfer leg, the other leg's. It decides nothing, so it runs outside the
// lock. A row already gone yields no accounts and no error; Delete's own read
// answers that.
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
