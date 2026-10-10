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
	"babki.my/babki/internal/category"
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

type categories interface {
	List(ctx context.Context, spaceID uuid.UUID) ([]category.Category, error)
}

// Service keeps cards' terms and works out where each card stands.
type Service struct {
	db         db.Executor
	accounts   accounts
	journal    journal
	categories categories
	now        func() time.Time
}

func NewService(x db.Executor, acc accounts, j journal, cats categories) *Service {
	return &Service{db: x, accounts: acc, journal: j, categories: cats, now: time.Now}
}

// Default categories that tell cashback and charges apart on a card's
// journal, whatever the row's type.
var (
	cashbackNames = map[string]bool{"Кэшбэк": true}
	chargeNames   = map[string]bool{"Проценты по кредитам": true, "Банковские комиссии": true}
)

// categorySet is the family's categories as the cards read them: the
// default kinds, and each category's parent, for a card's transfer
// categories.
type categorySet struct {
	kinds  Kinds
	parent map[uuid.UUID]uuid.UUID
}

func (s *Service) categorySet(ctx context.Context, spaceID uuid.UUID) (categorySet, error) {
	list, err := s.categories.List(ctx, spaceID)
	if err != nil {
		return categorySet{}, err
	}
	f := categorySet{kinds: Kinds{Cashback: map[uuid.UUID]bool{}, Charges: map[uuid.UUID]bool{}}, parent: map[uuid.UUID]uuid.UUID{}}
	for _, c := range list {
		switch {
		case c.Kind == category.KindIncome && cashbackNames[c.Name]:
			f.kinds.Cashback[c.ID] = true
		case c.Kind == category.KindExpense && chargeNames[c.Name]:
			f.kinds.Charges[c.ID] = true
		}
		if c.ParentID != nil {
			f.parent[c.ID] = *c.ParentID
		}
	}
	return f, nil
}

// of is the kinds for a card: the family's, with the card's transfer
// categories and the subcategories under them.
func (f categorySet) of(t Terms) Kinds {
	k := f.kinds
	k.Transfers = map[uuid.UUID]bool{}
	for _, id := range t.TransferCategories {
		k.Transfers[id] = true
	}
	for child, parent := range f.parent {
		if k.Transfers[parent] {
			k.Transfers[child] = true
		}
	}
	return k
}

