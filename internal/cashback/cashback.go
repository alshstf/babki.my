// Package cashback is a card's bank loyalty programme (decision Р-58): the
// programme and variant of the catalog it is tied to, the categories the
// family picked for each period with the percents the bank offered, and what
// the period's purchases earn by them, told by their merchant codes.
package cashback

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/loyalty"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/db"
)

// Binding is a card's programme and variant.
type Binding struct {
	Programme, Variant string
}

// Pick is a category picked for a period, at the percent the bank offered.
type Pick struct {
	Category string
	Percent  decimal.Decimal
}

// Store keeps the bindings and the picks.
type Store struct{ db db.Executor }

func NewStore(x db.Executor) *Store { return &Store{db: x} }

var (
	errNotCard = fmt.Errorf("%w: a loyalty programme is a card's: a current account or a credit card in roubles", family.ErrValidation)
	errBinding = fmt.Errorf("%w: no such programme or variant in the catalog", family.ErrValidation)
	errPicks   = fmt.Errorf("%w: the picks are the edition's categories, each once, at a percent above 0 and up to 100 to hundredths, no more than the variant picks and one more", family.ErrValidation)
	hundred    = decimal.NewFromInt(100)
)

// Binding is the card's programme, nil without one.
func (s *Store) Binding(ctx context.Context, spaceID, accountID uuid.UUID) (*Binding, error) {
	var b Binding
	err := s.db.QueryRow(ctx, `SELECT programme_id, variant_id FROM account_loyalty WHERE space_id = $1 AND account_id = $2`,
		spaceID, accountID).Scan(&b.Programme, &b.Variant)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cashback: binding: %w", err)
	}
	return &b, nil
}

