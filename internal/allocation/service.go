package allocation

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/portfolio"
	"babki.my/babki/internal/structure"
)

type structures interface {
	Of(ctx context.Context, spaceID uuid.UUID, valuation structure.Valuation) (structure.Structure, error)
}

type accounts interface {
	ListWithBalance(ctx context.Context, spaceID uuid.UUID) ([]account.WithBalance, error)
}

type sales interface {
	ForSale(ctx context.Context, spaceID, accountID uuid.UUID) ([]portfolio.SaleLots, error)
}

type spaces interface {
	SpaceByID(ctx context.Context, id uuid.UUID) (family.Space, error)
}

// Service keeps the targets and sets the worth against them.
type Service struct {
	db         db.Executor
	structures structures
	accounts   accounts
	sales      sales
	spaces     spaces
}

func NewService(x db.Executor, s structures) *Service {
	return &Service{db: x, structures: s}
}

// WithSales lets the service estimate what a rebalance's sales would bring in
// tax: the accounts the papers are on, their lots, the space's residency.
func (s *Service) WithSales(a accounts, sl sales, sp spaces) *Service {
	s.accounts, s.sales, s.spaces = a, sl, sp
	return s
}

// Rebalancing is the worth against the targets, what bringing it to them by
// buying and selling with fresh new money does, and the tax its sales are
// estimated to bring.
func (s *Service) Rebalancing(ctx context.Context, spaceID uuid.UUID, valuation structure.Valuation, fresh int64) (View, []Move, Estimate, family.TaxRules, error) {
	v, err := s.View(ctx, spaceID, valuation)
	if err != nil {
		return View{}, nil, Estimate{}, family.TaxRules{}, err
	}
	moves, err := Rebalance(v, fresh)
	if err != nil {
		return View{}, nil, Estimate{}, family.TaxRules{}, err
	}
	sp, err := s.spaces.SpaceByID(ctx, spaceID)
	if err != nil {
		return View{}, nil, Estimate{}, family.TaxRules{}, err
	}
	var held []portfolio.SaleLots
	if slices.ContainsFunc(moves, func(m Move) bool { return m.Minor < 0 && paperKinds[structure.Kind(m.Key)] }) {
		list, err := s.accounts.ListWithBalance(ctx, spaceID)
		if err != nil {
			return View{}, nil, Estimate{}, family.TaxRules{}, err
		}
		for _, a := range list {
			if a.Status != account.StatusActive || !account.CountedByJournal(a.Account) {
				continue
			}
			got, err := s.sales.ForSale(ctx, spaceID, a.ID)
			if err != nil {
				return View{}, nil, Estimate{}, family.TaxRules{}, err
			}
			held = append(held, got...)
		}
	}
	rules := sp.CostBasisRules()
	return v, moves, Estimating(moves, held, rules, time.Now().UTC()), rules, nil
}

// Settings are the family's targets and mode.
type Settings struct {
	Mode       Mode
	Kinds      []Target
	Currencies []Target
}

// Settings are the space's targets, ModeTopUp until the family picks.
func (s *Service) Settings(ctx context.Context, spaceID uuid.UUID) (Settings, error) {
	out := Settings{Mode: ModeTopUp, Kinds: []Target{}, Currencies: []Target{}}
	err := s.db.QueryRow(ctx, `SELECT mode FROM allocation_settings WHERE space_id = $1`, spaceID).Scan(&out.Mode)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Settings{}, fmt.Errorf("allocation: mode: %w", err)
	}
	rows, err := s.db.Query(ctx, `SELECT dimension, key, percent FROM allocation_targets WHERE space_id = $1 ORDER BY key`, spaceID)
	if err != nil {
		return Settings{}, fmt.Errorf("allocation: targets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var dim Dimension
		var t Target
		if err := rows.Scan(&dim, &t.Key, &t.Percent); err != nil {
			return Settings{}, err
		}
		if dim == ByKind {
			out.Kinds = append(out.Kinds, t)
		} else {
			out.Currencies = append(out.Currencies, t)
		}
	}
	return out, rows.Err()
}

// View is the space's worth today, papers at valuation (decision Р-11), against
// its targets — as «Структура» shows it at the valuation picked there.
func (s *Service) View(ctx context.Context, spaceID uuid.UUID, valuation structure.Valuation) (View, error) {
	st, err := s.Settings(ctx, spaceID)
	if err != nil {
		return View{}, err
	}
	worth, err := s.structures.Of(ctx, spaceID, valuation)
	if err != nil {
		return View{}, err
	}
	return view(worth, st.Mode, st.Kinds, st.Currencies), nil
}

var currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)

// SetTargets states the targets of a dimension anew: none, or percents that
// add up to 100, each key once.
func (s *Service) SetTargets(ctx context.Context, spaceID uuid.UUID, dim Dimension, targets []Target) error {
	if err := validTargets(dim, targets); err != nil {
		return err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM allocation_targets WHERE space_id = $1 AND dimension = $2`, spaceID, dim); err != nil {
		return fmt.Errorf("allocation: clear targets: %w", err)
	}
	for _, t := range targets {
		if _, err := tx.Exec(ctx, `INSERT INTO allocation_targets (space_id, dimension, key, percent) VALUES ($1, $2, $3, $4)`,
			spaceID, dim, t.Key, t.Percent); err != nil {
			return fmt.Errorf("allocation: set target: %w", err)
		}
	}
	return tx.Commit(ctx)
}

func validTargets(dim Dimension, targets []Target) error {
	if dim != ByKind && dim != ByCurrency {
		return fmt.Errorf("%w: targets are by kind or by currency", family.ErrValidation)
	}
	seen := map[string]bool{}
	sum := decimal.Zero
	for _, t := range targets {
		switch {
		case dim == ByKind && !slices.Contains(structure.Kinds, structure.Kind(t.Key)):
			return fmt.Errorf("%w: no kind of asset %q", family.ErrValidation, t.Key)
		case dim == ByCurrency && !currencyCode.MatchString(t.Key):
			return fmt.Errorf("%w: a currency is three capital letters, not %q", family.ErrValidation, t.Key)
		case seen[t.Key]:
			return fmt.Errorf("%w: %q twice", family.ErrValidation, t.Key)
		case t.Percent.IsNegative() || t.Percent.GreaterThan(hundred) || !t.Percent.Equal(t.Percent.Round(2)):
			return fmt.Errorf("%w: a target is 0 to 100 percent, to hundredths", family.ErrValidation)
		}
		seen[t.Key] = true
		sum = sum.Add(t.Percent)
	}
	if len(targets) > 0 && !sum.Equal(hundred) {
		return fmt.Errorf("%w: the targets add up to %s%%, not 100%%", family.ErrValidation, sum.String())
	}
	return nil
}

// SetMode states how the family brings its worth to the targets.
func (s *Service) SetMode(ctx context.Context, spaceID uuid.UUID, mode Mode) error {
	if mode != ModeTopUp && mode != ModeRebalance {
		return fmt.Errorf("%w: the mode is topup or rebalance", family.ErrValidation)
	}
	_, err := s.db.Exec(ctx, `INSERT INTO allocation_settings (space_id, mode) VALUES ($1, $2)
		ON CONFLICT (space_id) DO UPDATE SET mode = EXCLUDED.mode`, spaceID, mode)
	if err != nil {
		return fmt.Errorf("allocation: set mode: %w", err)
	}
	return nil
}