// spendingCategories checks that the ids are the family's spending
// categories, each once; nil comes back empty.
func (s *Service) spendingCategories(ctx context.Context, spaceID uuid.UUID, ids []uuid.UUID) ([]uuid.UUID, error) {
	out := []uuid.UUID{}
	if len(ids) == 0 {
		return out, nil
	}
	list, err := s.categories.List(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	spending := map[uuid.UUID]bool{}
	for _, c := range list {
		if c.Kind == category.KindExpense {
			spending[c.ID] = true
		}
	}
	seen := map[uuid.UUID]bool{}
	for _, id := range ids {
		if !spending[id] {
			return nil, fmt.Errorf("%w: a transfer category is one of the family's spending categories", family.ErrValidation)
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

const cols = `account_id, limit_minor, statement_day, payment_days, grace_kind, grace_days,
	min_percent, min_floor_minor, annual_rate, own_rate, window_months, grace_months, opened_on,
	grace_all_lost, pay_by_period_end, charges_in_full, transfer_categories`

func scan(row pgx.Row) (Terms, error) {
	var t Terms
	var own decimal.NullDecimal
	err := row.Scan(&t.AccountID, &t.Limit, &t.StatementDay, &t.PaymentDays, &t.GraceKind, &t.GraceDays,
		&t.MinPercent, &t.MinFloor, &t.AnnualRate, &own, &t.WindowMonths, &t.GraceMonths, &t.OpenedOn,
		&t.GraceAllLost, &t.PayByPeriodEnd, &t.ChargesInFull, &t.TransferCategories)
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
	// Only the grace's own kind keeps its numbers.
	if t.GraceKind != Long {
		t.GraceDays = 0
	}
	if t.GraceKind != Windows {
		t.WindowMonths, t.GraceMonths, t.OpenedOn = 0, 0, nil
	}
	if t.TransferCategories, err = s.spendingCategories(ctx, spaceID, t.TransferCategories); err != nil {
		return Terms{}, err
	}
	var own decimal.NullDecimal
	if t.OwnRate != nil {
		own = decimal.NullDecimal{Decimal: *t.OwnRate, Valid: true}
	}
	_, err = s.db.Exec(ctx, `
		INSERT INTO credit_cards (account_id, space_id, limit_minor, statement_day, payment_days, grace_kind,
			grace_days, min_percent, min_floor_minor, annual_rate, own_rate, window_months, grace_months,
			opened_on, grace_all_lost, pay_by_period_end, charges_in_full, transfer_categories)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
		ON CONFLICT (account_id) DO UPDATE SET limit_minor = EXCLUDED.limit_minor,
			statement_day = EXCLUDED.statement_day, payment_days = EXCLUDED.payment_days,
			grace_kind = EXCLUDED.grace_kind, grace_days = EXCLUDED.grace_days, min_percent = EXCLUDED.min_percent,
			min_floor_minor = EXCLUDED.min_floor_minor, annual_rate = EXCLUDED.annual_rate,
			own_rate = EXCLUDED.own_rate, window_months = EXCLUDED.window_months,
			grace_months = EXCLUDED.grace_months, opened_on = EXCLUDED.opened_on,
			grace_all_lost = EXCLUDED.grace_all_lost, pay_by_period_end = EXCLUDED.pay_by_period_end,
			charges_in_full = EXCLUDED.charges_in_full, transfer_categories = EXCLUDED.transfer_categories,
			updated_at = now()`,
		t.AccountID, spaceID, t.Limit, t.StatementDay, t.PaymentDays, t.GraceKind, t.GraceDays,
		t.MinPercent, t.MinFloor, t.AnnualRate, own, t.WindowMonths, t.GraceMonths, t.OpenedOn,
		t.GraceAllLost, t.PayByPeriodEnd, t.ChargesInFull, t.TransferCategories)
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
// which tells the debt but not what keeps the grace nor what the card was
// worth. Benefit is set by Card, not by All.
type Card struct {
	Account   account.WithBalance
	Terms     Terms
	Status    Status
	ByJournal bool
	Benefit   *Benefit
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
	f, err := s.categorySet(ctx, spaceID)
	if err != nil {
		return Card{}, err
	}
	k := f.of(t)
	c, ops, err := s.card(ctx, spaceID, a, t, k)
	if err != nil || !c.ByJournal {
		return c, err
	}
	b := Weigh(t, ops, a.Currency, s.today(), k)
	// What the bank will charge for the grace lost is a cost too, though not
	// in the journal yet.
	b.Pending = c.Status.NonGraceInterest
	for _, l := range c.Status.Lost {
		b.Pending += l.Interest
	}
	b.Total -= b.Pending
	c.Benefit = &b
	return c, nil
}

func (s *Service) today() time.Time {
	now := s.now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// card is the card today, with the journal it was worked out from (none when
// counted by its balance).
func (s *Service) card(ctx context.Context, spaceID uuid.UUID, a account.WithBalance, t Terms, k Kinds) (Card, []operation.Operation, error) {
	today := s.today()
	if !account.CountedByJournal(a.Account) {
		var balance int64
		if a.Balance != nil {
			balance = a.Balance.AmountMinor
		}
		return Card{Account: a, Terms: t, Status: ByBalance(t, balance, today)}, nil, nil
	}
	ops, err := s.journal.ListForEngine(ctx, spaceID, a.ID)
	if err != nil {
		return Card{}, nil, err
	}
	return Card{Account: a, Terms: t, Status: Work(t, ops, a.Currency, today, k), ByJournal: true}, ops, nil
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
	f, err := s.categorySet(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := []Card{}
	for _, a := range list {
		t, ok := terms[a.ID]
		if !ok || a.Status != account.StatusActive {
			continue
		}
		c, _, err := s.card(ctx, spaceID, a, t, f.of(t))
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}
