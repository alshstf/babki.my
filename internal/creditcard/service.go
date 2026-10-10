package creditcard

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	CounterpartAccounts(ctx context.Context, spaceID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]uuid.UUID, error)
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
	k.Earns = map[uuid.UUID]decimal.Decimal{}
	for _, c := range t.Cashback.Categories {
		k.Earns[c.CategoryID] = c.Percent
	}
	for child, parent := range f.parent {
		if k.Transfers[parent] {
			k.Transfers[child] = true
		}
		// A subcategory's own percent stands over its parent's.
		if pct, ok := k.Earns[parent]; ok {
			if _, own := k.Earns[child]; !own {
				k.Earns[child] = pct
			}
		}
	}
	return k
}

// cashbackCategories checks that the categories are the family's spending
// ones; a category named twice keeps its last percent. Nil comes back empty.
func (s *Service) cashbackCategories(ctx context.Context, spaceID uuid.UUID, in []CategoryPercent) ([]CategoryPercent, error) {
	ids := make([]uuid.UUID, 0, len(in))
	last := map[uuid.UUID]CategoryPercent{}
	for _, c := range in {
		ids = append(ids, c.CategoryID)
		last[c.CategoryID] = c
	}
	ids, err := s.spendingCategories(ctx, spaceID, ids)
	if err != nil {
		return nil, err
	}
	out := make([]CategoryPercent, 0, len(ids))
	for _, id := range ids {
		out = append(out, last[id])
	}
	return out, nil
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
			return nil, fmt.Errorf("%w: a card's category is one of the family's spending categories", family.ErrValidation)
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
	grace_all_lost, pay_by_period_end, charges_in_full, transfer_categories, monthly_fee_minor, cash_free_minor,
	cash_fee_percent, cash_fee_fixed_minor, transfer_fee_percent, transfer_fee_fixed_minor, penalty_daily_percent,
	cashback_base_percent, cashback_categories, cashback_cap_minor, cashback_points, cashback_credit_days,
	grace_run_from, pay_day, min_round_up_minor, installment_months, installment_fee_percent, installment_fee_minor,
	missed_minimum_period`

func scan(row pgx.Row) (Terms, error) {
	var t Terms
	var own decimal.NullDecimal
	err := row.Scan(&t.AccountID, &t.Limit, &t.StatementDay, &t.PaymentDays, &t.GraceKind, &t.GraceDays,
		&t.MinPercent, &t.MinFloor, &t.AnnualRate, &own, &t.WindowMonths, &t.GraceMonths, &t.OpenedOn,
		&t.GraceAllLost, &t.PayByPeriodEnd, &t.ChargesInFull, &t.TransferCategories, &t.Fees.Monthly, &t.Fees.CashFree,
		&t.Fees.CashPercent, &t.Fees.CashFixed, &t.Fees.TransferPercent, &t.Fees.TransferFixed, &t.Fees.PenaltyDaily,
		&t.Cashback.BasePercent, &t.Cashback.Categories, &t.Cashback.MonthlyCap, &t.Cashback.Points, &t.Cashback.CreditDays,
		&t.RunFrom, &t.PayDay, &t.MinRoundUp, &t.Installment.Months, &t.Installment.MonthlyFeePercent, &t.Installment.Fee,
		&t.MissedMinimumPeriod)
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
	if t.GraceKind != Long && t.GraceKind != Running {
		t.GraceDays = 0
	}
	if t.GraceKind != Running {
		t.RunFrom = FromPurchase
	}
	if t.GraceKind != Windows {
		t.WindowMonths, t.GraceMonths, t.OpenedOn = 0, 0, nil
	}
	if t.TransferCategories, err = s.spendingCategories(ctx, spaceID, t.TransferCategories); err != nil {
		return Terms{}, err
	}
	if t.Cashback.Categories, err = s.cashbackCategories(ctx, spaceID, t.Cashback.Categories); err != nil {
		return Terms{}, err
	}
	var own decimal.NullDecimal
	if t.OwnRate != nil {
		own = decimal.NullDecimal{Decimal: *t.OwnRate, Valid: true}
	}
	_, err = s.db.Exec(ctx, `
		INSERT INTO credit_cards (account_id, space_id, limit_minor, statement_day, payment_days, grace_kind,
			grace_days, min_percent, min_floor_minor, annual_rate, own_rate, window_months, grace_months,
			opened_on, grace_all_lost, pay_by_period_end, charges_in_full, transfer_categories, monthly_fee_minor,
			cash_free_minor, cash_fee_percent, cash_fee_fixed_minor, transfer_fee_percent, transfer_fee_fixed_minor,
			penalty_daily_percent, cashback_base_percent, cashback_categories, cashback_cap_minor, cashback_points,
			cashback_credit_days, grace_run_from, pay_day, min_round_up_minor, installment_months,
			installment_fee_percent, installment_fee_minor, missed_minimum_period)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21,
			$22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37)
		ON CONFLICT (account_id) DO UPDATE SET limit_minor = EXCLUDED.limit_minor,
			statement_day = EXCLUDED.statement_day, payment_days = EXCLUDED.payment_days,
			grace_kind = EXCLUDED.grace_kind, grace_days = EXCLUDED.grace_days, min_percent = EXCLUDED.min_percent,
			min_floor_minor = EXCLUDED.min_floor_minor, annual_rate = EXCLUDED.annual_rate,
			own_rate = EXCLUDED.own_rate, window_months = EXCLUDED.window_months,
			grace_months = EXCLUDED.grace_months, opened_on = EXCLUDED.opened_on,
			grace_all_lost = EXCLUDED.grace_all_lost, pay_by_period_end = EXCLUDED.pay_by_period_end,
			charges_in_full = EXCLUDED.charges_in_full, transfer_categories = EXCLUDED.transfer_categories,
			monthly_fee_minor = EXCLUDED.monthly_fee_minor, cash_free_minor = EXCLUDED.cash_free_minor,
			cash_fee_percent = EXCLUDED.cash_fee_percent, cash_fee_fixed_minor = EXCLUDED.cash_fee_fixed_minor,
			transfer_fee_percent = EXCLUDED.transfer_fee_percent,
			transfer_fee_fixed_minor = EXCLUDED.transfer_fee_fixed_minor,
			penalty_daily_percent = EXCLUDED.penalty_daily_percent,
			cashback_base_percent = EXCLUDED.cashback_base_percent, cashback_categories = EXCLUDED.cashback_categories,
			cashback_cap_minor = EXCLUDED.cashback_cap_minor, cashback_points = EXCLUDED.cashback_points,
			cashback_credit_days = EXCLUDED.cashback_credit_days, grace_run_from = EXCLUDED.grace_run_from,
			pay_day = EXCLUDED.pay_day, min_round_up_minor = EXCLUDED.min_round_up_minor,
			installment_months = EXCLUDED.installment_months, installment_fee_percent = EXCLUDED.installment_fee_percent,
			installment_fee_minor = EXCLUDED.installment_fee_minor,
			missed_minimum_period = EXCLUDED.missed_minimum_period, updated_at = now()`,
		t.AccountID, spaceID, t.Limit, t.StatementDay, t.PaymentDays, t.GraceKind, t.GraceDays,
		t.MinPercent, t.MinFloor, t.AnnualRate, own, t.WindowMonths, t.GraceMonths, t.OpenedOn,
		t.GraceAllLost, t.PayByPeriodEnd, t.ChargesInFull, t.TransferCategories, t.Fees.Monthly, t.Fees.CashFree,
		t.Fees.CashPercent, t.Fees.CashFixed, t.Fees.TransferPercent, t.Fees.TransferFixed, t.Fees.PenaltyDaily,
		t.Cashback.BasePercent, t.Cashback.Categories, t.Cashback.MonthlyCap, t.Cashback.Points, t.Cashback.CreditDays,
		t.RunFrom, t.PayDay, t.MinRoundUp, t.Installment.Months, t.Installment.MonthlyFeePercent, t.Installment.Fee,
		t.MissedMinimumPeriod)
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
	b.Pending = c.Status.NonGraceInterest + c.Status.Penalty
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
	bank, err := s.BankFigures(ctx, spaceID, a.ID)
	if err != nil {
		return Card{}, nil, err
	}
	if !account.CountedByJournal(a.Account) {
		var balance int64
		if a.Balance != nil {
			balance = a.Balance.AmountMinor
		}
		st := ByBalance(t, balance, today)
		st.WithBank(bank, nil, a.Currency, today)
		return Card{Account: a, Terms: t, Status: st}, nil, nil
	}
	ops, err := s.journal.ListForEngine(ctx, spaceID, a.ID)
	if err != nil {
		return Card{}, nil, err
	}
	if k.Cash, err = s.cashOut(ctx, spaceID, ops); err != nil {
		return Card{}, nil, err
	}
	if k.Installments, err = s.Installments(ctx, spaceID, a.ID); err != nil {
		return Card{}, nil, err
	}
	st := Work(t, ops, a.Currency, today, k)
	st.WithBank(bank, ops, a.Currency, today)
	return Card{Account: a, Terms: t, Status: st, ByJournal: true}, ops, nil
}

// BankFigures are what the bank last said is due on the card; nil when
// nothing is stated.
func (s *Service) BankFigures(ctx context.Context, spaceID, accountID uuid.UUID) (*BankFigures, error) {
	var b BankFigures
	var grace, minimum *int64
	var graceOn, minimumOn *time.Time
	err := s.db.QueryRow(ctx, `SELECT stated_on, grace_minor, grace_on, minimum_minor, minimum_on
		FROM card_bank_figures WHERE space_id = $1 AND account_id = $2`, spaceID, accountID).
		Scan(&b.StatedOn, &grace, &graceOn, &minimum, &minimumOn)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("credit card: bank figures: %w", err)
	}
	if grace != nil && graceOn != nil {
		b.Grace = &Due{On: *graceOn, Amount: *grace}
	}
	if minimum != nil && minimumOn != nil {
		b.Minimum = &Due{On: *minimumOn, Amount: *minimum}
	}
	return &b, nil
}

// SetBankFigures states what the bank says is due on the card, over what
// it said before.
func (s *Service) SetBankFigures(ctx context.Context, spaceID, accountID uuid.UUID, b BankFigures) error {
	if err := b.Validate(s.today()); err != nil {
		return err
	}
	if _, err := s.Terms(ctx, spaceID, accountID); err != nil {
		return err
	}
	var grace, minimum *int64
	var graceOn, minimumOn *time.Time
	if b.Grace != nil {
		grace, graceOn = &b.Grace.Amount, &b.Grace.On
	}
	if b.Minimum != nil {
		minimum, minimumOn = &b.Minimum.Amount, &b.Minimum.On
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO card_bank_figures (account_id, space_id, stated_on, grace_minor, grace_on, minimum_minor, minimum_on)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (account_id) DO UPDATE SET stated_on = EXCLUDED.stated_on, grace_minor = EXCLUDED.grace_minor,
			grace_on = EXCLUDED.grace_on, minimum_minor = EXCLUDED.minimum_minor, minimum_on = EXCLUDED.minimum_on,
			updated_at = now()`,
		accountID, spaceID, b.StatedOn, grace, graceOn, minimum, minimumOn)
	if err != nil {
		return fmt.Errorf("credit card: set bank figures: %w", err)
	}
	return nil
}

