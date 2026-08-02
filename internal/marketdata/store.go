package marketdata

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const fxRateCols = `base, quote, on_date, rate, source`

func scanFxRate(row pgx.Row) (FxRate, error) {
	var r FxRate
	err := row.Scan(&r.Base, &r.Quote, &r.On, &r.Rate, &r.Source)
	return r, err
}

const quoteCols = `instrument_id, on_date, price, currency, source`

func scanQuote(row pgx.Row) (Quote, error) {
	var q Quote
	err := row.Scan(&q.InstrumentID, &q.On, &q.Price, &q.Currency, &q.Source)
	return q, err
}

// runBatch sends batch and checks the result of each of its n queued
// commands, so a mid-batch failure (e.g. a CHECK violation) is reported
// instead of silently ignored.
func runBatch(ctx context.Context, pool *pgxpool.Pool, batch *pgx.Batch, n int) error {
	br := pool.SendBatch(ctx, batch)
	for range n {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return err
		}
	}
	return br.Close()
}

// UpsertFxRates inserts or updates a batch of daily FX rates. A repeat
// upsert for the same (base, quote, on) replaces the existing row rather
// than duplicating it.
func (s *Store) UpsertFxRates(ctx context.Context, rates []FxRate) error {
	if len(rates) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, r := range rates {
		batch.Queue(`
			INSERT INTO fx_rates (base, quote, on_date, rate, source)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (base, quote, on_date) DO UPDATE SET
				rate = EXCLUDED.rate, source = EXCLUDED.source, updated_at = now()`,
			r.Base, r.Quote, r.On, r.Rate, r.Source)
	}
	return runBatch(ctx, s.pool, batch, len(rates))
}

// FxRateOn returns the rate for (base, quote) on the exact date, or, if
// missing, the nearest earlier date. pgx.ErrNoRows if no rate exists on or
// before the given date.
func (s *Store) FxRateOn(ctx context.Context, base, quote string, on time.Time) (FxRate, error) {
	return scanFxRate(s.pool.QueryRow(ctx, `
		SELECT `+fxRateCols+` FROM fx_rates
		WHERE base = $1 AND quote = $2 AND on_date <= $3
		ORDER BY on_date DESC LIMIT 1`, base, quote, on))
}

// FxRatesOn resolves many (base, quote, date) lookups in a single round
// trip, one FxRateOn call each would otherwise need. Each key's outcome is
// exactly what FxRateOn would return for it individually: the exact date, or
// the nearest earlier one — the two statements of that rule are held
// together only by the differential test against FxRateOn in
// store_test.go (TestFxRatesOnBatch), because there is no good way to share
// the SQL fragment itself; change one only through that test. A key with
// nothing on or before its date is absent from the result map, not
// zero-valued — the same convention LatestQuotes uses for instruments
// without quotes. The returned FxRate carries the resolved row's own date
// (on_date), not the requested one, because "how stale is this rate" is
// exactly what callers use that date for.
//
// The lookup is expressed as unnest of the three key columns joined
// LATERAL against fx_rates, so the row-per-key "nearest earlier date" search
// runs inside a single query regardless of how many keys are passed — the
// same shape LatestQuotes uses for a set of instrument ids, generalized to a
// three-column key with its own per-row ORDER BY ... LIMIT 1 instead of a
// flat ANY($1).
//
// The unnest is WITH ORDINALITY, and what comes back per row is that
// ordinal, not the base/quote/on_date columns themselves. base and quote
// round-trip byte-identical, but on_date does not: it goes out as `date`
// and pgx reads dates back as midnight UTC, so a key rebuilt from the
// returned column would only match a caller's key that already happened to
// be exactly midnight in time.UTC — missing, for instance, every
// time.Now().UTC() caller in this codebase. Indexing the caller's own keys
// slice by ordinal instead makes the returned map key the caller's own
// value by construction, so that mismatch cannot recur. See FxRateKey's
// doc comment for the same reasoning from the caller's side.
func (s *Store) FxRatesOn(ctx context.Context, keys []FxRateKey) (map[FxRateKey]FxRate, error) {
	out := make(map[FxRateKey]FxRate, len(keys))
	if len(keys) == 0 {
		return out, nil
	}

	bases := make([]string, len(keys))
	quotes := make([]string, len(keys))
	dates := make([]time.Time, len(keys))
	for i, k := range keys {
		bases[i] = k.Base
		quotes[i] = k.Quote
		dates[i] = k.On
	}

	rows, err := s.pool.Query(ctx, `
		SELECT k.ord, r.on_date, r.rate, r.source
		FROM unnest($1::text[], $2::text[], $3::date[]) WITH ORDINALITY AS k(base, quote, on_date, ord)
		JOIN LATERAL (
			SELECT fx_rates.on_date, fx_rates.rate, fx_rates.source
			FROM fx_rates
			WHERE fx_rates.base = k.base
			  AND fx_rates.quote = k.quote
			  AND fx_rates.on_date <= k.on_date
			ORDER BY fx_rates.on_date DESC
			LIMIT 1
		) r ON true`, bases, quotes, dates)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ord int64
		var r FxRate
		if err := rows.Scan(&ord, &r.On, &r.Rate, &r.Source); err != nil {
			return nil, err
		}
		key := keys[ord-1] // WITH ORDINALITY is 1-based
		r.Base, r.Quote = key.Base, key.Quote
		out[key] = r
	}
	return out, rows.Err()
}

