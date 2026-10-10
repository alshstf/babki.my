package budget

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"babki.my/babki/internal/cashflow"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/money"
)

// ErrNotFound is a limit that is not there: a 404.
var ErrNotFound = fmt.Errorf("budget: %w", pgx.ErrNoRows)

type reports interface {
	Report(ctx context.Context, spaceID uuid.UUID, from, to time.Time, whose cashflow.Whose) (cashflow.Report, error)
}

type categories interface {
	List(ctx context.Context, spaceID uuid.UUID) ([]category.Category, error)
}

// Service keeps the limits and works out a month's budget.
type Service struct {
	db         db.Executor
	reports    reports
	categories categories
}

func NewService(x db.Executor, r reports, cats categories) *Service {
	return &Service{db: x, reports: r, categories: cats}
}

// Limits are the space's limits, by category and month.
func (s *Service) Limits(ctx context.Context, spaceID uuid.UUID) ([]Limit, error) {
	rows, err := s.db.Query(ctx, `
		SELECT category_id, from_month, amount_minor, rollover FROM budget_limits
		WHERE space_id = $1 ORDER BY category_id, from_month`, spaceID)
	if err != nil {
		return nil, fmt.Errorf("budget: limits: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Limit, error) {
		var l Limit
		return l, row.Scan(&l.CategoryID, &l.From, &l.Amount, &l.Rollover)
	})
}

// SetLimit states a category's limit from a month on, over one stated from the
// same month.
func (s *Service) SetLimit(ctx context.Context, spaceID uuid.UUID, l Limit) error {
	switch {
	case l.Amount < 0 || l.Amount > money.MaxAmountMinor:
		return fmt.Errorf("%w: a limit is 0 or more, within the amounts the program keeps", family.ErrValidation)
	case !l.From.Equal(firstOfMonth(l.From)):
		return fmt.Errorf("%w: a limit runs from a month's first day", family.ErrValidation)
	}
	if err := s.spending(ctx, spaceID, l.CategoryID); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO budget_limits (space_id, category_id, from_month, amount_minor, rollover)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (space_id, category_id, from_month) DO UPDATE SET amount_minor = EXCLUDED.amount_minor,
			rollover = EXCLUDED.rollover, updated_at = now()`,
		spaceID, l.CategoryID, l.From, l.Amount, l.Rollover)
	if err != nil {
		return fmt.Errorf("budget: set limit: %w", err)
	}
	return nil
}

// DeleteLimit takes back a limit stated from a month: the one before it stands
// again.
func (s *Service) DeleteLimit(ctx context.Context, spaceID, categoryID uuid.UUID, from time.Time) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM budget_limits WHERE space_id = $1 AND category_id = $2 AND from_month = $3`,
		spaceID, categoryID, from)
	if err != nil {
		return fmt.Errorf("budget: delete limit: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// spending refuses a category that is not one of the family's spending ones.
func (s *Service) spending(ctx context.Context, spaceID, id uuid.UUID) error {
	list, err := s.categories.List(ctx, spaceID)
	if err != nil {
		return err
	}
	for _, c := range list {
		if c.ID == id && c.Kind == category.KindExpense {
			return nil
		}
	}
	return fmt.Errorf("%w: a limit is a spending category's of the family", family.ErrValidation)
}

// Month is the budget of the month m falls in.
func (s *Service) Month(ctx context.Context, spaceID uuid.UUID, m time.Time) (Month, error) {
	m = firstOfMonth(m)
	limits, err := s.Limits(ctx, spaceID)
	if err != nil {
		return Month{}, err
	}
	byCategory := map[uuid.UUID][]Limit{}
	from := m
	for _, l := range limits {
		byCategory[l.CategoryID] = append(byCategory[l.CategoryID], l)
		// A копилка fills from its first month: the report reaches back to it,
		// as far as a report goes.
		if l.Rollover && l.From.Before(from) {
			from = l.From
		}
	}
	if earliest := m.AddDate(0, 1-cashflow.MaxMonths, 0); from.Before(earliest) {
		from = earliest
	}
	r, err := s.reports.Report(ctx, spaceID, from, m.AddDate(0, 1, -1), cashflow.Whose{})
	if err != nil {
		return Month{}, err
	}
	cats, err := s.categories.List(ctx, spaceID)
	if err != nil {
		return Month{}, err
	}
	return assemble(m, r, cats, byCategory), nil
}

// assemble lays out the month m from the report up to it: each limited
// category's line in the categories' order, and the sums.
func assemble(m time.Time, r cashflow.Report, cats []category.Category, limits map[uuid.UUID][]Limit) Month {
	out := Month{Month: m, BaseCurrency: r.BaseCurrency, Lines: []Line{}, MissingRates: r.MissingRates}
	spent := map[uuid.UUID][]int64{}
	var walk func(lines []cashflow.Line)
	walk = func(lines []cashflow.Line) {
		for _, l := range lines {
			if l.CategoryID != nil {
				spent[*l.CategoryID] = l.ByMonth
			}
			walk(l.Children)
		}
	}
	walk(r.Expense.Lines)
	last := len(r.Months) - 1
	zero := make([]int64, len(r.Months))

	parent := map[uuid.UUID]uuid.UUID{}
	for _, c := range cats {
		if c.ParentID != nil {
			parent[c.ID] = *c.ParentID
		}
	}
	limited := map[uuid.UUID]bool{}
	var counted int64
	for _, c := range ordered(cats) {
		byMonth, ok := spent[c.ID]
		if !ok {
			byMonth = zero
		}
		l, ok := line(c.ID, limits[c.ID], r.Months, byMonth, m)
		if !ok {
			continue
		}
		limited[c.ID] = true
		out.Lines = append(out.Lines, l)
		// A subcategory's spending is in its limited parent's already.
		if p, ok := parent[c.ID]; ok && limited[p] {
			continue
		}
		out.Planned += l.Carried + l.Limit
		out.Spent += l.Spent
		counted += l.Spent
	}
	out.Left = out.Planned - out.Spent
	if last >= 0 {
		out.Unlimited = r.Expense.ByMonth[last] - counted
	}
	return out
}

// ordered is the categories in the order the money report keeps: each top
// one by its position, its subcategories right after it.
func ordered(cats []category.Category) []category.Category {
	children := map[uuid.UUID][]category.Category{}
	var top []category.Category
	for _, c := range cats {
		if c.ParentID == nil {
			top = append(top, c)
		} else {
			children[*c.ParentID] = append(children[*c.ParentID], c)
		}
	}
	sortByPosition(top)
	out := make([]category.Category, 0, len(cats))
	for _, c := range top {
		out = append(out, c)
		kids := children[c.ID]
		sortByPosition(kids)
		out = append(out, kids...)
	}
	return out
}

func sortByPosition(cs []category.Category) {
	slices.SortStableFunc(cs, func(a, b category.Category) int { return cmp.Compare(a.Position, b.Position) })
}
