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
	if p.CostMinorOverride != nil {
		cost = *p.CostMinorOverride
	} else {
		cost, err = portfolio.ReleasedCost(sourceJournal, p.InstrumentID, p.Quantity)
		if err != nil {
			return Operation{}, Operation{}, fmt.Errorf("%w: %v", ErrInconsistent, err)
		}
	}

	outOp := Operation{
		AccountID: p.FromAccountID, InstrumentID: &p.InstrumentID, Type: TypeTransferOut,
		OccurredOn: p.OccurredOn, Quantity: &p.Quantity, AmountMinor: cost,
		Currency: currency, Note: p.Note,
	}
	inOp := Operation{
		AccountID: p.ToAccountID, InstrumentID: &p.InstrumentID, Type: TypeTransferIn,
		OccurredOn: p.OccurredOn, Quantity: &p.Quantity, AmountMinor: cost,
		Currency: currency, Note: p.Note,
	}

	// The two legs touch different accounts' journals, so each is checked
	// independently: the source loses the position (checked against its own
	// history), the destination gains a fresh lot (always consistent on its
	// own, but checked for uniformity and to catch a same-account edge case
	// earlier logic might have missed).
	if err := s.checkJournal(ctx, spaceID, p.FromAccountID, []Operation{outOp}, nil); err != nil {
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
