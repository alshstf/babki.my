package loan

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
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/db"
)

// ErrNotFound is an account with no terms stated: a 404.
var ErrNotFound = fmt.Errorf("loan: %w", pgx.ErrNoRows)

// InterestCategory is the default spending category a loan's interest is filed
// under, when the family still has it.
const InterestCategory = "Проценты по кредитам"

type accounts interface {
	ByID(ctx context.Context, spaceID, id uuid.UUID) (account.WithBalance, error)
}

type journal interface {
	Create(ctx context.Context, spaceID uuid.UUID, op operation.Operation) (operation.Operation, error)
	CreateMoneyTransfer(ctx context.Context, spaceID uuid.UUID, p operation.MoneyTransferParams) (out, in operation.Operation, err error)
	Delete(ctx context.Context, spaceID, id uuid.UUID) error
}

type categories interface {
	List(ctx context.Context, spaceID uuid.UUID) ([]category.Category, error)
}

// Service keeps loans' terms and records their payments.
type Service struct {
	db         db.Executor
	accounts   accounts
	journal    journal
	categories categories
}

func NewService(x db.Executor, acc accounts, j journal, cats categories) *Service {
	return &Service{db: x, accounts: acc, journal: j, categories: cats}
}

// loanAccount is the space's account id, refused unless it is a loan.
func (s *Service) loanAccount(ctx context.Context, spaceID, accountID uuid.UUID) (account.WithBalance, error) {
	a, err := s.accounts.ByID(ctx, spaceID, accountID)
	if err != nil {
		return account.WithBalance{}, err
	}
	if a.Type != account.TypeLoan {
		return account.WithBalance{}, fmt.Errorf("%w: terms and a schedule are a loan account's", family.ErrValidation)
	}
	return a, nil
}

// Terms are the account's stated terms.
func (s *Service) Terms(ctx context.Context, spaceID, accountID uuid.UUID) (Terms, error) {
	var t Terms
	err := s.db.QueryRow(ctx, `
		SELECT account_id, principal_minor, annual_rate, term_months, issued_on, kind
		FROM loans WHERE space_id = $1 AND account_id = $2`, spaceID, accountID).
		Scan(&t.AccountID, &t.Principal, &t.AnnualRate, &t.TermMonths, &t.IssuedOn, &t.Kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return Terms{}, ErrNotFound
	}
	if err != nil {
		return Terms{}, fmt.Errorf("loan: terms: %w", err)
	}
	pre, err := s.prepayments(ctx, spaceID, &accountID)
	if err != nil {
		return Terms{}, err
	}
	t.Prepayments = pre[accountID]
	return t, nil
}

