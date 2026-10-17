package account

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/deposit"
	"babki.my/babki/internal/family"
)

// ConditionsMode is how a programme's conditions are told (decision Р-53):
// by the family's marks, or by the program from its own data where it can.
type ConditionsMode string

const (
	ConditionsManual ConditionsMode = "manual"
	ConditionsAuto   ConditionsMode = "auto"
)

// Binding is an account's programme of its bank in the catalog: the variant
// chosen at opening, the day it was opened or last prolonged, a deposit's
// term in the bank's unit and the sum it was opened with, and how its
// interest is paid where the variant allows more than one way (nil: its only
// way, or not said).
type Binding struct {
	Programme  string
	Variant    string
	OpenedOn   time.Time
	Term       *int
	Amount     *int64
	Conditions ConditionsMode
	Payout     *deposit.Payout
}

// Mark is the family's word on a condition: from From on it holds at Level
// (a bonus's step), or — Met false — no longer holds.
type Mark struct {
	Condition string
	From      time.Time
	Level     int
	Met       bool
}

var errBinding = fmt.Errorf("%w: the programme, its variant, the day of opening and — for a deposit — a term and a sum the bank has a rate for and a way of payment the variant has", family.ErrValidation)

// Binding is the account's programme, nil without one.
func (s *Store) Binding(ctx context.Context, spaceID, accountID uuid.UUID) (*Binding, error) {
	all, err := s.bindings(ctx, `WHERE space_id = $1 AND account_id = $2`, spaceID, accountID)
	if err != nil {
		return nil, err
	}
	b, ok := all[accountID]
	if !ok {
		return nil, nil
	}
	return &b, nil
}

// AllBindings are the space's accounts' programmes by account.
func (s *Store) AllBindings(ctx context.Context, spaceID uuid.UUID) (map[uuid.UUID]Binding, error) {
	return s.bindings(ctx, `WHERE space_id = $1`, spaceID)
}

func (s *Store) bindings(ctx context.Context, where string, args ...any) (map[uuid.UUID]Binding, error) {
	rows, err := s.db.Query(ctx, `SELECT account_id, programme, variant, opened_on, term, amount_minor, conditions, payout FROM account_programmes `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("account: programmes: %w", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]Binding{}
	for rows.Next() {
		var id uuid.UUID
		var b Binding
		if err := rows.Scan(&id, &b.Programme, &b.Variant, &b.OpenedOn, &b.Term, &b.Amount, &b.Conditions, &b.Payout); err != nil {
			return nil, fmt.Errorf("account: programmes: %w", err)
		}
		out[id] = b
	}
	return out, rows.Err()
}

// SetBinding ties the account to a programme of the catalog, over the one it
// had. Refused for an account that holds no rate, a programme of the other
// kind, a variant in another currency, or terms the catalog has no rate for
// on the day of opening.
func (s *Store) SetBinding(ctx context.Context, spaceID, accountID uuid.UUID, b Binding) error {
	a, err := s.ByID(ctx, spaceID, accountID)
	if err != nil {
		return err
	}
	if !Earns(a.Type) {
		return errEarns
	}
	p, ok := deposit.ByID(b.Programme)
	if !ok || (a.Type == TypeDeposit) != (p.Kind == deposit.KindDeposit) || b.OpenedOn.IsZero() {
		return errBinding
	}
	if b.Conditions != ConditionsManual && b.Conditions != ConditionsAuto {
		return errBinding
	}
	if money, ok := p.VariantMoney(b.Variant); !ok || money != a.Currency {
		return errBinding
	}
	if p.Kind == deposit.KindDeposit {
		if b.Term == nil || b.Amount == nil || b.Payout != nil && !p.Pays(b.Variant, b.OpenedOn, *b.Payout) {
			return errBinding
		}
		// A deposit takes the edition of its day of opening: there must be
		// a rate for its term and sum in it, the key rate aside.
		h := deposit.Holding{Variant: b.Variant, OpenedOn: b.OpenedOn, Term: *b.Term, Amount: *b.Amount}
		anyKey := func(time.Time) (decimal.Decimal, bool, error) { return decimal.NewFromInt(10), true, nil }
		if _, err := p.Quote(h, b.OpenedOn, *b.Amount, anyKey, nil); err != nil {
			return errBinding
		}
	} else if !p.Admits(b.Variant, b.OpenedOn) {
		return errBinding
	}
	if p.Kind == deposit.KindSavings {
		b.Term, b.Amount, b.Payout = nil, nil, nil
	}
	_, err = s.db.Exec(ctx, `
		INSERT INTO account_programmes (account_id, space_id, programme, variant, opened_on, term, amount_minor, conditions, payout)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (account_id) DO UPDATE SET programme = EXCLUDED.programme, variant = EXCLUDED.variant,
			opened_on = EXCLUDED.opened_on, term = EXCLUDED.term, amount_minor = EXCLUDED.amount_minor,
			conditions = EXCLUDED.conditions, payout = EXCLUDED.payout, updated_at = now()`,
		accountID, spaceID, b.Programme, b.Variant, b.OpenedOn, b.Term, b.Amount, b.Conditions, b.Payout)
	if err != nil {
		return fmt.Errorf("account: set programme: %w", err)
	}
	return nil
}

// DeleteBinding unties the account from its programme; its marks go with it.
func (s *Store) DeleteBinding(ctx context.Context, spaceID, accountID uuid.UUID) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM account_programmes WHERE space_id = $1 AND account_id = $2`, spaceID, accountID)
	if err != nil {
		return fmt.Errorf("account: delete programme: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM account_condition_marks WHERE space_id = $1 AND account_id = $2`, spaceID, accountID); err != nil {
		return fmt.Errorf("account: delete marks: %w", err)
	}
	return nil
}

