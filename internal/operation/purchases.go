package operation

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/money"
	"babki.my/babki/internal/portfolio"
)

// StatePurchases records what the shares of an arrival from another broker
// cost and when they were bought, and returns the arrival as stored.
//
// Such an arrival — a transfer_in with no sibling — comes with no purchases
// behind it: the broker that sent the shares does not pass them on. Until they
// are stated the shares count as bought for nothing (see
// portfolio.UnknownCost), which overstates every profit on them by what was
// really paid. The pieces stated here become the arrival's breakdown, exactly
// as a transfer between the owner's own accounts carries the purchases of the
// account it left, and its basis becomes their sum.
//
// A transfer between two of the owner's accounts is refused: its purchases are
// the source account's, and the place to state them is wherever the shares
// first arrived.
//
// Stating them again replaces what was stated before. Shares of the arrival
// may since have moved on to the owner's other accounts, each move carrying a
// breakdown released from the old basis; every such later move made by hand is
// released again from the restated history, in the order the moves happened,
// and written with the statement in one transaction (issue #227). A move whose
// basis was given by hand carries no breakdown and stays as it is; a move an
// importer recorded is the importer's, and when the restated history can no
// longer take it the statement is refused with the engine's reason.
func (s *Service) StatePurchases(ctx context.Context, spaceID, operationID uuid.UUID, stated []StatedPurchase) (Operation, error) {
	pieces, err := piecesOf(stated)
	if err != nil {
		return Operation{}, err
	}
	// Which accounts to lock cannot be learned under the lock, so the row and
	// the accounts its shares went on to are read once on the pool and read
	// again inside; an account that turns up only the second time means the
	// shares moved meanwhile, and the request is refused rather than half done.
	first, err := s.store.ByID(ctx, spaceID, operationID)
	if err != nil {
		return Operation{}, err
	}
	locked := []uuid.UUID{first.AccountID}
	if first.Type == TypeTransferIn {
		onward, err := onwardJournals(ctx, s.store, spaceID, first)
		if err != nil {
			return Operation{}, err
		}
		locked = slices.Collect(maps.Keys(onward))
	}
	var stored Operation
	err = s.store.WithOpenAccountsLocked(ctx, spaceID, locked, func(st *Store) error {
		op, err := st.ByID(ctx, spaceID, operationID)
		if err != nil {
			return err
		}
		if op.Type != TypeTransferIn || op.TransferGroupID != nil {
			return fmt.Errorf("%w: purchases can be stated only for shares that arrived from another broker; "+
				"a transfer between your own accounts carries the purchases of the account it left", family.ErrValidation)
		}
		cost, err := checkStatedPurchases(op, pieces)
		if err != nil {
			return err
		}
		restated := op
		restated.AmountMinor, restated.TransferLots = cost, pieces

		journals, err := onwardJournals(ctx, st, spaceID, op)
		if err != nil {
			return err
		}
		for accountID := range journals {
			if !slices.Contains(locked, accountID) {
				return fmt.Errorf("%w: the shares were moved to another account meanwhile; try again", ErrInconsistent)
			}
		}
		journals[op.AccountID] = journalReplacing(journals[op.AccountID], restated)
		moves, err := releaseOnward(journals, restated)
		if err != nil {
			return err
		}
		for _, journal := range journals {
			if _, err := portfolio.Compute(journal); err != nil {
				return fmt.Errorf("%w: %v", ErrInconsistent, err)
			}
		}

		if stored, err = st.setPurchases(ctx, spaceID, op, cost, pieces); err != nil {
			return err
		}
		for _, m := range moves {
			if err := st.setMoveBreakdown(ctx, spaceID, m.out, m.in); err != nil {
				return err
			}
		}
		// Every account touched, read back as stored and folded once more
		// before the commit: the pieces' quantities come back from a column
		// with a scale (see writeTransferLots).
		for accountID := range journals {
			journal, err := st.ListForEngine(ctx, spaceID, accountID)
			if err != nil {
				return err
			}
			if _, err := portfolio.Compute(journal); err != nil {
				return fmt.Errorf("the purchases as stored no longer replay on account %s: %v", accountID, err)
			}
		}
		return nil
	})
	if err != nil {
		return Operation{}, mapWriteError(err)
	}
	return stored, nil
}

