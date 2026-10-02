package operation

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/currency"
	"babki.my/babki/internal/portfolio"
)

// ArrivalParams describes shares arriving from another broker, entered by hand:
// how many of which paper reached which account on which day, and — when the
// owner has them — the purchases behind them.
type ArrivalParams struct {
	AccountID    uuid.UUID
	InstrumentID uuid.UUID
	OccurredOn   time.Time
	Quantity     decimal.Decimal
	Currency     string
	Note         string
	Purchases    []StatedPurchase
}

// CreateArrival records shares arriving from another broker on an account no
// importer feeds: a transfer_in with no sibling, which until now only an
// importer could write. The hand-entry door refuses transfer legs because a move
// between two of the owner's accounts has to be written as a pair; shares from a
// broker this program does not hold have no other half to write.
//
// Without purchases the shares arrive bought for nothing (see
// portfolio.UnknownCost) and can be given their purchases later
// (StatePurchases). With them, the purchases are the arrival's breakdown from
// the start, checked by the same rules.
func (s *Service) CreateArrival(ctx context.Context, spaceID uuid.UUID, p ArrivalParams) (Operation, error) {
	if !p.Quantity.IsPositive() {
		return Operation{}, fmt.Errorf("%w: quantity must be positive", family.ErrValidation)
	}
	if err := checkQuantityBound(p.Quantity); err != nil {
		return Operation{}, err
	}
	if !currency.Valid(p.Currency) {
		return Operation{}, fmt.Errorf("%w: currency must be ISO-4217 uppercase", family.ErrValidation)
	}
	if err := checkOccurredOn(p.OccurredOn); err != nil {
		return Operation{}, err
	}
	if err := checkNote(p.Note); err != nil {
		return Operation{}, err
	}
	instrumentID, quantity := p.InstrumentID, p.Quantity
	op := Operation{
		AccountID: p.AccountID, InstrumentID: &instrumentID, Type: TypeTransferIn,
		OccurredOn: p.OccurredOn, Quantity: &quantity, Currency: p.Currency,
		Note: p.Note, Source: SourceManual,
	}
	if err := normalizeForStorage(&op); err != nil {
		return Operation{}, err
	}
	if len(p.Purchases) > 0 {
		pieces, err := piecesOf(p.Purchases)
		if err != nil {
			return Operation{}, err
		}
		cost, err := checkStatedPurchases(op, pieces)
		if err != nil {
			return Operation{}, err
		}
		op.AmountMinor, op.TransferLots = cost, pieces
	}

	var created Operation
	err := s.store.WithOpenAccountsLocked(ctx, spaceID, []uuid.UUID{p.AccountID}, func(st *Store) error {
		journal, err := st.ListForEngine(ctx, spaceID, p.AccountID)
		if err != nil {
			return err
		}
		if err := checkJournalOps(journal, []Operation{op}, nil); err != nil {
			return err
		}
		stored, err := st.ApplyDelta(ctx, spaceID, []Operation{op}, nil, func(stored []Operation) error {
			if _, err := portfolio.Compute(journalWith(journal, stored, nil)); err != nil {
				return fmt.Errorf("the arrival as stored no longer replays on account %s: %v", p.AccountID, err)
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
	s.manualWriteDone(ctx, spaceID, p.AccountID)
	return created, nil
}

// Arrival is one occasion on which shares of a paper reached an account, as the
// screen that asks for their purchases needs it.
type Arrival struct {
	Operation Operation
	// FromAnotherBroker is true for shares with no other half in this program:
	// the ones whose purchases can be stated (StatePurchases).
	FromAnotherBroker bool
	// FromAccountID is the account a move between the owner's accounts left,
	// which is where those shares' purchases are to be stated. Nil otherwise.
	FromAccountID *uuid.UUID
}

// Arrivals lists the shares of a paper that reached an account by a transfer,
// oldest first, each with the purchases recorded behind it.
func (s *Service) Arrivals(ctx context.Context, spaceID, accountID, instrumentID uuid.UUID) ([]Arrival, error) {
	journal, err := s.store.ListForEngine(ctx, spaceID, accountID)
	if err != nil {
		return nil, err
	}
	out := []Arrival{}
	for _, o := range journal {
		if o.Type != TypeTransferIn || o.InstrumentID == nil || *o.InstrumentID != instrumentID {
			continue
		}
		a := Arrival{Operation: o, FromAnotherBroker: o.TransferGroupID == nil}
		if o.TransferGroupID != nil {
			legs, err := s.store.ByTransferGroup(ctx, spaceID, *o.TransferGroupID)
			if err != nil {
				return nil, err
			}
			if i := slices.IndexFunc(legs, func(l Operation) bool { return l.ID != o.ID }); i >= 0 {
				from := legs[i].AccountID
				a.FromAccountID = &from
			}
		}
		out = append(out, a)
	}
	return out, nil
}
