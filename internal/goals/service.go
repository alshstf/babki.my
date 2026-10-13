package goals

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/structure"
)

type structures interface {
	Of(ctx context.Context, spaceID uuid.UUID, valuation structure.Valuation) (structure.Structure, error)
}

type accounts interface {
	ByID(ctx context.Context, spaceID, id uuid.UUID) (account.WithBalance, error)
	ListWithBalance(ctx context.Context, spaceID uuid.UUID) ([]account.WithBalance, error)
}

// Service keeps the goals and values them.
type Service struct {
	db         db.Executor
	structures structures
	accounts   accounts
	now        func() time.Time
}

func NewService(x db.Executor, s structures, a accounts) *Service {
	return &Service{db: x, structures: s, accounts: a, now: time.Now}
}

// ErrNotFound: no such goal in the space.
var ErrNotFound = fmt.Errorf("goals: %w", pgx.ErrNoRows)

// MaxNameRunes bounds a goal's name.
const MaxNameRunes = 100

// Valued is a goal with its progress and what each fund comes to.
type Valued struct {
	Goal
	Progress   Progress
	FundValues map[uuid.UUID]int64
	// Overdrawn: the sums out of one account across the goals come to more
	// than it holds.
	Overdrawn []uuid.UUID
	// OnBroker: some of its money is on a brokerage account, where a
	// contribution is a top-up to invest (Р-37).
	OnBroker bool
}

// List is the space's goals, in their order, each against its target.
func (s *Service) List(ctx context.Context, spaceID uuid.UUID) ([]Valued, string, error) {
	list, err := s.load(ctx, spaceID)
	if err != nil {
		return nil, "", err
	}
	worth, err := s.structures.Of(ctx, spaceID, structure.Liquid)
	if err != nil {
		return nil, "", err
	}
	held := map[uuid.UUID]int64{}
	for _, a := range worth.ByAccount {
		if id, err := uuid.Parse(a.Key); err == nil {
			held[id] = a.Minor
		}
	}
	all, err := s.accounts.ListWithBalance(ctx, spaceID)
	if err != nil {
		return nil, "", err
	}
	broker := map[uuid.UUID]bool{}
	for _, a := range all {
		broker[a.ID] = a.Type == account.TypeBrokerage
	}
	parts := map[uuid.UUID]int64{}
	for _, g := range list {
		for _, f := range g.Funds {
			if f.Minor != nil {
				parts[f.Account] += *f.Minor
			}
		}
	}
	today := s.now().UTC()
	out := make([]Valued, 0, len(list))
	for _, g := range list {
		v := Valued{Goal: g, FundValues: map[uuid.UUID]int64{}}
		var saved int64
		for _, f := range g.Funds {
			value := max(held[f.Account], 0)
			if f.Minor != nil {
				value = min(*f.Minor, value)
			}
			v.FundValues[f.Account] = value
			v.OnBroker = v.OnBroker || broker[f.Account]
			saved += value
			if f.Minor != nil && parts[f.Account] > held[f.Account] {
				v.Overdrawn = append(v.Overdrawn, f.Account)
			}
		}
		v.Progress = progress(g, saved, today)
		out = append(out, v)
	}
	return out, worth.BaseCurrency, nil
}

// Reserve is what the goals set aside on an account: the whole of it, or
// Minor in the base currency.
type Reserve struct {
	Whole bool
	Minor int64
}

// SetAside is what the goals set aside on each account, a reached goal's
// too: its money is still for it until spent (the forecast's «можно
// потратить» leaves it out, Р-44).
func (s *Service) SetAside(ctx context.Context, spaceID uuid.UUID) (map[uuid.UUID]Reserve, error) {
	list, err := s.load(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID]Reserve{}
	for _, g := range list {
		for _, f := range g.Funds {
			r := out[f.Account]
			if f.Minor == nil {
				r.Whole = true
			} else {
				r.Minor += *f.Minor
			}
			out[f.Account] = r
		}
	}
	return out, nil
}