// onwardJournals reads the journal of the arrival's account and of every
// account its shares went on to after it: each later transfer of the paper out
// of an account already read is followed to the account it went into, until
// none is left. The journals are keyed by account.
func onwardJournals(ctx context.Context, st *Store, spaceID uuid.UUID, arrival Operation) (map[uuid.UUID][]Operation, error) {
	journals := make(map[uuid.UUID][]Operation)
	queue := []uuid.UUID{arrival.AccountID}
	for len(queue) > 0 {
		accountID := queue[0]
		queue = queue[1:]
		if _, read := journals[accountID]; read {
			continue
		}
		journal, err := st.ListForEngine(ctx, spaceID, accountID)
		if err != nil {
			return nil, err
		}
		journals[accountID] = journal
		for _, o := range journal {
			if !movesOnward(o, arrival) {
				continue
			}
			legs, err := st.ByTransferGroup(ctx, spaceID, *o.TransferGroupID)
			if err != nil {
				return nil, err
			}
			for _, leg := range legs {
				if leg.Type == TypeTransferIn {
					queue = append(queue, leg.AccountID)
				}
			}
		}
	}
	return journals, nil
}

// movesOnward reports whether o moves shares of the arrival's paper from one of
// the owner's accounts to another and folds after the arrival: only such a
// move can carry the arrival's shares, wherever they have got to by then.
func movesOnward(o, arrival Operation) bool {
	return o.Type == TypeTransferOut && o.TransferGroupID != nil &&
		o.InstrumentID != nil && arrival.InstrumentID != nil && *o.InstrumentID == *arrival.InstrumentID &&
		foldsAfter(o, arrival)
}

// foldsAfter reports whether a folds after b in the engine's order (see
// foldsBefore), whichever accounts the two belong to.
func foldsAfter(a, b Operation) bool { return foldsBefore(b, a) }

// move is one transfer between the owner's accounts, both legs.
type move struct{ out, in Operation }

// releaseOnward releases again, from the restated history, every later move of
// the arrival's paper made by hand with a breakdown — in the order the moves
// happened, so that a move out of an account the shares reached by an earlier
// move is released from that account as the earlier one left it — exactly as
// CreateTransfer released it when it was made: the FIFO front of the source
// account's position at the move's place in its journal. journals is updated
// in place; the moves whose breakdown changed are returned.
//
// A move whose basis was given by hand has no breakdown and nothing to
// release, and a move an importer wrote is the importer's own record; both are
// left as they are, for the caller's fold to accept or refuse.
func releaseOnward(journals map[uuid.UUID][]Operation, arrival Operation) ([]move, error) {
	var outs []Operation
	for _, journal := range journals {
		for _, o := range journal {
			if movesOnward(o, arrival) {
				outs = append(outs, o)
			}
		}
	}
	sortJournal(outs)

	var changed []move
	for _, out := range outs {
		in, ok := arrivingLeg(journals, *out.TransferGroupID)
		if !ok {
			return nil, fmt.Errorf("transfer %s: its arriving leg was not read", *out.TransferGroupID)
		}
		if out.Source != SourceManual || in.Source != SourceManual || len(out.TransferLots) == 0 || out.Quantity == nil {
			continue
		}
		source := journals[out.AccountID]
		at := slices.IndexFunc(source, func(o Operation) bool { return o.ID == out.ID })
		if at < 0 {
			return nil, fmt.Errorf("transfer %s: its departing leg is not in its account's journal", *out.TransferGroupID)
		}
		lots, err := portfolio.ReleasedLots(source[:at], *out.InstrumentID, *out.Quantity)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInconsistent, err)
		}
		lots = quantizeLots(lots, *out.Quantity)
		if sameLots(lots, out.TransferLots) {
			continue
		}
		cost := portfolio.LotsCost(lots)
		out.AmountMinor, out.TransferLots = cost, lots
		in.AmountMinor, in.TransferLots = cost, lots
		journals[out.AccountID] = journalReplacing(journals[out.AccountID], out)
		journals[in.AccountID] = journalReplacing(journals[in.AccountID], in)
		changed = append(changed, move{out: out, in: in})
	}
	return changed, nil
}

