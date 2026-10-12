package marketdata

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

// Metal is a precious metal the Bank of Russia prices.
type Metal string

const (
	Gold      Metal = "gold"
	Silver    Metal = "silver"
	Platinum  Metal = "platinum"
	Palladium Metal = "palladium"
)

// Metals are the four, in the bank's order.
var Metals = []Metal{Gold, Silver, Platinum, Palladium}

// MetalPrice is a metal's accounting price on a day, roubles per gram of the
// refined metal (decisions Р-39, Р-40): what a bar is worth by the market.
type MetalPrice struct {
	Metal  Metal
	On     time.Time
	Price  decimal.Decimal
	Source string
}

// UpsertMetalPrices stores prices, a day's price replacing the one stored.
func (s *Store) UpsertMetalPrices(ctx context.Context, prices []MetalPrice) error {
	if len(prices) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, p := range prices {
		batch.Queue(`
			INSERT INTO metal_prices (metal, on_date, price, source)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (metal, on_date) DO UPDATE SET
				price = EXCLUDED.price, source = EXCLUDED.source, updated_at = now()`,
			p.Metal, p.On, p.Price, p.Source)
	}
	return runBatch(ctx, s.db, batch, len(prices))
}

// MetalPriceOn is the metal's price on the day or the nearest earlier one,
// or pgx.ErrNoRows.
func (s *Store) MetalPriceOn(ctx context.Context, metal Metal, on time.Time) (MetalPrice, error) {
	p := MetalPrice{Metal: metal}
	err := s.db.QueryRow(ctx, `
		SELECT on_date, price, source FROM metal_prices
		WHERE metal = $1 AND on_date <= $2
		ORDER BY on_date DESC LIMIT 1`, metal, on).Scan(&p.On, &p.Price, &p.Source)
	return p, err
}

// LatestMetalPriceDay is the last day any metal has a price, or
// pgx.ErrNoRows.
func (s *Store) LatestMetalPriceDay(ctx context.Context) (time.Time, error) {
	var on *time.Time
	if err := s.db.QueryRow(ctx, `SELECT max(on_date) FROM metal_prices`).Scan(&on); err != nil {
		return time.Time{}, err
	}
	if on == nil {
		return time.Time{}, pgx.ErrNoRows
	}
	return *on, nil
}

// MetalPricesUpTo is every price of the metals on or before the day, each
// metal's oldest first — a history to resolve many days from in one read.
func (s *Store) MetalPricesUpTo(ctx context.Context, metals []Metal, to time.Time) ([]MetalPrice, error) {
	names := make([]string, len(metals))
	for i, m := range metals {
		names[i] = string(m)
	}
	rows, err := s.db.Query(ctx, `
		SELECT metal, on_date, price, source FROM metal_prices
		WHERE metal = ANY($1) AND on_date <= $2 ORDER BY metal, on_date`, names, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MetalPrice
	for rows.Next() {
		var p MetalPrice
		if err := rows.Scan(&p.Metal, &p.On, &p.Price, &p.Source); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
