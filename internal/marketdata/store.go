package marketdata

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// querier is what Store needs from a pool. Tests substitute rows that fail
// mid-stream, and `babki seed` passes a pgx.Tx.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

type Store struct{ db querier }

func NewStore(x querier) *Store { return &Store{db: x} }

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

// runBatch sends batch and checks each of its n results, so a mid-batch
// failure is reported.
func runBatch(ctx context.Context, db querier, batch *pgx.Batch, n int) error {
	br := db.SendBatch(ctx, batch)
	for range n {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return err
		}
	}
	return br.Close()
}

// UpsertFxRates inserts or replaces daily rates by (base, quote, on).
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
	return runBatch(ctx, s.db, batch, len(rates))
}

// FxRateOn returns the rate on the date or the nearest earlier one, or
// pgx.ErrNoRows.
func (s *Store) FxRateOn(ctx context.Context, base, quote string, on time.Time) (FxRate, error) {
	return scanFxRate(s.db.QueryRow(ctx, `
		SELECT `+fxRateCols+` FROM fx_rates
		WHERE base = $1 AND quote = $2 AND on_date <= $3
		ORDER BY on_date DESC LIMIT 1`, base, quote, on))
}

// FxRatesOn answers many FxRateOn lookups in one round trip, with the same
// semantics (held together by TestFxRatesOnBatch). Keys with no rate are
// absent. Each result carries the row's own date.
//
// Results are matched to keys by ordinal, not by the returned date: pgx reads
// dates back as midnight UTC, which would not equal most callers' keys.
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

	rows, err := s.db.Query(ctx, `
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
	// A read broken partway is an error, not a shorter map: a missing key would
	// read as an honest "no rate" (#71).
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// LatestFxRates returns the most recent rate for every (base, quote) pair.
func (s *Store) LatestFxRates(ctx context.Context) ([]FxRate, error) {
	rows, err := s.db.Query(ctx, `
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
	// A broken read is an error, not a shorter slice.
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// UpsertQuotes inserts or replaces daily quotes by (instrument, on).
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
	return runBatch(ctx, s.db, batch, len(quotes))
}

// StoreLatestQuotes stores each quote as its source's latest word: the row is
// upserted and the same source's later-dated rows for the instrument are
// removed. A source that now dates a price earlier had carried it forward
// under each session's date (#199). Other sources' rows are untouched.
func (s *Store) StoreLatestQuotes(ctx context.Context, quotes []Quote) error {
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
		batch.Queue(`DELETE FROM quotes WHERE instrument_id = $1 AND source = $2 AND on_date > $3`,
			q.InstrumentID, q.Source, q.On)
	}
	return runBatch(ctx, s.db, batch, 2*len(quotes))
}

func (s *Store) HistoryCoverage(ctx context.Context, ids []uuid.UUID, source string) (map[uuid.UUID]time.Time, error) {
	out := make(map[uuid.UUID]time.Time, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `SELECT instrument_id, max(on_date) FROM quotes
		WHERE source = $1 AND instrument_id = ANY($2) GROUP BY instrument_id`, source, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id  uuid.UUID
			day time.Time
		)
		if err := rows.Scan(&id, &day); err != nil {
			return nil, err
		}
		out[id] = day
	}
	return out, rows.Err()
}

// QuoteOn returns the price on the date or the nearest earlier one, or
// pgx.ErrNoRows.
func (s *Store) QuoteOn(ctx context.Context, instrumentID uuid.UUID, on time.Time) (Quote, error) {
	return scanQuote(s.db.QueryRow(ctx, `
		SELECT `+quoteCols+` FROM quotes
		WHERE instrument_id = $1 AND on_date <= $2
		ORDER BY on_date DESC LIMIT 1`, instrumentID, on))
}

// QuotesOn returns each instrument's price on day or the nearest earlier one.
// Instruments with none are absent.
func (s *Store) QuotesOn(ctx context.Context, instrumentIDs []uuid.UUID, day time.Time) (map[uuid.UUID]Quote, error) {
	out := make(map[uuid.UUID]Quote, len(instrumentIDs))
	if len(instrumentIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT ON (instrument_id) `+quoteCols+` FROM quotes
		WHERE instrument_id = ANY($1) AND on_date <= $2
		ORDER BY instrument_id, on_date DESC`, instrumentIDs, day)
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

// PriceSeries returns the quotes in [from, to], oldest first.
func (s *Store) PriceSeries(ctx context.Context, instrumentID uuid.UUID, from, to time.Time) ([]Quote, error) {
	rows, err := s.db.Query(ctx, `SELECT `+quoteCols+` FROM quotes
		WHERE instrument_id = $1 AND on_date BETWEEN $2 AND $3
		ORDER BY on_date`, instrumentID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Quote
	for rows.Next() {
		q, err := scanQuote(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// LatestQuotes returns each instrument's most recent quote. Instruments with
// none are absent.
func (s *Store) LatestQuotes(ctx context.Context, instrumentIDs []uuid.UUID) (map[uuid.UUID]Quote, error) {
	out := make(map[uuid.UUID]Quote, len(instrumentIDs))
	if len(instrumentIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `
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
	// A broken read is an error: a missing instrument would read as unquoted
	// (#71).
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
