package account

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
)

// RateKind is how a rate is struck: fixed, or the Central Bank's key rate on
// each day plus a margin (deposits tied to it).
type RateKind string

const (
	RateFixed RateKind = "fixed"
	RateKey   RateKind = "key"
)

// Rate is a savings account's or a deposit's rate from From on (decision
// Р-51 А): Percent a year when fixed, the margin over the key rate when tied
// to it. ByHand: the family wrote it in — over anything the program would
// learn of the bank by itself. Until, if set, is its last day (Р-59 А): after
// it the programme's terms hold again, till the next rate.
type Rate struct {
	From    time.Time
	Until   *time.Time
	Kind    RateKind
	Percent decimal.Decimal
	ByHand  bool
}

// Earns says an account of the type holds a rate: a savings account or a
// deposit.
func Earns(t Type) bool { return t == TypeSavings || t == TypeDeposit }

var (
	hundred  = decimal.NewFromInt(100)
	fifty    = decimal.NewFromInt(50)
	errRate  = fmt.Errorf("%w: a rate is fixed from 0 to under 100 %% a year, or the key rate plus a margin within ±50, to thousandths", family.ErrValidation)
	errUntil = fmt.Errorf("%w: a rate's last day is not before its first", family.ErrValidation)
	errEarns = fmt.Errorf("%w: only a savings account or a deposit holds a rate", family.ErrValidation)
)

// Rates are the account's rates, oldest first.
func (s *Store) Rates(ctx context.Context, spaceID, accountID uuid.UUID) ([]Rate, error) {
	all, err := s.rates(ctx, `WHERE space_id = $1 AND account_id = $2`, spaceID, accountID)
	if err != nil {
		return nil, err
	}
	return all[accountID], nil
}

// AllRates are the space's accounts' rates by account, each oldest first.
func (s *Store) AllRates(ctx context.Context, spaceID uuid.UUID) (map[uuid.UUID][]Rate, error) {
	return s.rates(ctx, `WHERE space_id = $1`, spaceID)
}

func (s *Store) rates(ctx context.Context, where string, args ...any) (map[uuid.UUID][]Rate, error) {
	rows, err := s.db.Query(ctx, `SELECT account_id, from_on, until_on, kind, percent, by_hand FROM account_rates `+where+` ORDER BY account_id, from_on`, args...)
	if err != nil {
		return nil, fmt.Errorf("account: rates: %w", err)
	}
	defer rows.Close()
	out := map[uuid.UUID][]Rate{}
	for rows.Next() {
		var id uuid.UUID
		var r Rate
		if err := rows.Scan(&id, &r.From, &r.Until, &r.Kind, &r.Percent, &r.ByHand); err != nil {
			return nil, fmt.Errorf("account: rates: %w", err)
		}
		out[id] = append(out[id], r)
	}
	return out, rows.Err()
}

// SetRate states the account's rate from its day, over one stated for the
// same day. Refused for an account that holds no rate.
func (s *Store) SetRate(ctx context.Context, spaceID, accountID uuid.UUID, r Rate) error {
	switch r.Kind {
	case RateFixed:
		if r.Percent.IsNegative() || !r.Percent.LessThan(hundred) {
			return errRate
		}
	case RateKey:
		if !r.Percent.Abs().LessThan(fifty) {
			return errRate
		}
	default:
		return errRate
	}
	if !r.Percent.Equal(r.Percent.Truncate(3)) || r.From.IsZero() {
		return errRate
	}
	if r.Until != nil && r.Until.Before(r.From) {
		return errUntil
	}
	a, err := s.ByID(ctx, spaceID, accountID)
	if err != nil {
		return err
	}
	if !Earns(a.Type) {
		return errEarns
	}
	_, err = s.db.Exec(ctx, `
		INSERT INTO account_rates (account_id, space_id, from_on, until_on, kind, percent, by_hand) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (account_id, from_on) DO UPDATE SET until_on = EXCLUDED.until_on, kind = EXCLUDED.kind, percent = EXCLUDED.percent, by_hand = EXCLUDED.by_hand`,
		accountID, spaceID, r.From, r.Until, r.Kind, r.Percent, r.ByHand)
	if err != nil {
		return fmt.Errorf("account: set rate: %w", err)
	}
	return nil
}

// DeleteRate forgets the rate stated from day.
func (s *Store) DeleteRate(ctx context.Context, spaceID, accountID uuid.UUID, day time.Time) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM account_rates WHERE space_id = $1 AND account_id = $2 AND from_on = $3`, spaceID, accountID, day)
	if err != nil {
		return fmt.Errorf("account: delete rate: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// RateOn is the rate in force on day — the last one from day or before,
// unless its last day is past — as a percent a year, and that row; a rate
// tied to the key rate adds its margin to keyOn(day). ok is false without a
// rate in force, or without the key rate. A later rate replaces an earlier
// one even when that one's last day is ahead: the bank changed it.
func RateOn(rates []Rate, day time.Time, keyOn func(time.Time) (decimal.Decimal, bool, error)) (decimal.Decimal, *Rate, bool, error) {
	var in *Rate
	for i := range rates {
		if rates[i].From.After(day) {
			break
		}
		in = &rates[i]
	}
	if in == nil || (in.Until != nil && day.After(*in.Until)) {
		return decimal.Zero, nil, false, nil
	}
	if in.Kind == RateFixed {
		return in.Percent, in, true, nil
	}
	key, ok, err := keyOn(day)
	if err != nil || !ok {
		return decimal.Zero, in, false, err
	}
	return decimal.Max(key.Add(in.Percent), decimal.Zero), in, true, nil
}