// Marks are the account's marks, by condition and day.
func (s *Store) Marks(ctx context.Context, spaceID, accountID uuid.UUID) ([]Mark, error) {
	all, err := s.marks(ctx, `WHERE space_id = $1 AND account_id = $2`, spaceID, accountID)
	return all[accountID], err
}

// AllMarks are the space's accounts' marks by account.
func (s *Store) AllMarks(ctx context.Context, spaceID uuid.UUID) (map[uuid.UUID][]Mark, error) {
	return s.marks(ctx, `WHERE space_id = $1`, spaceID)
}

func (s *Store) marks(ctx context.Context, where string, args ...any) (map[uuid.UUID][]Mark, error) {
	rows, err := s.db.Query(ctx, `SELECT account_id, condition, from_on, level, met FROM account_condition_marks `+where+` ORDER BY account_id, condition, from_on`, args...)
	if err != nil {
		return nil, fmt.Errorf("account: marks: %w", err)
	}
	defer rows.Close()
	out := map[uuid.UUID][]Mark{}
	for rows.Next() {
		var id uuid.UUID
		var m Mark
		if err := rows.Scan(&id, &m.Condition, &m.From, &m.Level, &m.Met); err != nil {
			return nil, fmt.Errorf("account: marks: %w", err)
		}
		out[id] = append(out[id], m)
	}
	return out, rows.Err()
}

var errMark = fmt.Errorf("%w: a mark is of a condition of the account's programme, with its day and a level it has", family.ErrValidation)

// SetMark records the family's word on a condition of the account's
// programme, over one of the same day.
func (s *Store) SetMark(ctx context.Context, spaceID, accountID uuid.UUID, m Mark) error {
	b, err := s.Binding(ctx, spaceID, accountID)
	if err != nil {
		return err
	}
	if b == nil {
		return errMark
	}
	p, ok := deposit.ByID(b.Programme)
	if !ok || m.From.IsZero() || m.Level < 0 {
		return errMark
	}
	levels, known := p.Conditions(b.Variant)[m.Condition]
	if !known || (m.Met && m.Level >= max(levels, 1)) {
		return errMark
	}
	_, err = s.db.Exec(ctx, `
		INSERT INTO account_condition_marks (account_id, space_id, condition, from_on, level, met) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (account_id, condition, from_on) DO UPDATE SET level = EXCLUDED.level, met = EXCLUDED.met`,
		accountID, spaceID, m.Condition, m.From, m.Level, m.Met)
	if err != nil {
		return fmt.Errorf("account: set mark: %w", err)
	}
	return nil
}