// prepayments are the space's prepayments by loan, or one loan's.
func (s *Service) prepayments(ctx context.Context, spaceID uuid.UUID, accountID *uuid.UUID) (map[uuid.UUID][]Prepayment, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, account_id, paid_on, amount_minor, mode FROM loan_prepayments
		WHERE space_id = $1 AND ($2::uuid IS NULL OR account_id = $2) ORDER BY paid_on, created_at`, spaceID, accountID)
	if err != nil {
		return nil, fmt.Errorf("loan: prepayments: %w", err)
	}
	defer rows.Close()
	out := map[uuid.UUID][]Prepayment{}
	for rows.Next() {
		var p Prepayment
		var account uuid.UUID
		if err := rows.Scan(&p.ID, &account, &p.On, &p.Amount, &p.Mode); err != nil {
			return nil, fmt.Errorf("loan: prepayments: %w", err)
		}
		out[account] = append(out[account], p)
	}
	return out, rows.Err()
}

// All is every loan's terms in the space.
func (s *Service) All(ctx context.Context, spaceID uuid.UUID) ([]Terms, error) {
	rows, err := s.db.Query(ctx, `
		SELECT account_id, principal_minor, annual_rate, term_months, issued_on, kind
		FROM loans WHERE space_id = $1 ORDER BY issued_on`, spaceID)
	if err != nil {
		return nil, fmt.Errorf("loan: all: %w", err)
	}
	defer rows.Close()
	var out []Terms
	for rows.Next() {
		var t Terms
		if err := rows.Scan(&t.AccountID, &t.Principal, &t.AnnualRate, &t.TermMonths, &t.IssuedOn, &t.Kind); err != nil {
			return nil, fmt.Errorf("loan: all: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("loan: all: %w", err)
	}
	rows.Close()
	pre, err := s.prepayments(ctx, spaceID, nil)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Prepayments = pre[out[i].AccountID]
	}
	return out, nil
}

// SetTerms states or restates a loan account's terms.
func (s *Service) SetTerms(ctx context.Context, spaceID uuid.UUID, t Terms) (Terms, error) {
	if err := t.Validate(); err != nil {
		return Terms{}, err
	}
	if _, err := s.loanAccount(ctx, spaceID, t.AccountID); err != nil {
		return Terms{}, err
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO loans (account_id, space_id, principal_minor, annual_rate, term_months, issued_on, kind)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (account_id) DO UPDATE SET principal_minor = EXCLUDED.principal_minor,
			annual_rate = EXCLUDED.annual_rate, term_months = EXCLUDED.term_months,
			issued_on = EXCLUDED.issued_on, kind = EXCLUDED.kind, updated_at = now()`,
		t.AccountID, spaceID, t.Principal, t.AnnualRate, t.TermMonths, t.IssuedOn, t.Kind)
	if err != nil {
		return Terms{}, fmt.Errorf("loan: set terms: %w", err)
	}
	return s.Terms(ctx, spaceID, t.AccountID)
}

