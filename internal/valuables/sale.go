package valuables

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
)

type journal interface {
	Create(ctx context.Context, spaceID uuid.UUID, op operation.Operation) (operation.Operation, error)
	Delete(ctx context.Context, spaceID, id uuid.UUID) error
}

type categories interface {
	List(ctx context.Context, spaceID uuid.UUID) ([]category.Category, error)
}

// WithJournal lets a sale write its money to an account.
func (s *Service) WithJournal(j journal, c categories) *Service {
	s.journal, s.categories = j, c
	return s
}

// SaleCategory is the earning category a sale's money is filed under, when
// the family has it.
const SaleCategory = "Продажа вещей"

// Sale is a piece sold: the day, what it sold for in roubles, and the
// account the money went to — none when the family writes it itself.
type Sale struct {
	On          time.Time
	Minor       int64
	ToAccountID *uuid.UUID
}

// Sell records a piece sold and, when an account is named, the money there
// as an earning under «Продажа вещей». Should the piece's record fail, the
// money's row is taken back.
func (s *Service) Sell(ctx context.Context, spaceID, accountID, itemID uuid.UUID, sale Sale) (Holding, error) {
	if err := s.storageAccount(ctx, spaceID, accountID); err != nil {
		return Holding{}, err
	}
	it, err := s.item(ctx, accountID, itemID)
	if err != nil {
		return Holding{}, err
	}
	switch {
	case it.SoldOn != nil:
		return Holding{}, fmt.Errorf("%w: the piece is sold already", family.ErrValidation)
	case sale.Minor < 0:
		return Holding{}, fmt.Errorf("%w: what it sold for is not below zero", family.ErrValidation)
	case sale.On.Before(it.BoughtOn) || sale.On.After(s.now()):
		return Holding{}, fmt.Errorf("%w: a piece is sold after it was bought and not in the future", family.ErrValidation)
	}
	var opID *uuid.UUID
	if sale.ToAccountID != nil {
		if s.journal == nil {
			return Holding{}, errors.New("valuables: a sale's money asked of a service built without WithJournal")
		}
		to, err := s.accounts.ByID(ctx, spaceID, *sale.ToAccountID)
		if err != nil {
			return Holding{}, err
		}
		if to.Currency != rouble || to.Status != account.StatusActive || to.Type == account.TypeValuables {
			return Holding{}, fmt.Errorf("%w: the money goes to an active rouble account", family.ErrValidation)
		}
		op, err := s.journal.Create(ctx, spaceID, operation.Operation{
			AccountID: to.ID, Type: operation.TypeDeposit, OccurredOn: sale.On, AmountMinor: sale.Minor, Currency: rouble,
			Note: saleNote(it), CategoryID: s.saleCategory(ctx, spaceID),
		})
		if err != nil {
			return Holding{}, err
		}
		opID = &op.ID
	}
	if _, err := s.db.Exec(ctx, `UPDATE valuables_items SET sold_on = $3, sold_minor = $4, sale_operation_id = $5
		WHERE id = $1 AND account_id = $2`, itemID, accountID, sale.On, sale.Minor, opID); err != nil {
		if opID != nil {
			_ = s.journal.Delete(ctx, spaceID, *opID)
		}
		return Holding{}, fmt.Errorf("valuables: sell: %w", err)
	}
	return s.holding(ctx, spaceID, accountID, s.now())
}

// Unsell takes a sale back — recorded by mistake — and the money's row with
// it.
func (s *Service) Unsell(ctx context.Context, spaceID, accountID, itemID uuid.UUID) (Holding, error) {
	if err := s.storageAccount(ctx, spaceID, accountID); err != nil {
		return Holding{}, err
	}
	it, err := s.item(ctx, accountID, itemID)
	if err != nil {
		return Holding{}, err
	}
	if it.SoldOn == nil {
		return Holding{}, fmt.Errorf("%w: the piece is not sold", family.ErrValidation)
	}
	if it.SaleOperationID != nil && s.journal != nil {
		if err := s.journal.Delete(ctx, spaceID, *it.SaleOperationID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Holding{}, err
		}
	}
	if _, err := s.db.Exec(ctx, `UPDATE valuables_items SET sold_on = NULL, sold_minor = NULL, sale_operation_id = NULL
		WHERE id = $1 AND account_id = $2`, itemID, accountID); err != nil {
		return Holding{}, fmt.Errorf("valuables: unsell: %w", err)
	}
	return s.holding(ctx, spaceID, accountID, s.now())
}

func (s *Service) item(ctx context.Context, accountID, itemID uuid.UUID) (Item, error) {
	items, err := s.items(ctx, accountID)
	if err != nil {
		return Item{}, err
	}
	for _, it := range items {
		if it.ID == itemID {
			return it, nil
		}
	}
	return Item{}, ErrNotFound
}

// saleNote is the money's row's words: what was sold.
func saleNote(it Item) string {
	what := "Продажа слитка"
	if it.Kind == KindCoin {
		what = "Продажа монеты"
	}
	note := fmt.Sprintf("%s: %s г, %s", what, it.Weight.String(), metalName[it.Metal])
	if it.Serial != "" {
		note += ", № " + it.Serial
	}
	return note
}

var metalName = map[marketdata.Metal]string{
	marketdata.Gold: "золото", marketdata.Silver: "серебро", marketdata.Platinum: "платина", marketdata.Palladium: "палладий",
}

// saleCategory is the family's active «Продажа вещей», or nil.
func (s *Service) saleCategory(ctx context.Context, spaceID uuid.UUID) *uuid.UUID {
	if s.categories == nil {
		return nil
	}
	list, err := s.categories.List(ctx, spaceID)
	if err != nil {
		return nil
	}
	for _, c := range list {
		if c.Kind == category.KindIncome && c.Name == SaleCategory && !c.Archived {
			id := c.ID
			return &id
		}
	}
	return nil
}

// SoldPiece is a piece sold in a year, with the account it was kept on.
type SoldPiece struct {
	Item
	AccountOwner *uuid.UUID
}

type lister interface {
	ListWithBalance(ctx context.Context, spaceID uuid.UUID) ([]account.WithBalance, error)
}

// SoldIn are the space's pieces sold in year, for the year's tax (decision
// Р-41: the sale of property).
func (s *Service) SoldIn(ctx context.Context, spaceID uuid.UUID, year int) ([]SoldPiece, error) {
	l, ok := s.accounts.(lister)
	if !ok {
		return nil, errors.New("valuables: the accounts cannot be listed")
	}
	list, err := l.ListWithBalance(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	var out []SoldPiece
	for _, a := range list {
		if a.Type != account.TypeValuables {
			continue
		}
		items, err := s.items(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if it.SoldOn != nil && it.SoldOn.Year() == year {
				out = append(out, SoldPiece{Item: it, AccountOwner: a.OwnerUserID})
			}
		}
	}
	return out, nil
}
