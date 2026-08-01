package operation

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
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

var currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)

// maxAmountMinor caps |amount_minor| and fee_minor at 10^15 minor units
// (≈10 trillion roubles) — far above any real portfolio, yet far enough from
// math.MaxInt64 that summing a whole journal of such values cannot overflow.
// Without the cap, a single amount_minor = math.MinInt64 poisons the FIFO
// cost basis and wraps realized P&L.
const maxAmountMinor int64 = 1_000_000_000_000_000

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
// both operations.quantity and operation_transfer_lots.quantity are
// NUMERIC(30,10) (see the migrations). It is a storage fact, so it lives here
// rather than in the pure engine, which knows nothing about a database.
//
// Nothing bounds a quantity in memory to this scale — a split multiplies lot
// quantities by an arbitrary ratio — so anything on its way into those columns
// must be brought onto the scale deliberately, by code that can decide where
// the rounding goes. Letting Postgres do it silently, piece by piece, is what
// made a legitimate transfer unreadable afterwards (see quantizeLots).
const quantityScale = 10

// maxOccurredOn mirrors the account package's as_of slack: a day of leeway
// past the UTC "today" boundary so a user anywhere from UTC+3 to UTC+12 can
// record "today" in their own local date.
func maxOccurredOn() time.Time {
	return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
}

