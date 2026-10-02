package operation

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/family"
)

// MoneyTransferParams describes money moved from one of the family's accounts
// to another. Received is what arrived when the money was converted on the
// way; nil, it arrives as it left.
type MoneyTransferParams struct {
	FromAccountID uuid.UUID
	ToAccountID   uuid.UUID
	OccurredOn    time.Time
	AmountMinor   int64
	Currency      string
	Received      *Money
	Note          string
}

// Money is an amount in minor units of a currency.
type Money struct {
	Minor    int64
	Currency string
}

// CreateMoneyTransfer records money moved between two of the family's
// accounts: a withdrawal from one and a deposit into the other, linked as one
// transfer so that deleting either deletes both. Each half is held to the rules
// a withdrawal or a deposit entered on its own is, and both accounts' journals
// are replayed under their locks before anything is written.
func (s *Service) CreateMoneyTransfer(ctx context.Context, spaceID uuid.UUID, p MoneyTransferParams) (out, in Operation, err error) {
	if p.FromAccountID == p.ToAccountID {
		return Operation{}, Operation{}, fmt.Errorf("%w: from and to accounts must differ", family.ErrValidation)
	}
	if p.AmountMinor <= 0 {
		return Operation{}, Operation{}, fmt.Errorf("%w: amount_minor must be positive", family.ErrValidation)
	}
	received := Money{Minor: p.AmountMinor, Currency: p.Currency}
	if p.Received != nil {
		if p.Received.Minor <= 0 {
			return Operation{}, Operation{}, fmt.Errorf("%w: received_minor must be positive", family.ErrValidation)
		}
		received = *p.Received
	}
	out = Operation{
		AccountID: p.FromAccountID, Type: TypeWithdrawal, OccurredOn: p.OccurredOn,
		AmountMinor: -p.AmountMinor, Currency: p.Currency, Note: p.Note, Source: SourceManual,
	}
	in = Operation{
		AccountID: p.ToAccountID, Type: TypeDeposit, OccurredOn: p.OccurredOn,
		AmountMinor: received.Minor, Currency: received.Currency, Note: p.Note, Source: SourceManual,
	}
	for _, leg := range []*Operation{&out, &in} {
		if err := normalizeForStorage(leg); err != nil {
			return Operation{}, Operation{}, err
		}
		if err := validate(*leg); err != nil {
			return Operation{}, Operation{}, err
		}
	}

	err = s.store.WithOpenAccountsLocked(ctx, spaceID, []uuid.UUID{p.FromAccountID, p.ToAccountID}, func(st *Store) error {
		if err := checkJournal(ctx, st, spaceID, p.FromAccountID, []Operation{out}, nil); err != nil {
			return err
		}
		if err := checkJournal(ctx, st, spaceID, p.ToAccountID, []Operation{in}, nil); err != nil {
			return err
		}
		out, in, err = st.CreatePair(ctx, spaceID, out, in, nil)
		return err
	})
	if err != nil {
		return Operation{}, Operation{}, mapWriteError(err)
	}
	s.manualWriteDone(ctx, spaceID, p.FromAccountID, p.ToAccountID)
	return out, in, nil
}
