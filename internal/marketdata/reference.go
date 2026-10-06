package marketdata

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

// ReferenceKind is which reference price a paper has (decision Р-11).
type ReferenceKind string

const (
	// ReferenceNAV is a fund's net asset value per unit.
	ReferenceNAV ReferenceKind = "nav"
	// ReferenceForeign is a foreign share's price on its home exchange.
	ReferenceForeign ReferenceKind = "foreign"
)

// ReferencePrice is a price the full valuation may use for a paper on a day,
// in Currency, which need not be the paper's. It is never a price the paper
// can be sold at here: that is a quote.
type ReferencePrice struct {
	InstrumentID uuid.UUID
	Kind         ReferenceKind
	On           time.Time
	Price        decimal.Decimal
	Currency     string
	Source       string
}

// UpsertReferencePrices inserts or replaces prices by (paper, kind, day).
func (s *Store) UpsertReferencePrices(ctx context.Context, prices []ReferencePrice) error {
	if len(prices) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, p := range prices {
		batch.Queue(`
			INSERT INTO reference_prices (instrument_id, kind, on_date, price, currency, source)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (instrument_id, kind, on_date) DO UPDATE
			SET price = EXCLUDED.price, currency = EXCLUDED.currency, source = EXCLUDED.source, updated_at = now()`,
			p.InstrumentID, string(p.Kind), p.On, p.Price, p.Currency, p.Source)
	}
	if err := runBatch(ctx, s.db, batch, len(prices)); err != nil {
		return fmt.Errorf("marketdata: store reference prices: %w", err)
	}
	return nil
}

// ReferenceCoverage is the last day stored per paper for kind.
func (s *Store) ReferenceCoverage(ctx context.Context, ids []uuid.UUID, kind ReferenceKind) (map[uuid.UUID]time.Time, error) {
	out := make(map[uuid.UUID]time.Time, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `SELECT instrument_id, max(on_date) FROM reference_prices
		WHERE kind = $1 AND instrument_id = ANY($2) GROUP BY instrument_id`, string(kind), ids)
	if err != nil {
		return nil, fmt.Errorf("marketdata: reference coverage: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id  uuid.UUID
			day time.Time
		)
		if err := rows.Scan(&id, &day); err != nil {
			return nil, fmt.Errorf("marketdata: reference coverage: %w", err)
		}
		out[id] = day
	}
	return out, rows.Err()
}

// ReferencePricesOn is, per paper and kind, the price on day or the nearest
// earlier one.
func (s *Store) ReferencePricesOn(ctx context.Context, ids []uuid.UUID, day time.Time) (map[uuid.UUID]map[ReferenceKind]ReferencePrice, error) {
	out := map[uuid.UUID]map[ReferenceKind]ReferencePrice{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT ON (instrument_id, kind) instrument_id, kind, on_date, price, currency, source
		FROM reference_prices
		WHERE instrument_id = ANY($1) AND on_date <= $2
		ORDER BY instrument_id, kind, on_date DESC`, ids, day)
	if err != nil {
		return nil, fmt.Errorf("marketdata: reference prices: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanReference(rows)
		if err != nil {
			return nil, err
		}
		if out[p.InstrumentID] == nil {
			out[p.InstrumentID] = map[ReferenceKind]ReferencePrice{}
		}
		out[p.InstrumentID][p.Kind] = p
	}
	return out, rows.Err()
}

// ReferenceSeries is a paper's prices of kind from from to to, oldest first.
func (s *Store) ReferenceSeries(ctx context.Context, id uuid.UUID, kind ReferenceKind, from, to time.Time) ([]ReferencePrice, error) {
	rows, err := s.db.Query(ctx, `
		SELECT instrument_id, kind, on_date, price, currency, source FROM reference_prices
		WHERE instrument_id = $1 AND kind = $2 AND on_date BETWEEN $3 AND $4
		ORDER BY on_date`, id, string(kind), from, to)
	if err != nil {
		return nil, fmt.Errorf("marketdata: reference series: %w", err)
	}
	defer rows.Close()
	var out []ReferencePrice
	for rows.Next() {
		p, err := scanReference(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func scanReference(rows pgx.Rows) (ReferencePrice, error) {
	var (
		p    ReferencePrice
		kind string
	)
	if err := rows.Scan(&p.InstrumentID, &kind, &p.On, &p.Price, &p.Currency, &p.Source); err != nil {
		return ReferencePrice{}, fmt.Errorf("marketdata: read reference price: %w", err)
	}
	p.Kind = ReferenceKind(kind)
	return p, nil
}