// validate checks operation fields that are cheap and local — i.e. don't
// require replaying the journal. See the package's task brief for the
// per-type contract; note the engine itself does not check amount sign or
// non-zero-ness for dividend/coupon/tax/fee/interest/deposit/withdrawal, so
// the service must, or silent corruption of income/fees becomes possible.
func validate(o Operation) error {
	if !o.Type.Valid() {
		return fmt.Errorf("%w: invalid operation type", family.ErrValidation)
	}
	if !currencyRe.MatchString(o.Currency) {
		return fmt.Errorf("%w: currency must be ISO-4217 uppercase", family.ErrValidation)
	}
	if o.OccurredOn.After(maxOccurredOn()) {
		return fmt.Errorf("%w: occurred_on must not be in the future", family.ErrValidation)
	}
	if o.FeeMinor < 0 {
		return fmt.Errorf("%w: fee_minor must be >= 0", family.ErrValidation)
	}
	// Bounds are checked with explicit comparisons rather than an abs() so
	// that math.MinInt64 (whose negation overflows) is rejected too.
	if o.AmountMinor > maxAmountMinor || o.AmountMinor < -maxAmountMinor {
		return fmt.Errorf("%w: amount_minor must be within ±%d", family.ErrValidation, maxAmountMinor)
	}
	if o.FeeMinor > maxAmountMinor {
		return fmt.Errorf("%w: fee_minor must be <= %d", family.ErrValidation, maxAmountMinor)
	}

	switch o.Type {
	case TypeBuy, TypeSell:
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
		if o.Type == TypeSell && o.AmountMinor <= 0 {
			return fmt.Errorf("%w: sell amount_minor must be positive", family.ErrValidation)
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
		if o.Source != "" && o.Source != "manual" {
			return fmt.Errorf("%w: split is only supported for source=manual", family.ErrValidation)
		}
	case TypeTransferIn, TypeTransferOut:
		return fmt.Errorf("%w: use the transfer endpoint for %s", family.ErrValidation, o.Type)
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
func (s *Service) checkJournal(ctx context.Context, spaceID, accountID uuid.UUID,
	add []Operation, removeIDs map[uuid.UUID]bool,
) error {
	ops, err := s.store.ListForEngine(ctx, spaceID, accountID)
	if err != nil {
		return err
	}
	return checkJournalOps(ops, add, removeIDs)
}

// checkJournalOps is checkJournal over an already-loaded journal, so a caller
// that has fetched the account's operations for another reason (see
// CreateTransfer) does not pay for a second round trip.
func checkJournalOps(ops []Operation, add []Operation, removeIDs map[uuid.UUID]bool) error {
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
	sort.SliceStable(journal, func(i, j int) bool {
		if !journal[i].OccurredOn.Equal(journal[j].OccurredOn) {
			return journal[i].OccurredOn.Before(journal[j].OccurredOn)
		}
		return journal[i].CreatedAt.Before(journal[j].CreatedAt)
	})
	if _, err := portfolio.Compute(journal); err != nil {
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
// A piece's quantity is a fraction of a lot, and a lot's quantity is not bound
// to ten decimal places: a split multiplies it by a ratio, and a reverse split
// by ratio 0.3333333333 turns two whole shares into 1.16666666655 apiece.
// Storing such pieces as they are means Postgres rounds each one on its own,
// independently, and two pieces rounding up push the stored sum 1e-10 above
// the stored quantity of the transfer itself. The engine checks that sum on
// every read (portfolio.CheckTransferLots), so the transfer is accepted and
// the receiving account's positions screen then fails forever — for data the
// application wrote itself.
//
// The allocation is the one releaseFIFO already uses for costs, applied to
// quantities: truncate the RUNNING TOTAL to the scale and give each piece the
// difference from the previous piece's running total. Every piece is then
// exactly representable, no piece can exceed its exact share by more than the
// last digit, and the final running total is total itself, so the pieces sum
// to the moved quantity exactly rather than approximately. Truncating each
// piece on its own instead would leave a remainder unaccounted for.
//
// A piece whose share is finer than the scale can vanish this way (a dust lot
// of 5e-11 that does not push the running total over the next 1e-10 boundary).
// Such a piece is dropped rather than stored as a zero — the table rejects
// zero quantities, and a zero-quantity lot is not a thing that exists — but
// its COST is real money and is carried onto the next surviving piece, whose
// acquisition date it then shares. Cost is never invented and never lost: the
// pieces still sum to the same basis, which is why CreateTransfer can go on
// summing them for the operation's own amount. The last piece always survives
// (its quantity is total minus everything already placed, and total is
// positive), so there is always somewhere for a carry to land.
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

// Create validates op, checks that appending it keeps the account's journal
// consistent under the portfolio engine, and persists it.
func (s *Service) Create(ctx context.Context, spaceID uuid.UUID, op Operation) (Operation, error) {
	if err := validate(op); err != nil {
		return Operation{}, err
	}
	if err := s.checkJournal(ctx, spaceID, op.AccountID, []Operation{op}, nil); err != nil {
		return Operation{}, err
	}
	created, err := s.store.Create(ctx, spaceID, op)
	if err != nil {
		return Operation{}, mapWriteError(err)
	}
	return created, nil
}

// CreateTransfer moves an in-kind position between two accounts as an
// atomic transfer_out/transfer_in pair sharing the moved cost basis.
func (s *Service) CreateTransfer(ctx context.Context, spaceID uuid.UUID, p TransferParams) (out, in Operation, err error) {
	if p.FromAccountID == p.ToAccountID {
		return Operation{}, Operation{}, fmt.Errorf("%w: from and to accounts must differ", family.ErrValidation)
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
	if p.OccurredOn.After(maxOccurredOn()) {
		return Operation{}, Operation{}, fmt.Errorf("%w: occurred_on must not be in the future", family.ErrValidation)
	}

	sourceJournal, err := s.store.ListForEngine(ctx, spaceID, p.FromAccountID)
	if err != nil {
		return Operation{}, Operation{}, err
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
		return Operation{}, Operation{}, fmt.Errorf("%w: no source history for instrument", family.ErrValidation)
	}

	cost := int64(0)
	var lots []ReleasedLot
	if p.CostMinorOverride != nil {
		// A basis given by hand is not a release of anything: there are no
		// source lots behind it and therefore no acquisition dates to carry.
		// The destination lot keeps the transfer's own date, as before —
		// inventing pieces here would fabricate history.
		cost = *p.CostMinorOverride
		if cost < 0 || cost > maxAmountMinor {
			return Operation{}, Operation{}, fmt.Errorf("%w: cost_minor must be within 0..%d", family.ErrValidation, maxAmountMinor)
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
			return Operation{}, Operation{}, fmt.Errorf("%w: %v", ErrInconsistent, err)
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
		return Operation{}, Operation{}, err
	}
	if err := s.checkJournal(ctx, spaceID, p.ToAccountID, []Operation{inOp}, nil); err != nil {
		return Operation{}, Operation{}, err
	}

	cOut, cIn, err := s.store.CreatePair(ctx, spaceID, outOp, inOp)
	if err != nil {
		return Operation{}, Operation{}, mapWriteError(err)
	}
	return cOut, cIn, nil
}

// Delete removes an operation (or, if it belongs to a transfer group, the
// whole group) after confirming every affected account's journal stays
// consistent without it.
func (s *Service) Delete(ctx context.Context, spaceID, id uuid.UUID) error {
	op, err := s.store.ByID(ctx, spaceID, id)
	if err != nil {
		return err
	}

	accounts := map[uuid.UUID]bool{op.AccountID: true}
	removeIDs := map[uuid.UUID]bool{op.ID: true}
	if op.TransferGroupID != nil {
		// A transfer group's two legs live on two different accounts; both
		// must be re-validated, and both must be excluded from either
		// account's replayed journal.
		group, err := s.store.ByTransferGroup(ctx, spaceID, *op.TransferGroupID)
		if err != nil {
			return err
		}
		for _, o := range group {
			accounts[o.AccountID] = true
			removeIDs[o.ID] = true
		}
	}

	for accountID := range accounts {
		if err := s.checkJournal(ctx, spaceID, accountID, nil, removeIDs); err != nil {
			return err
		}
	}

	if _, err := s.store.Delete(ctx, spaceID, id); err != nil {
		return err
	}
	return nil
}