// arrivingLeg finds the transfer_in of a transfer group among journals.
func arrivingLeg(journals map[uuid.UUID][]Operation, group uuid.UUID) (Operation, bool) {
	for _, journal := range journals {
		for _, o := range journal {
			if o.Type == TypeTransferIn && o.TransferGroupID != nil && *o.TransferGroupID == group {
				return o, true
			}
		}
	}
	return Operation{}, false
}

// sameLots reports whether two breakdowns name the same pieces in the same order.
func sameLots(a, b []ReleasedLot) bool {
	return slices.EqualFunc(a, b, func(x, y ReleasedLot) bool {
		sameDay := (x.AcquiredOn == nil) == (y.AcquiredOn == nil) &&
			(x.AcquiredOn == nil || x.AcquiredOn.Equal(*y.AcquiredOn))
		return x.Quantity.Equal(y.Quantity) && x.CostMinor == y.CostMinor && sameDay
	})
}

// setMoveBreakdown writes a move's breakdown released again: the basis on both
// legs and the pieces beside the arriving one, where every read takes them
// from for both (see attachTransferLots). A savepoint inside the caller's
// transaction.
func (s *Store) setMoveBreakdown(ctx context.Context, spaceID uuid.UUID, out, in Operation) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, leg := range []Operation{out, in} {
		if _, err := tx.Exec(ctx, setAmountSQL, spaceID, leg.ID, leg.AmountMinor); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM operation_transfer_lots WHERE operation_id = $1`, in.ID); err != nil {
		return err
	}
	stored := in
	if stored.TransferLots, err = writeTransferLots(ctx, tx, in.ID, in.TransferLots); err != nil {
		return err
	}
	if err := checkStoredLots(stored); err != nil {
		return fmt.Errorf("a move released again, as stored: %w", err)
	}
	return tx.Commit(ctx)
}

// StatedPurchase is one purchase as the owner states it: how many shares,
// either the price of one or what they cost in all, the commission paid on
// top, and — when known — the day they were bought.
type StatedPurchase struct {
	Quantity decimal.Decimal
	// Price is per share, in major units of the arrival's currency.
	Price *decimal.Decimal
	// CostMinor is what the shares cost in all, for an owner who has the total
	// and not the price. Exactly one of the two.
	CostMinor  *int64
	FeeMinor   int64
	AcquiredOn *time.Time
}

// piecesOf turns stated purchases into the pieces of a breakdown. The cost of a
// piece priced per share is struck here, by the same rounding a buy's amount
// is (TradeAmountMinor), so the figure recorded is never one a browser worked
// out; the commission is added to it, as it is to every purchase's lot.
func piecesOf(stated []StatedPurchase) ([]ReleasedLot, error) {
	pieces := make([]ReleasedLot, 0, len(stated))
	for i, sp := range stated {
		if (sp.Price == nil) == (sp.CostMinor == nil) {
			return nil, fmt.Errorf("%w: purchase %d: give either the price of one share or what they cost in all",
				family.ErrValidation, i+1)
		}
		if sp.FeeMinor < 0 || sp.FeeMinor > money.MaxAmountMinor {
			return nil, fmt.Errorf("%w: purchase %d: fee_minor must be within 0..%d",
				family.ErrValidation, i+1, money.MaxAmountMinor)
		}
		var cost int64
		if sp.Price != nil {
			if sp.Price.IsNegative() || sp.Price.GreaterThan(maxPrice) {
				return nil, fmt.Errorf("%w: purchase %d: price must be within 0..%s", family.ErrValidation, i+1, maxPrice)
			}
			var err error
			if cost, err = TradeAmountMinor(TypeSell, sp.Quantity, *sp.Price); err != nil {
				return nil, fmt.Errorf("%w: purchase %d: %s shares at %s cost more than %d",
					family.ErrValidation, i+1, sp.Quantity, sp.Price, money.MaxAmountMinor)
			}
		} else {
			cost = *sp.CostMinor
			if cost < 0 || cost > money.MaxAmountMinor {
				return nil, fmt.Errorf("%w: purchase %d: cost_minor must be within 0..%d",
					family.ErrValidation, i+1, money.MaxAmountMinor)
			}
		}
		withFee, err := money.Add(cost, sp.FeeMinor)
		if err != nil {
			return nil, fmt.Errorf("%w: purchase %d: with its fee it costs more than %d",
				family.ErrValidation, i+1, money.MaxAmountMinor)
		}
		pieces = append(pieces, ReleasedLot{Quantity: sp.Quantity, CostMinor: withFee, AcquiredOn: sp.AcquiredOn})
	}
	return pieces, nil
}

// checkStatedPurchases checks what an owner states about an arrival and returns
// the basis it adds up to.
//
// The pieces must account for exactly the shares that arrived — no more, no
// fewer — because they become the lots those shares are held as. A date is
// optional: a price known without its day is still a price, and the shares then
// carry it undated (see portfolio.DatelessBasis for what that costs). A date
// given cannot be after the day the shares arrived, since they were bought
// before they were moved.
func checkStatedPurchases(op Operation, pieces []ReleasedLot) (int64, error) {
	if len(pieces) == 0 {
		return 0, fmt.Errorf("%w: state at least one purchase", family.ErrValidation)
	}
	if op.Quantity == nil {
		return 0, fmt.Errorf("%w: the arrival carries no quantity to state purchases for", family.ErrValidation)
	}
	var cost int64
	total := decimal.Zero
	for i, pc := range pieces {
		if !pc.Quantity.IsPositive() || pc.Quantity.GreaterThan(maxQuantity) {
			return 0, fmt.Errorf("%w: purchase %d: quantity must be positive and at most %s",
				family.ErrValidation, i+1, maxQuantity)
		}
		if !pc.Quantity.Equal(pc.Quantity.Truncate(quantityScale)) {
			return 0, fmt.Errorf("%w: purchase %d: quantity is finer than the %d decimal places the journal records",
				family.ErrValidation, i+1, quantityScale)
		}
		if pc.CostMinor < 0 || pc.CostMinor > money.MaxAmountMinor {
			return 0, fmt.Errorf("%w: purchase %d: cost_minor must be within 0..%d",
				family.ErrValidation, i+1, money.MaxAmountMinor)
		}
		if pc.AcquiredOn != nil {
			if pc.AcquiredOn.After(op.OccurredOn) {
				return 0, fmt.Errorf("%w: purchase %d: bought on %s, after the shares arrived on %s",
					family.ErrValidation, i+1, pc.AcquiredOn.Format("2006-01-02"), op.OccurredOn.Format("2006-01-02"))
			}
			if pc.AcquiredOn.Before(minOccurredOn) {
				return 0, fmt.Errorf("%w: purchase %d: acquired_on must not be earlier than %s",
					family.ErrValidation, i+1, minOccurredOn.Format("2006-01-02"))
			}
		}
		var err error
		if cost, err = money.Add(cost, pc.CostMinor); err != nil {
			return 0, fmt.Errorf("%w: the purchases add up to more than %d", family.ErrValidation, money.MaxAmountMinor)
		}
		total = total.Add(pc.Quantity)
	}
	if !total.Equal(*op.Quantity) {
		return 0, fmt.Errorf("%w: the purchases add up to %s shares and %s arrived", family.ErrValidation, total, op.Quantity)
	}
	return cost, nil
}

// journalReplacing is the journal with one row restated under its own moment
// of recording, in the order the engine folds it: on its own day it folds
// exactly where the row it replaces did, and an edit that moves it to another
// day puts it among that day's rows by the same moment.
func journalReplacing(journal []Operation, op Operation) []Operation {
	out := make([]Operation, len(journal))
	copy(out, journal)
	for i := range out {
		if out[i].ID == op.ID {
			out[i] = op
		}
	}
	sortJournal(out)
	return out
}

const (
	setAmountSQL = `UPDATE operations SET amount_minor = $3 WHERE space_id = $1 AND id = $2 RETURNING ` + cols

	insertStatedSQL = `
		INSERT INTO operation_stated_purchases
			(space_id, account_id, source, external_id, seq, quantity, cost_minor, acquired_on)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
)