// LatestFxRates returns the most recent rate for every (base, quote) pair.
func (s *Store) LatestFxRates(ctx context.Context) ([]FxRate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (base, quote) `+fxRateCols+`
		FROM fx_rates
		ORDER BY base, quote, on_date DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FxRate
	for rows.Next() {
		r, err := scanFxRate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertQuotes inserts or updates a batch of daily instrument quotes. A
// repeat upsert for the same (instrument, on) replaces the existing row
// rather than duplicating it.
func (s *Store) UpsertQuotes(ctx context.Context, quotes []Quote) error {
	if len(quotes) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, q := range quotes {
		batch.Queue(`
			INSERT INTO quotes (instrument_id, on_date, price, currency, source)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (instrument_id, on_date) DO UPDATE SET
				price = EXCLUDED.price, currency = EXCLUDED.currency,
				source = EXCLUDED.source, updated_at = now()`,
			q.InstrumentID, q.On, q.Price, q.Currency, q.Source)
	}
	return runBatch(ctx, s.pool, batch, len(quotes))
}

// QuoteOn returns the instrument's price on the exact date, or, if missing,
// the nearest earlier date. pgx.ErrNoRows if no quote exists on or before
// the given date.
func (s *Store) QuoteOn(ctx context.Context, instrumentID uuid.UUID, on time.Time) (Quote, error) {
	return scanQuote(s.pool.QueryRow(ctx, `
		SELECT `+quoteCols+` FROM quotes
		WHERE instrument_id = $1 AND on_date <= $2
		ORDER BY on_date DESC LIMIT 1`, instrumentID, on))
}

// LatestQuotes returns the most recent quote for each of instrumentIDs, in
// a single round trip. Instruments with no quotes at all are absent from
// the result map (not zero-valued).
func (s *Store) LatestQuotes(ctx context.Context, instrumentIDs []uuid.UUID) (map[uuid.UUID]Quote, error) {
	out := make(map[uuid.UUID]Quote, len(instrumentIDs))
	if len(instrumentIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (instrument_id) `+quoteCols+`
		FROM quotes
		WHERE instrument_id = ANY($1)
		ORDER BY instrument_id, on_date DESC`, instrumentIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		q, err := scanQuote(rows)
		if err != nil {
			return nil, err
		}
		out[q.InstrumentID] = q
	}
	return out, rows.Err()
}