func (s *Service) load(ctx context.Context, spaceID uuid.UUID) ([]Goal, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, name, target_minor, target_on, monthly_minor, yield_percent, position
		FROM savings_goals WHERE space_id = $1 ORDER BY position, created_at`, spaceID)
	if err != nil {
		return nil, fmt.Errorf("goals: list: %w", err)
	}
	var out []Goal
	at := map[uuid.UUID]int{}
	for rows.Next() {
		var g Goal
		if err := rows.Scan(&g.ID, &g.Name, &g.Target, &g.TargetOn, &g.Monthly, &g.Yield, &g.Position); err != nil {
			rows.Close()
			return nil, fmt.Errorf("goals: list: %w", err)
		}
		at[g.ID] = len(out)
		out = append(out, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	funds, err := s.db.Query(ctx, `
		SELECT f.goal_id, f.account_id, f.amount_minor FROM savings_goal_funds f
		JOIN savings_goals g ON g.id = f.goal_id WHERE g.space_id = $1 ORDER BY f.account_id`, spaceID)
	if err != nil {
		return nil, fmt.Errorf("goals: funds: %w", err)
	}
	defer funds.Close()
	for funds.Next() {
		var goal uuid.UUID
		var f Fund
		if err := funds.Scan(&goal, &f.Account, &f.Minor); err != nil {
			return nil, fmt.Errorf("goals: funds: %w", err)
		}
		if i, ok := at[goal]; ok {
			out[i].Funds = append(out[i].Funds, f)
		}
	}
	return out, funds.Err()
}

// Save states a goal anew — a new one when g.ID is zero — with its funds.
func (s *Service) Save(ctx context.Context, spaceID uuid.UUID, g Goal) (uuid.UUID, error) {
	g.Name = strings.TrimSpace(g.Name)
	switch {
	case g.Name == "" || utf8.RuneCountInString(g.Name) > MaxNameRunes:
		return uuid.Nil, fmt.Errorf("%w: a goal's name is 1 to %d characters", family.ErrValidation, MaxNameRunes)
	case g.Target != nil && *g.Target <= 0, g.Monthly != nil && *g.Monthly <= 0:
		return uuid.Nil, fmt.Errorf("%w: a target and a contribution are above zero", family.ErrValidation)
	case g.Yield.IsNegative() || g.Yield.GreaterThanOrEqual(decimal.NewFromInt(100)) || !g.Yield.Equal(g.Yield.Round(2)):
		return uuid.Nil, fmt.Errorf("%w: a yield is 0 to below 100 percent, to hundredths", family.ErrValidation)
	}
	seen := map[uuid.UUID]bool{}
	for _, f := range g.Funds {
		if seen[f.Account] || (f.Minor != nil && *f.Minor <= 0) {
			return uuid.Nil, fmt.Errorf("%w: an account once in a goal, a sum out of it above zero", family.ErrValidation)
		}
		seen[f.Account] = true
		a, err := s.accounts.ByID(ctx, spaceID, f.Account)
		if err != nil {
			return uuid.Nil, err
		}
		if a.Type == account.TypeLoan || a.Type == account.TypeCreditCard {
			return uuid.Nil, fmt.Errorf("%w: a debt does not stand for a goal", family.ErrValidation)
		}
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	id := g.ID
	if id == uuid.Nil {
		err = tx.QueryRow(ctx, `
			INSERT INTO savings_goals (space_id, name, target_minor, target_on, monthly_minor, yield_percent, position)
			VALUES ($1, $2, $3, $4, $5, $6, (SELECT COALESCE(MAX(position), 0) + 1 FROM savings_goals WHERE space_id = $1))
			RETURNING id`, spaceID, g.Name, g.Target, g.TargetOn, g.Monthly, g.Yield).Scan(&id)
	} else {
		var ct interface{ RowsAffected() int64 }
		ct, err = tx.Exec(ctx, `
			UPDATE savings_goals SET name = $3, target_minor = $4, target_on = $5, monthly_minor = $6, yield_percent = $7
			WHERE space_id = $1 AND id = $2`, spaceID, id, g.Name, g.Target, g.TargetOn, g.Monthly, g.Yield)
		if err == nil && ct.RowsAffected() == 0 {
			err = ErrNotFound
		}
	}
	if err != nil {
		return uuid.Nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM savings_goal_funds WHERE goal_id = $1`, id); err != nil {
		return uuid.Nil, err
	}
	for _, f := range g.Funds {
		if _, err := tx.Exec(ctx, `INSERT INTO savings_goal_funds (goal_id, account_id, amount_minor) VALUES ($1, $2, $3)`,
			id, f.Account, f.Minor); err != nil {
			return uuid.Nil, err
		}
	}
	return id, tx.Commit(ctx)
}

// Delete takes a goal away; the money stays where it is.
func (s *Service) Delete(ctx context.Context, spaceID, id uuid.UUID) error {
	ct, err := s.db.Exec(ctx, `DELETE FROM savings_goals WHERE space_id = $1 AND id = $2`, spaceID, id)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
