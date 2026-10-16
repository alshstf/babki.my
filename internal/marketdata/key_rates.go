package marketdata

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

// KeyRate is the Bank of Russia's key rate in force on a day, percent.
type KeyRate struct {
	On     time.Time
	Rate   decimal.Decimal
	Source string
}

// UpsertKeyRates stores rates, a day's replacing the one stored.
func (s *Store) UpsertKeyRates(ctx context.Context, rates []KeyRate) error {
	if len(rates) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, r := range rates {
		batch.Queue(`
			INSERT INTO key_rates (on_date, rate, source) VALUES ($1, $2, $3)
			ON CONFLICT (on_date) DO UPDATE SET rate = EXCLUDED.rate, source = EXCLUDED.source, updated_at = now()`,
			r.On, r.Rate, r.Source)
	}
	return runBatch(ctx, s.db, batch, len(rates))
}

// HighestKeyRate is the highest key rate in force on any day of [from, to]:
// the days stored within it and the rate in force on from, the last before
// it. found is false when nothing is stored for the span.
func (s *Store) HighestKeyRate(ctx context.Context, from, to time.Time) (rate decimal.Decimal, found bool, err error) {
	var r *decimal.Decimal
	err = s.db.QueryRow(ctx, `
		SELECT max(rate) FROM key_rates
		WHERE on_date BETWEEN $1 AND $2
		   OR on_date = (SELECT max(on_date) FROM key_rates WHERE on_date <= $1)`, from, to).Scan(&r)
	if err != nil || r == nil {
		return decimal.Zero, false, err
	}
	return *r, true, nil
}

// LatestKeyRateDay is the last day a rate is stored for, or pgx.ErrNoRows.
func (s *Store) LatestKeyRateDay(ctx context.Context) (time.Time, error) {
	var on *time.Time
	if err := s.db.QueryRow(ctx, `SELECT max(on_date) FROM key_rates`).Scan(&on); err != nil {
		return time.Time{}, err
	}
	if on == nil {
		return time.Time{}, pgx.ErrNoRows
	}
	return *on, nil
}

// KeyRateOn is the key rate in force on day: the last stored on or before it.
// found is false before the first.
func (s *Store) KeyRateOn(ctx context.Context, day time.Time) (rate decimal.Decimal, found bool, err error) {
	var r *decimal.Decimal
	err = s.db.QueryRow(ctx, `SELECT rate FROM key_rates WHERE on_date <= $1 ORDER BY on_date DESC LIMIT 1`, day).Scan(&r)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && r == nil) {
		return decimal.Zero, false, nil
	}
	if err != nil {
		return decimal.Zero, false, err
	}
	return *r, true, nil
}