// DeleteBankFigures forgets what the bank said.
func (s *Service) DeleteBankFigures(ctx context.Context, spaceID, accountID uuid.UUID) error {
	ct, err := s.db.Exec(ctx, `DELETE FROM card_bank_figures WHERE space_id = $1 AND account_id = $2`, spaceID, accountID)
	if err != nil {
		return fmt.Errorf("credit card: delete bank figures: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("credit card: bank figures: %w", pgx.ErrNoRows)
	}
	return nil
}

// cashOut is the card's rows that took cash out: money moved to one of the
// family's cash accounts.
func (s *Service) cashOut(ctx context.Context, spaceID uuid.UUID, ops []operation.Operation) (map[uuid.UUID]bool, error) {
	var moved []uuid.UUID
	for _, op := range ops {
		if op.TransferGroupID != nil && op.AmountMinor < 0 {
			moved = append(moved, op.ID)
		}
	}
	out := map[uuid.UUID]bool{}
	if len(moved) == 0 {
		return out, nil
	}
	peers, err := s.journal.CounterpartAccounts(ctx, spaceID, moved)
	if err != nil {
		return nil, err
	}
	list, err := s.accounts.ListWithBalance(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	cash := map[uuid.UUID]bool{}
	for _, a := range list {
		if a.Type == account.TypeCash {
			cash[a.ID] = true
		}
	}
	for op, peer := range peers {
		if cash[peer] {
			out[op] = true
		}
	}
	return out, nil
}

// Installments are the card's purchases in installments by their row.
func (s *Service) Installments(ctx context.Context, spaceID, accountID uuid.UUID) (map[uuid.UUID]Plan, error) {
	rows, err := s.db.Query(ctx, `SELECT operation_id, months, monthly_fee_percent, fee_minor FROM card_installments
		WHERE space_id = $1 AND account_id = $2`, spaceID, accountID)
	if err != nil {
		return nil, fmt.Errorf("credit card: installments: %w", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]Plan{}
	for rows.Next() {
		var id uuid.UUID
		var p Plan
		if err := rows.Scan(&id, &p.Months, &p.MonthlyFeePercent, &p.Fee); err != nil {
			return nil, fmt.Errorf("credit card: installments: %w", err)
		}
		out[id] = p
	}
	return out, rows.Err()
}

// SetInstallment puts a purchase on the card in installments, or restates
// its plan. The purchase is a spending of the card's own journal.
func (s *Service) SetInstallment(ctx context.Context, spaceID, accountID, operationID uuid.UUID, p Plan) error {
	if p.Months < 1 {
		return fmt.Errorf("%w: installments are 1 to 60 months", family.ErrValidation)
	}
	if err := p.Validate(); err != nil {
		return err
	}
	if _, err := s.Terms(ctx, spaceID, accountID); err != nil {
		return err
	}
	ops, err := s.journal.ListForEngine(ctx, spaceID, accountID)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(ops, func(o operation.Operation) bool { return o.ID == operationID })
	if i < 0 {
		return fmt.Errorf("credit card: installment: %w", pgx.ErrNoRows)
	}
	if op := ops[i]; op.Type != operation.TypeWithdrawal || op.TransferGroupID != nil || op.AmountMinor >= 0 {
		return fmt.Errorf("%w: only a purchase on the card goes in installments", family.ErrValidation)
	}
	_, err = s.db.Exec(ctx, `
		INSERT INTO card_installments (operation_id, account_id, space_id, months, monthly_fee_percent, fee_minor)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (operation_id) DO UPDATE SET months = EXCLUDED.months,
			monthly_fee_percent = EXCLUDED.monthly_fee_percent, fee_minor = EXCLUDED.fee_minor`,
		operationID, accountID, spaceID, p.Months, p.MonthlyFeePercent, p.Fee)
	if err != nil {
		return fmt.Errorf("credit card: set installment: %w", err)
	}
	return nil
}

// DeleteInstallment takes a purchase out of installments.
func (s *Service) DeleteInstallment(ctx context.Context, spaceID, accountID, operationID uuid.UUID) error {
	ct, err := s.db.Exec(ctx, `DELETE FROM card_installments WHERE space_id = $1 AND account_id = $2 AND operation_id = $3`,
		spaceID, accountID, operationID)
	if err != nil {
		return fmt.Errorf("credit card: delete installment: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("credit card: installment: %w", pgx.ErrNoRows)
	}
	return nil
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
