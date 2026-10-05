package marketdata

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

// Dividend is one dividend an issuer declared, as a market data source
// publishes it: so much per share, in Currency, to whoever holds the paper on
// RecordDate (decision Р-14).
//
// PerShare is the declared amount BEFORE any tax: what the issuer pays, not
// what reaches the holder. That difference is the whole use of it — a foreign
// dividend arrives net of the tax withheld abroad, and the declared amount per
// share times the shares held on the record date is the gross it was taken
// from.
//
// LastBuyDate is the last day a purchase still carried the right to it, when
// the source says; it is the precise question "how many shares did this
// account hold for it", where the record date is one a settlement cycle late.
type Dividend struct {
	InstrumentID uuid.UUID
	Source       string
	RecordDate   time.Time
	PaymentDate  *time.Time
	LastBuyDate  *time.Time
	PerShare     decimal.Decimal
	Currency     string
}

// ReplaceDividends stores what source says about one paper's dividends now, in
// place of everything it said before. One batch, so a reader sees either the
// old calendar or the new one and never a paper with none in between.
func (s *Store) ReplaceDividends(ctx context.Context, instrumentID uuid.UUID, source string,
	dividends []Dividend, fetchedAt time.Time,
) error {
	batch := &pgx.Batch{}
	batch.Queue(`DELETE FROM instrument_dividends WHERE instrument_id = $1 AND source = $2`, instrumentID, source)
	for _, d := range dividends {
		if d.InstrumentID != instrumentID || d.Source != source {
			return fmt.Errorf("marketdata: a dividend of %s from %q handed in with the calendar of %s from %q",
				d.InstrumentID, d.Source, instrumentID, source)
		}
		batch.Queue(`
			INSERT INTO instrument_dividends
				(instrument_id, source, record_date, payment_date, last_buy_date, per_share, currency, fetched_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			d.InstrumentID, d.Source, d.RecordDate, d.PaymentDate, d.LastBuyDate, d.PerShare, d.Currency, fetchedAt)
	}
	return runBatch(ctx, s.db, batch, 1+len(dividends))
}

// DividendsOf is every stored dividend of the given papers, by paper, oldest
// record date first.
func (s *Store) DividendsOf(ctx context.Context, instrumentIDs []uuid.UUID) (map[uuid.UUID][]Dividend, error) {
	out := map[uuid.UUID][]Dividend{}
	if len(instrumentIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT instrument_id, source, record_date, payment_date, last_buy_date, per_share, currency
		FROM instrument_dividends
		WHERE instrument_id = ANY($1)
		ORDER BY instrument_id, record_date, source`, instrumentIDs)
	if err != nil {
		return nil, fmt.Errorf("marketdata: list dividends: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d Dividend
		if err := rows.Scan(&d.InstrumentID, &d.Source, &d.RecordDate, &d.PaymentDate, &d.LastBuyDate,
			&d.PerShare, &d.Currency); err != nil {
			return nil, fmt.Errorf("marketdata: list dividends: %w", err)
		}
		out[d.InstrumentID] = append(out[d.InstrumentID], d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("marketdata: list dividends: %w", err)
	}
	return out, nil
}