// setPurchases writes an arrival's stated purchases: the basis on the row, the
// pieces as its breakdown and, for a row an importer rebuilds, a copy under the
// row's own name for the importer to find (see StatedPurchases). One
// transaction — a savepoint, when the caller already holds one.
func (s *Store) setPurchases(ctx context.Context, spaceID uuid.UUID, op Operation, cost int64, pieces []ReleasedLot) (Operation, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Operation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stored, err := scan(tx.QueryRow(ctx, setAmountSQL, spaceID, op.ID, cost))
	if err != nil {
		return Operation{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM operation_transfer_lots WHERE operation_id = $1`, op.ID); err != nil {
		return Operation{}, err
	}
	if stored.TransferLots, err = writeTransferLots(ctx, tx, op.ID, pieces); err != nil {
		return Operation{}, err
	}
	if err := portfolio.CheckTransferLots(stored); err != nil {
		return Operation{}, fmt.Errorf("stated purchases as stored: %w", err)
	}

	if op.Source != SourceManual && op.ExternalID != nil {
		if _, err := tx.Exec(ctx, `DELETE FROM operation_stated_purchases
			WHERE account_id = $1 AND source = $2 AND external_id = $3`,
			op.AccountID, op.Source, *op.ExternalID); err != nil {
			return Operation{}, err
		}
		batch := &pgx.Batch{}
		for i, pc := range stored.TransferLots {
			batch.Queue(insertStatedSQL, spaceID, op.AccountID, op.Source, *op.ExternalID,
				i, pc.Quantity, pc.CostMinor, pc.AcquiredOn)
		}
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return Operation{}, fmt.Errorf("stated purchases: %w", err)
		}
	}
	return stored, tx.Commit(ctx)
}

// StatedKey names an imported row within its space: its account and the name
// its importer gave it.
type StatedKey struct {
	AccountID  uuid.UUID
	ExternalID string
}

// StatedPurchases returns what the owner stated about the arrivals an importer
// wrote into these accounts, by row, each row's pieces in order. An importer
// that rebuilds its rows from the broker's record reads this to put the pieces
// back (see StatePurchases).
func (s *Store) StatedPurchases(ctx context.Context, spaceID uuid.UUID, accountIDs []uuid.UUID, source string) (
	map[StatedKey][]ReleasedLot, error,
) {
	out := map[StatedKey][]ReleasedLot{}
	if len(accountIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT account_id, external_id, quantity, cost_minor, acquired_on
		FROM operation_stated_purchases
		WHERE space_id = $1 AND account_id = ANY($2) AND source = $3
		ORDER BY account_id, external_id, seq`, spaceID, accountIDs, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key StatedKey
		var pc ReleasedLot
		if err := rows.Scan(&key.AccountID, &key.ExternalID, &pc.Quantity, &pc.CostMinor, &pc.AcquiredOn); err != nil {
			return nil, err
		}
		out[key] = append(out[key], pc)
	}
	return out, rows.Err()
}
