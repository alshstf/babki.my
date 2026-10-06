package marketdata

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

// BondDay is a bond's outstanding face value and the coupon interest accrued
// on it, per unit, on a day, both in Currency. A bond's quote is a percentage
// of that face; its worth is that plus the interest. Accrued is nil when the
// source stated it in another currency or not at all.
type BondDay struct {
	InstrumentID uuid.UUID
	On           time.Time
	Face         decimal.Decimal
	Accrued      *decimal.Decimal
	Currency     string
	Source       string
}

// UpsertBondDays inserts or replaces bond days by (bond, day).
func (s *Store) UpsertBondDays(ctx context.Context, days []BondDay) error {
	if len(days) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, d := range days {
		batch.Queue(`
			INSERT INTO bond_days (instrument_id, on_date, face_value, accrued, currency, source)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (instrument_id, on_date) DO UPDATE
			SET face_value = EXCLUDED.face_value, accrued = EXCLUDED.accrued,
				currency = EXCLUDED.currency, source = EXCLUDED.source, updated_at = now()`,
			d.InstrumentID, d.On, d.Face, d.Accrued, d.Currency, d.Source)
	}
	if err := runBatch(ctx, s.db, batch, len(days)); err != nil {
		return fmt.Errorf("marketdata: store bond days: %w", err)
	}
	return nil
}

// BondDaysOn is, per bond, its day on day or the nearest earlier one.
func (s *Store) BondDaysOn(ctx context.Context, ids []uuid.UUID, day time.Time) (map[uuid.UUID]BondDay, error) {
	out := make(map[uuid.UUID]BondDay, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT ON (instrument_id) instrument_id, on_date, face_value, accrued, currency, source
		FROM bond_days
		WHERE instrument_id = ANY($1) AND on_date <= $2
		ORDER BY instrument_id, on_date DESC`, ids, day)
	if err != nil {
		return nil, fmt.Errorf("marketdata: bond days: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d BondDay
		if err := rows.Scan(&d.InstrumentID, &d.On, &d.Face, &d.Accrued, &d.Currency, &d.Source); err != nil {
			return nil, fmt.Errorf("marketdata: read bond day: %w", err)
		}
		out[d.InstrumentID] = d
	}
	return out, rows.Err()
}

// BondDaysCoverage is the last bond day stored per bond from source.
func (s *Store) BondDaysCoverage(ctx context.Context, ids []uuid.UUID, source string) (map[uuid.UUID]time.Time, error) {
	out := make(map[uuid.UUID]time.Time, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `SELECT instrument_id, max(on_date) FROM bond_days
		WHERE source = $1 AND instrument_id = ANY($2) GROUP BY instrument_id`, source, ids)
	if err != nil {
		return nil, fmt.Errorf("marketdata: bond days coverage: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id  uuid.UUID
			day time.Time
		)
		if err := rows.Scan(&id, &day); err != nil {
			return nil, fmt.Errorf("marketdata: bond days coverage: %w", err)
		}
		out[id] = day
	}
	return out, rows.Err()
}