// DeleteTerms forgets a loan's terms; the journal keeps what was paid.
func (s *Service) DeleteTerms(ctx context.Context, spaceID, accountID uuid.UUID) error {
	ct, err := s.db.Exec(ctx, `DELETE FROM loans WHERE space_id = $1 AND account_id = $2`, spaceID, accountID)
	if err != nil {
		return fmt.Errorf("loan: delete terms: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Payment is one payment on a loan from another account: the part that pays
// the debt down and the interest, in minor units of both accounts' currency.
type Payment struct {
	LoanAccountID uuid.UUID
	FromAccountID uuid.UUID
	OccurredOn    time.Time
	Principal     int64
	Interest      int64
}

// RecordPayment writes a payment the way Р-24 counts it: the interest is a
// spending from the paying account, under «Проценты по кредитам» when the
// family has it; the rest is a transfer to the loan's account, which brings the
// debt down. Should the transfer be refused, the interest is taken back.
func (s *Service) RecordPayment(ctx context.Context, spaceID uuid.UUID, p Payment) (interest, out, in *operation.Operation, err error) {
	loanAcc, err := s.loanAccount(ctx, spaceID, p.LoanAccountID)
	if err != nil {
		return nil, nil, nil, err
	}
	from, err := s.accounts.ByID(ctx, spaceID, p.FromAccountID)
	if err != nil {
		return nil, nil, nil, err
	}
	if from.Currency != loanAcc.Currency {
		return nil, nil, nil, fmt.Errorf("%w: the loan is in %s and the paying account in %s", family.ErrValidation, loanAcc.Currency, from.Currency)
	}
	if p.Principal < 0 || p.Interest < 0 || p.Principal+p.Interest == 0 {
		return nil, nil, nil, fmt.Errorf("%w: a payment repays some debt or interest, neither below zero", family.ErrValidation)
	}
	if p.Interest > 0 {
		op := operation.Operation{
			AccountID: p.FromAccountID, Type: operation.TypeWithdrawal, OccurredOn: p.OccurredOn,
			AmountMinor: -p.Interest, Currency: from.Currency, Counterparty: loanAcc.Name,
			CategoryID: s.interestCategory(ctx, spaceID),
		}
		created, err := s.journal.Create(ctx, spaceID, op)
		if err != nil {
			return nil, nil, nil, err
		}
		interest = &created
	}
	if p.Principal > 0 {
		o, i, err := s.journal.CreateMoneyTransfer(ctx, spaceID, operation.MoneyTransferParams{
			FromAccountID: p.FromAccountID, ToAccountID: p.LoanAccountID, OccurredOn: p.OccurredOn,
			AmountMinor: p.Principal, Currency: from.Currency,
		})
		if err != nil {
			if interest != nil {
				_ = s.journal.Delete(ctx, spaceID, interest.ID)
			}
			return nil, nil, nil, err
		}
		out, in = &o, &i
	}
	return interest, out, in, nil
}

// interestCategory is the family's active «Проценты по кредитам», or nil.
func (s *Service) interestCategory(ctx context.Context, spaceID uuid.UUID) *uuid.UUID {
	list, err := s.categories.List(ctx, spaceID)
	if err != nil {
		return nil
	}
	for _, c := range list {
		if c.Kind == category.KindExpense && c.Name == InterestCategory && !c.Archived {
			id := c.ID
			return &id
		}
	}
	return nil
}

// Prepay is debt paid ahead of the schedule from another account of the
// loan's currency.
type Prepay struct {
	LoanAccountID uuid.UUID
	FromAccountID uuid.UUID
	OccurredOn    time.Time
	Amount        int64
	Mode          Mode
}

// RecordPrepayment moves the money to the loan's account — a transfer, as the
// debt part of every payment — and notes it for the schedule. Should the
// note be refused, the transfer is taken back.
func (s *Service) RecordPrepayment(ctx context.Context, spaceID uuid.UUID, p Prepay) (Prepayment, error) {
	if p.Amount <= 0 {
		return Prepayment{}, fmt.Errorf("%w: a prepayment repays some debt", family.ErrValidation)
	}
	if p.Mode != Shorter && p.Mode != Lower {
		return Prepayment{}, fmt.Errorf("%w: a prepayment shortens the term or lowers the payment", family.ErrValidation)
	}
	if _, err := s.Terms(ctx, spaceID, p.LoanAccountID); err != nil {
		return Prepayment{}, err
	}
	loanAcc, err := s.loanAccount(ctx, spaceID, p.LoanAccountID)
	if err != nil {
		return Prepayment{}, err
	}
	from, err := s.accounts.ByID(ctx, spaceID, p.FromAccountID)
	if err != nil {
		return Prepayment{}, err
	}
	if from.Currency != loanAcc.Currency {
		return Prepayment{}, fmt.Errorf("%w: the loan is in %s and the paying account in %s", family.ErrValidation, loanAcc.Currency, from.Currency)
	}
	out, _, err := s.journal.CreateMoneyTransfer(ctx, spaceID, operation.MoneyTransferParams{
		FromAccountID: p.FromAccountID, ToAccountID: p.LoanAccountID, OccurredOn: p.OccurredOn,
		AmountMinor: p.Amount, Currency: from.Currency,
	})
	if err != nil {
		return Prepayment{}, err
	}
	pre := Prepayment{ID: uuid.New(), On: p.OccurredOn, Amount: p.Amount, Mode: p.Mode}
	_, err = s.db.Exec(ctx, `INSERT INTO loan_prepayments (id, account_id, space_id, paid_on, amount_minor, mode)
		VALUES ($1, $2, $3, $4, $5, $6)`, pre.ID, p.LoanAccountID, spaceID, pre.On, pre.Amount, pre.Mode)
	if err != nil {
		_ = s.journal.Delete(ctx, spaceID, out.ID)
		return Prepayment{}, fmt.Errorf("loan: prepayment: %w", err)
	}
	return pre, nil
}

// DeletePrepayment forgets a prepayment for the schedule; the money moved
// stays in the journal until deleted there.
func (s *Service) DeletePrepayment(ctx context.Context, spaceID, accountID, id uuid.UUID) error {
	ct, err := s.db.Exec(ctx, `DELETE FROM loan_prepayments WHERE space_id = $1 AND account_id = $2 AND id = $3`, spaceID, accountID, id)
	if err != nil {
		return fmt.Errorf("loan: delete prepayment: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
