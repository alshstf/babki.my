package operation

import (
	"context"
	"fmt"
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
// Stating them again replaces what was stated before. The account's journal is
// replayed with the restated arrival before anything is written, so a statement
// the rest of the journal cannot take — shares later moved on with a breakdown
// recorded against the old basis — is refused with the engine's reason.
func (s *Service) StatePurchases(ctx context.Context, spaceID, operationID uuid.UUID, stated []StatedPurchase) (Operation, error) {
	pieces, err := piecesOf(stated)
	if err != nil {
		return Operation{}, err
	}
	// Which account to lock cannot be learned under the lock, so the row is read
	// once on the pool for its account alone and read again inside.
	first, err := s.store.ByID(ctx, spaceID, operationID)
	if err != nil {
		return Operation{}, err
	}
	var stored Operation
	err = s.store.WithAccountsLocked(ctx, spaceID, []uuid.UUID{first.AccountID}, func(st *Store) error {
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

		journal, err := st.ListForEngine(ctx, spaceID, op.AccountID)
		if err != nil {
			return err
		}
		if _, err := portfolio.Compute(journalReplacing(journal, restated)); err != nil {
			return fmt.Errorf("%w: %v", ErrInconsistent, err)
		}
		stored, err = st.setPurchases(ctx, spaceID, op, cost, pieces)
		if err != nil {
			return err
		}
		// The arrival as stored, folded once more before the commit: the pieces'
		// quantities come back from a column with a scale (see writeTransferLots).
		if _, err := portfolio.Compute(journalReplacing(journal, stored)); err != nil {
			return fmt.Errorf("the purchases as stored no longer replay on account %s: %v", op.AccountID, err)
		}
		return nil
	})
	if err != nil {
		return Operation{}, mapWriteError(err)
	}
	return stored, nil
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