// Bindings are the space's cards' programmes by account.
func (s *Store) Bindings(ctx context.Context, spaceID uuid.UUID) (map[uuid.UUID]Binding, error) {
	rows, err := s.db.Query(ctx, `SELECT account_id, programme_id, variant_id FROM account_loyalty WHERE space_id = $1`, spaceID)
	if err != nil {
		return nil, fmt.Errorf("cashback: bindings: %w", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]Binding{}
	for rows.Next() {
		var id uuid.UUID
		var b Binding
		if err := rows.Scan(&id, &b.Programme, &b.Variant); err != nil {
			return nil, fmt.Errorf("cashback: bindings: %w", err)
		}
		out[id] = b
	}
	return out, rows.Err()
}

func (s *Store) setBinding(ctx context.Context, spaceID, accountID uuid.UUID, b Binding) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO account_loyalty (account_id, space_id, programme_id, variant_id) VALUES ($1, $2, $3, $4)
		ON CONFLICT (account_id) DO UPDATE SET programme_id = EXCLUDED.programme_id, variant_id = EXCLUDED.variant_id`,
		accountID, spaceID, b.Programme, b.Variant)
	if err != nil {
		return fmt.Errorf("cashback: set binding: %w", err)
	}
	return nil
}

// deleteBinding unties the card and forgets its picks; false without a
// programme.
func (s *Store) deleteBinding(ctx context.Context, spaceID, accountID uuid.UUID) (bool, error) {
	if _, err := s.db.Exec(ctx, `DELETE FROM account_loyalty_picks WHERE space_id = $1 AND account_id = $2`, spaceID, accountID); err != nil {
		return false, fmt.Errorf("cashback: delete binding: %w", err)
	}
	ct, err := s.db.Exec(ctx, `DELETE FROM account_loyalty WHERE space_id = $1 AND account_id = $2`, spaceID, accountID)
	if err != nil {
		return false, fmt.Errorf("cashback: delete binding: %w", err)
	}
	return ct.RowsAffected() > 0, nil
}

// Picks are the categories picked for the period from its first day.
func (s *Store) Picks(ctx context.Context, spaceID, accountID uuid.UUID, from time.Time) ([]Pick, error) {
	rows, err := s.db.Query(ctx, `
		SELECT category_id, percent FROM account_loyalty_picks
		WHERE space_id = $1 AND account_id = $2 AND period_from = $3 ORDER BY percent DESC, category_id`, spaceID, accountID, from)
	if err != nil {
		return nil, fmt.Errorf("cashback: picks: %w", err)
	}
	defer rows.Close()
	var out []Pick
	for rows.Next() {
		var p Pick
		if err := rows.Scan(&p.Category, &p.Percent); err != nil {
			return nil, fmt.Errorf("cashback: picks: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PeriodPick is a pick with the first day of its period.
type PeriodPick struct {
	From time.Time
	Pick
}

// AllPicks are the space's cards' picks by account, by period and percent.
func (s *Store) AllPicks(ctx context.Context, spaceID uuid.UUID) (map[uuid.UUID][]PeriodPick, error) {
	rows, err := s.db.Query(ctx, `
		SELECT account_id, period_from, category_id, percent FROM account_loyalty_picks
		WHERE space_id = $1 ORDER BY account_id, period_from, percent DESC, category_id`, spaceID)
	if err != nil {
		return nil, fmt.Errorf("cashback: all picks: %w", err)
	}
	defer rows.Close()
	out := map[uuid.UUID][]PeriodPick{}
	for rows.Next() {
		var id uuid.UUID
		var p PeriodPick
		if err := rows.Scan(&id, &p.From, &p.Category, &p.Percent); err != nil {
			return nil, fmt.Errorf("cashback: all picks: %w", err)
		}
		out[id] = append(out[id], p)
	}
	return out, rows.Err()
}

// setPicks puts picks in place of the period's.
func (s *Store) setPicks(ctx context.Context, spaceID, accountID uuid.UUID, from time.Time, picks []Pick) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM account_loyalty_picks WHERE space_id = $1 AND account_id = $2 AND period_from = $3`,
		spaceID, accountID, from); err != nil {
		return fmt.Errorf("cashback: set picks: %w", err)
	}
	for _, p := range picks {
		if _, err := tx.Exec(ctx, `INSERT INTO account_loyalty_picks (account_id, space_id, period_from, category_id, percent) VALUES ($1, $2, $3, $4, $5)`,
			accountID, spaceID, from, p.Category, p.Percent); err != nil {
			return fmt.Errorf("cashback: set picks: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// accounts and journals are what the service reads of the family's data.
type accounts interface {
	ByID(ctx context.Context, spaceID, id uuid.UUID) (account.WithBalance, error)
}

type journals interface {
	ListForEngine(ctx context.Context, spaceID, accountID uuid.UUID) ([]operation.Operation, error)
}

// Service ties cards to programmes and reckons what they earn.
type Service struct {
	store    *Store
	accounts accounts
	journals journals
	now      func() time.Time
}

func NewService(store *Store, a accounts, j journals) *Service {
	return &Service{store: store, accounts: a, journals: j, now: time.Now}
}

// card is the account if it can hold a programme: a rouble current account
// or credit card.
func (s *Service) card(ctx context.Context, spaceID, accountID uuid.UUID) (account.WithBalance, error) {
	a, err := s.accounts.ByID(ctx, spaceID, accountID)
	if err != nil {
		return account.WithBalance{}, err
	}
	if (a.Type != account.TypeChecking && a.Type != account.TypeCreditCard) || a.Currency != "RUB" {
		return account.WithBalance{}, errNotCard
	}
	return a, nil
}

// Bind ties the card to the catalog's programme and variant.
func (s *Service) Bind(ctx context.Context, spaceID, accountID uuid.UUID, b Binding) error {
	if _, err := s.card(ctx, spaceID, accountID); err != nil {
		return err
	}
	p, ok := loyalty.ByID(b.Programme)
	if !ok || !slices.ContainsFunc(p.Editions, func(e loyalty.Edition) bool {
		return slices.ContainsFunc(e.Variants, func(v loyalty.Variant) bool { return v.ID == b.Variant })
	}) {
		return errBinding
	}
	return s.store.setBinding(ctx, spaceID, accountID, b)
}

// Unbind unties the card; pgx.ErrNoRows without a programme.
func (s *Service) Unbind(ctx context.Context, spaceID, accountID uuid.UUID) error {
	if _, err := s.accounts.ByID(ctx, spaceID, accountID); err != nil {
		return err
	}
	found, err := s.store.deleteBinding(ctx, spaceID, accountID)
	if err == nil && !found {
		return pgx.ErrNoRows
	}
	return err
}

// SetPicks keeps the categories picked for the period day falls in: the
// edition's of the period's first day, each once, at a percent above 0 and
// up to 100 to hundredths; as many as the variant picks and one more — a bank
// may offer an extra one within the period (Т-Банк, Альфа).
func (s *Service) SetPicks(ctx context.Context, spaceID, accountID uuid.UUID, day time.Time, picks []Pick) error {
	if _, err := s.card(ctx, spaceID, accountID); err != nil {
		return err
	}
	b, err := s.store.Binding(ctx, spaceID, accountID)
	if err != nil {
		return err
	}
	if b == nil {
		return fmt.Errorf("%w: the card has no loyalty programme", family.ErrValidation)
	}
	e, v, ok := terms(*b, day)
	if !ok || e.Period == loyalty.PeriodNone {
		return fmt.Errorf("%w: the programme has no categories to pick then", family.ErrValidation)
	}
	from, _ := e.Period.PeriodOf(day)
	if len(picks) > v.Pick+1 {
		return errPicks
	}
	seen := map[string]bool{}
	for _, p := range picks {
		known := slices.ContainsFunc(e.Categories, func(c loyalty.Category) bool { return c.ID == p.Category })
		if !known || seen[p.Category] || !p.Percent.IsPositive() || p.Percent.GreaterThan(hundred) || !p.Percent.Equal(p.Percent.Truncate(2)) {
			return errPicks
		}
		seen[p.Category] = true
	}
	return s.store.setPicks(ctx, spaceID, accountID, from, picks)
}

// terms are the binding's edition and variant on day.
func terms(b Binding, day time.Time) (loyalty.Edition, loyalty.Variant, bool) {
	p, ok := loyalty.ByID(b.Programme)
	if !ok {
		return loyalty.Edition{}, loyalty.Variant{}, false
	}
	e, ok := p.EditionOn(day)
	if !ok {
		return loyalty.Edition{}, loyalty.Variant{}, false
	}
	i := slices.IndexFunc(e.Variants, func(v loyalty.Variant) bool { return v.ID == b.Variant })
	if i < 0 {
		return loyalty.Edition{}, loyalty.Variant{}, false
	}
	return e, e.Variants[i], true
}

// Line is what a picked category earned in the period: the purchases in it
// and the bonus at its percent.
type Line struct {
	Pick
	Name   string
	Spent  int64
	Bonus  int64
	Counts int
}

// Card is a card's programme as of a period: the binding and its terms, the
// period, the picks, and what the period's purchases earn by them. Uncoded
// are the period's purchases without a merchant code, which cannot be told
// into a category; Capped says the bank's cap cut the bonus.
type Card struct {
	Binding  *Binding
	Edition  loyalty.Edition
	Variant  loyalty.Variant
	Name     string
	Known    bool
	From, To time.Time
	Picks    []Pick
	Lines    []Line
	Bonus    int64
	Capped   bool
	Uncoded  int64
	Uncount  int
}

// CardOf is the card's programme for the period day falls in.
func (s *Service) CardOf(ctx context.Context, spaceID, accountID uuid.UUID, day time.Time) (Card, error) {
	if _, err := s.card(ctx, spaceID, accountID); err != nil {
		return Card{}, err
	}
	b, err := s.store.Binding(ctx, spaceID, accountID)
	if err != nil || b == nil {
		return Card{}, err
	}
	out := Card{Binding: b}
	p, _ := loyalty.ByID(b.Programme)
	out.Name = p.Name
	e, v, ok := terms(*b, day)
	if !ok {
		return out, nil
	}
	out.Edition, out.Variant, out.Known = e, v, true
	out.From, out.To = e.Period.PeriodOf(day)
	if out.Picks, err = s.store.Picks(ctx, spaceID, accountID, out.From); err != nil {
		return Card{}, err
	}
	ops, err := s.journals.ListForEngine(ctx, spaceID, accountID)
	if err != nil {
		return Card{}, err
	}
	out.reckon(ops)
	return out, nil
}

// reckon tells each purchase of the period into the picked category of its
// code that pays the most, sums each category's purchases and bonus, and
// holds the total to the variant's cap. A bonus is a percent of a category's
// sum, to the kopeck down — banks count it so or by the purchase, and round
// to the rouble at most: the figure is an estimate.
func (c *Card) reckon(ops []operation.Operation) {
	names := map[string]string{}
	for _, cat := range c.Edition.Categories {
		names[cat.ID] = cat.Name
	}
	lines := map[string]*Line{}
	for _, p := range c.Picks {
		lines[p.Category] = &Line{Pick: p, Name: names[p.Category]}
	}
	for _, o := range ops {
		if o.Type != operation.TypeWithdrawal || o.AmountMinor >= 0 || o.OccurredOn.Before(c.From) || o.OccurredOn.After(c.To) {
			continue
		}
		spent := -o.AmountMinor
		if o.MCC == "" {
			c.Uncoded += spent
			c.Uncount++
			continue
		}
		var best *Line
		for _, cat := range c.Edition.CategoriesOf(o.MCC) {
			if l, ok := lines[cat.ID]; ok && (best == nil || l.Percent.GreaterThan(best.Percent)) {
				best = l
			}
		}
		if best != nil {
			best.Spent += spent
			best.Counts++
		}
	}
	for _, p := range c.Picks {
		l := lines[p.Category]
		l.Bonus = decimal.NewFromInt(l.Spent).Mul(l.Percent).Div(hundred).Floor().IntPart()
		c.Bonus += l.Bonus
		c.Lines = append(c.Lines, *l)
	}
	if c.Variant.Cap > 0 && c.Bonus > c.Variant.Cap {
		c.Bonus, c.Capped = c.Variant.Cap, true
	}
}

// Due are the space's cards whose programme lets their holder pick
// categories for the period today falls in, and none are said for it yet.
func (s *Service) Due(ctx context.Context, spaceID uuid.UUID, today time.Time) ([]DueCard, error) {
	bindings, err := s.store.Bindings(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	var out []DueCard
	for id, b := range bindings {
		e, _, ok := terms(b, today)
		if !ok || e.Period == loyalty.PeriodNone {
			continue
		}
		from, to := e.Period.PeriodOf(today)
		picks, err := s.store.Picks(ctx, spaceID, id, from)
		if err != nil {
			return nil, err
		}
		if len(picks) > 0 {
			continue
		}
		a, err := s.accounts.ByID(ctx, spaceID, id)
		if err != nil {
			return nil, err
		}
		if a.Status != account.StatusActive {
			continue
		}
		out = append(out, DueCard{AccountID: id, Name: a.Name, Owner: a.OwnerUserID, From: from, To: to})
	}
	slices.SortFunc(out, func(x, y DueCard) int { return strings.Compare(x.Name, y.Name) })
	return out, nil
}

// DueCard is a card waiting for its period's picks.
type DueCard struct {
	AccountID uuid.UUID
	Name      string
	Owner     *uuid.UUID
	From, To  time.Time
}