// DeleteMark forgets the mark of a condition made on day.
func (s *Store) DeleteMark(ctx context.Context, spaceID, accountID uuid.UUID, condition string, day time.Time) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM account_condition_marks WHERE space_id = $1 AND account_id = $2 AND condition = $3 AND from_on = $4`,
		spaceID, accountID, condition, day)
	if err != nil {
		return fmt.Errorf("account: delete mark: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// MarksSay is what the marks say of a condition in a month: the last mark
// made in it or before.
func MarksSay(marks []Mark) deposit.Conditions {
	return func(id string, month time.Time) (int, bool) {
		next := time.Date(month.Year(), month.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		var last *Mark
		for i := range marks {
			if marks[i].Condition == id && marks[i].From.Before(next) && (last == nil || !marks[i].From.Before(last.From)) {
				last = &marks[i]
			}
		}
		if last == nil || !last.Met {
			return 0, false
		}
		return last.Level, true
	}
}

// Measure is what the program's own data say of a bank in a month, for the
// conditions told automatically (Р-53): purchases on its cards, wages paid
// into it, the balance kept in it, in minor units; known is false when there
// is nothing to tell by — no operations of its cards that month.
type Measure func(bank string, check deposit.Check, month time.Time) (value int64, known bool)

// Told is how a condition stands in a month and what told it: the family's
// mark, the program's data — Value measured in Month, the month before an
// unfinished one — or nothing.
type Told struct {
	Met   bool
	Level int
	By    string // "mark", "data" or "none"
	Value int64
	Month time.Time
}

// Tell is how the account's conditions stand in month: a mark made in it or
// before stands over everything; in the automatic mode a bonus the program
// can measure is told by its data — an unfinished month by the one before,
// as the bank pays for a whole month.
func Tell(b Binding, p deposit.Programme, marks []Mark, measure Measure, today time.Time) func(id string, month time.Time) Told {
	say := MarksSay(marks)
	thisMonth := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	return func(id string, month time.Time) Told {
		if hasMark(marks, id, month) {
			level, ok := say(id, month)
			return Told{Met: ok, Level: level, By: "mark"}
		}
		bonus, ok := p.Bonus(b.Variant, id)
		if b.Conditions != ConditionsAuto || measure == nil || !ok || bonus.Check == deposit.CheckManual {
			return Told{By: "none"}
		}
		if !month.Before(thisMonth) {
			month = thisMonth.AddDate(0, -1, 0)
		}
		value, known := measure(p.Bank, bonus.Check, month)
		if !known {
			return Told{By: "none", Month: month}
		}
		out := Told{By: "data", Value: value, Month: month}
		if len(bonus.Levels) == 0 {
			out.Met = value > 0
			return out
		}
		for i, l := range bonus.Levels {
			if value >= l.From {
				out.Met, out.Level = true, i
			}
		}
		return out
	}
}

// hasMark says a mark of the condition was made in month or before.
func hasMark(marks []Mark, id string, month time.Time) bool {
	next := time.Date(month.Year(), month.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	for _, m := range marks {
		if m.Condition == id && m.From.Before(next) {
			return true
		}
	}
	return false
}

// RateSource is what an account's rate is reckoned from: the rates written
// in by hand, the programme, the marks of its conditions and — in the
// automatic mode — the program's own data.
type RateSource struct {
	Rates   []Rate
	Binding *Binding
	Marks   []Mark
	Measure Measure
}

// Resolved is an account's rate on a day: a percent a year, and where it
// came from — a rate written in by hand (Rate, over everything, Р-51), or
// the programme's terms (Quote).
type Resolved struct {
	Percent decimal.Decimal
	Rate    *Rate
	Quote   *deposit.Quote
}

// ByHand says the rate was written in by hand.
func (r Resolved) ByHand() bool { return r.Rate != nil && r.Rate.ByHand }

// Resolve is the account's rate on day with balance on it: the rate written
// in by hand if one is in force, else the programme's; ok is false without
// either. A programme whose terms the catalog cannot reckon for the day (a
// term it has no rate for, an edition not yet known, no key rate) gives
// none, its error aside. Its conditions are told as of today: a month past by
// its own data, this month and those ahead by the last one finished — a day
// ahead is not told by a month with no data yet.
func Resolve(src RateSource, day, today time.Time, balance int64, keyOn func(time.Time) (decimal.Decimal, bool, error)) (Resolved, bool, error) {
	pct, row, ok, err := RateOn(src.Rates, day, keyOn)
	if err != nil {
		return Resolved{}, false, err
	}
	if ok {
		return Resolved{Percent: pct, Rate: row}, true, nil
	}
	if src.Binding == nil {
		return Resolved{}, false, nil
	}
	p, found := deposit.ByID(src.Binding.Programme)
	if !found {
		return Resolved{}, false, nil
	}
	h := deposit.Holding{Variant: src.Binding.Variant, OpenedOn: src.Binding.OpenedOn}
	if src.Binding.Term != nil {
		h.Term = *src.Binding.Term
	}
	if src.Binding.Amount != nil {
		h.Amount = *src.Binding.Amount
	}
	tell := Tell(*src.Binding, p, src.Marks, src.Measure, today)
	q, err := p.Quote(h, day, balance, keyOn, func(id string, month time.Time) (int, bool) {
		t := tell(id, month)
		return t.Level, t.Met
	})
	if err != nil {
		if errors.Is(err, deposit.ErrNoEdition) || errors.Is(err, deposit.ErrNoCell) || errors.Is(err, deposit.ErrNoVariant) || errors.Is(err, deposit.ErrNoKeyRate) || errors.Is(err, deposit.ErrOpened) {
			return Resolved{}, false, nil
		}
		return Resolved{}, false, err
	}
	return Resolved{Percent: q.Percent, Quote: &q}, true, nil
}
