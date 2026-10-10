package creditcard

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/db"
)

// ErrNotFound is a card with no terms stated: a 404.
var ErrNotFound = fmt.Errorf("credit card: %w", pgx.ErrNoRows)

type accounts interface {
	ByID(ctx context.Context, spaceID, id uuid.UUID) (account.WithBalance, error)
	ListWithBalance(ctx context.Context, spaceID uuid.UUID) ([]account.WithBalance, error)
}

type journal interface {
	ListForEngine(ctx context.Context, spaceID, accountID uuid.UUID) ([]operation.Operation, error)
}

// Service keeps cards' terms and works out where each card stands.
type Service struct {
	db       db.Executor
	accounts accounts
	journal  journal
	now      func() time.Time
}

func NewService(x db.Executor, acc accounts, j journal) *Service {
	return &Service{db: x, accounts: acc, journal: j, now: time.Now}
}

const cols = `account_id, limit_minor, statement_day, payment_days, grace_kind, grace_days,
	min_percent, min_floor_minor, annual_rate, own_rate`

func scan(row pgx.Row) (Terms, error) {
	var t Terms
	var own decimal.NullDecimal
	err := row.Scan(&t.AccountID, &t.Limit, &t.StatementDay, &t.PaymentDays, &t.GraceKind, &t.GraceDays,
		&t.MinPercent, &t.MinFloor, &t.AnnualRate, &own)
	if own.Valid {
		t.OwnRate = &own.Decimal
	}
	return t, err
}

// Terms are the card's stated terms.
func (s *Service) Terms(ctx context.Context, spaceID, accountID uuid.UUID) (Terms, error) {
	t, err := scan(s.db.QueryRow(ctx, `SELECT `+cols+` FROM credit_cards WHERE space_id = $1 AND account_id = $2`, spaceID, accountID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Terms{}, ErrNotFound
	}
	if err != nil {
		return Terms{}, fmt.Errorf("credit card: terms: %w", err)
	}
	return t, nil
}

// SetTerms states or restates a credit card's terms.
func (s *Service) SetTerms(ctx context.Context, spaceID uuid.UUID, t Terms) (Terms, error) {
	if err := t.Validate(); err != nil {
		return Terms{}, err
	}
	a, err := s.accounts.ByID(ctx, spaceID, t.AccountID)
	if err != nil {
		return Terms{}, err
	}
	if a.Type != account.TypeCreditCard {
		return Terms{}, fmt.Errorf("%w: a limit and a grace period are a credit card's", family.ErrValidation)
	}
	if t.GraceKind == FromStatement {
		t.GraceDays = 0
	}
	var own decimal.NullDecimal
	if t.OwnRate != nil {
		own = decimal.NullDecimal{Decimal: *t.OwnRate, Valid: true}
	}
	_, err = s.db.Exec(ctx, `
		INSERT INTO credit_cards (account_id, space_id, limit_minor, statement_day, payment_days, grace_kind,
			grace_days, min_percent, min_floor_minor, annual_rate, own_rate)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (account_id) DO UPDATE SET limit_minor = EXCLUDED.limit_minor,
			statement_day = EXCLUDED.statement_day, payment_days = EXCLUDED.payment_days,
			grace_kind = EXCLUDED.grace_kind, grace_days = EXCLUDED.grace_days, min_percent = EXCLUDED.min_percent,
			min_floor_minor = EXCLUDED.min_floor_minor, annual_rate = EXCLUDED.annual_rate,
			own_rate = EXCLUDED.own_rate, updated_at = now()`,
		t.AccountID, spaceID, t.Limit, t.StatementDay, t.PaymentDays, t.GraceKind, t.GraceDays,
		t.MinPercent, t.MinFloor, t.AnnualRate, own)
	if err != nil {
		return Terms{}, fmt.Errorf("credit card: set terms: %w", err)
	}
	return s.Terms(ctx, spaceID, t.AccountID)
}

// DeleteTerms forgets a card's terms; the journal keeps what was spent.
func (s *Service) DeleteTerms(ctx context.Context, spaceID, accountID uuid.UUID) error {
	ct, err := s.db.Exec(ctx, `DELETE FROM credit_cards WHERE space_id = $1 AND account_id = $2`, spaceID, accountID)
	if err != nil {
		return fmt.Errorf("credit card: delete terms: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Card is a card with its terms and where it stands today. ByJournal says the
// status comes from the card's journal; otherwise from its last balance,
// which tells the debt but not what keeps the grace.
type Card struct {
	Account   account.WithBalance
	Terms     Terms
	Status    Status
	ByJournal bool
}

// Card is the account's card today.
func (s *Service) Card(ctx context.Context, spaceID, accountID uuid.UUID) (Card, error) {
	a, err := s.accounts.ByID(ctx, spaceID, accountID)
	if err != nil {
		return Card{}, err
	}
	t, err := s.Terms(ctx, spaceID, accountID)
	if err != nil {
		return Card{}, err
	}
	return s.card(ctx, spaceID, a, t)
}

func (s *Service) card(ctx context.Context, spaceID uuid.UUID, a account.WithBalance, t Terms) (Card, error) {
	now := s.now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if !account.CountedByJournal(a.Account) {
		var balance int64
		if a.Balance != nil {
			balance = a.Balance.AmountMinor
		}
		return Card{Account: a, Terms: t, Status: ByBalance(t, balance, today)}, nil
	}
	ops, err := s.journal.ListForEngine(ctx, spaceID, a.ID)
	if err != nil {
		return Card{}, err
	}
	return Card{Account: a, Terms: t, Status: Work(t, ops, a.Currency, today), ByJournal: true}, nil
}

// AllTerms is every card's terms in the space.
func (s *Service) AllTerms(ctx context.Context, spaceID uuid.UUID) ([]Terms, error) {
	rows, err := s.db.Query(ctx, `SELECT `+cols+` FROM credit_cards WHERE space_id = $1 ORDER BY account_id`, spaceID)
	if err != nil {
		return nil, fmt.Errorf("credit card: all: %w", err)
	}
	defer rows.Close()
	var out []Terms
	for rows.Next() {
		t, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("credit card: all: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// All is every active card with terms in the space, for the reminders.
func (s *Service) All(ctx context.Context, spaceID uuid.UUID) ([]Card, error) {
	all, err := s.AllTerms(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return []Card{}, nil
	}
	terms := map[uuid.UUID]Terms{}
	for _, t := range all {
		terms[t.AccountID] = t
	}
	list, err := s.accounts.ListWithBalance(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := []Card{}
	for _, a := range list {
		t, ok := terms[a.ID]
		if !ok || a.Status != account.StatusActive {
			continue
		}
		c, err := s.card(ctx, spaceID, a, t)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}
