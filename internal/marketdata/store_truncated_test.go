package marketdata_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
)

// This file covers the one failure a real fixture cannot be asked to produce:
// a query that starts streaming rows and then fails partway through. Postgres
// answers every statement these readers send either wholly or not at all, so
// the only way to reach the code that handles a mid-stream failure is to hand
// the Store a result set that behaves that way — see
// marketdata.NewStoreForRows.
//
// What is at stake is not the error's wording. A read cut short after its
// first row, reported as success, comes back as a SHORTER set of quotes or
// rates, and downstream that reads as "these instruments have no quotes" —
// an outage printed as the honest gap this application shows a person when
// data really is missing (#71).

// scanRow fills one row's worth of Scan destinations. One per row the fake
// yields, in the column order of the query being stood in for.
type scanRow func(dest ...any) error

// truncatedRows is a pgx.Rows whose iteration ends in a failure rather than in
// exhaustion: it yields every row in scans, and then Next reports false while
// Err reports err — which is precisely how pgx surfaces a connection that dies
// mid-result. A loop that watches only Next cannot tell the two apart, and
// that is the whole subject of these tests.
type truncatedRows struct {
	scans []scanRow
	err   error
	i     int
}

func (r *truncatedRows) Next() bool {
	if r.i >= len(r.scans) {
		return false
	}
	r.i++
	return true
}

func (r *truncatedRows) Scan(dest ...any) error { return r.scans[r.i-1](dest...) }
func (r *truncatedRows) Err() error             { return r.err }
func (r *truncatedRows) Close()                 {}

// The rest of pgx.Rows is not part of what these readers use. Panicking rather
// than returning a zero value keeps an accidental dependency on one of them
// visible instead of letting it quietly read as empty data.
func (r *truncatedRows) CommandTag() pgconn.CommandTag {
	panic("truncatedRows: CommandTag not used")
}

func (r *truncatedRows) FieldDescriptions() []pgconn.FieldDescription {
	panic("truncatedRows: FieldDescriptions not used")
}
func (r *truncatedRows) Values() ([]any, error) { panic("truncatedRows: Values not used") }
func (r *truncatedRows) RawValues() [][]byte    { panic("truncatedRows: RawValues not used") }
func (r *truncatedRows) Conn() *pgx.Conn        { panic("truncatedRows: Conn not used") }

// fixedRows answers every Query with the same result set, whatever the SQL. It
// stands in for the connection pool, so the reader under test runs its real
// body over a result set chosen here.
type fixedRows struct{ rows pgx.Rows }

func (q fixedRows) Query(context.Context, string, ...any) (pgx.Rows, error) { return q.rows, nil }

func (q fixedRows) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("fixedRows: QueryRow not used")
}

func (q fixedRows) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	panic("fixedRows: SendBatch not used")
}

// storeOver builds a Store whose reads come from one result set that yields
// scans and then fails with err.
func storeOver(err error, scans ...scanRow) *marketdata.Store {
	return marketdata.NewStoreForRows(fixedRows{rows: &truncatedRows{scans: scans, err: err}})
}

// assign copies values into Scan's destinations, checking both the count and
// each destination's type, so a fake row that stops matching the query it
// stands in for fails loudly here rather than silently scanning a zero value
// into the result under test.
func assign(dest []any, values ...any) error {
	if len(dest) != len(values) {
		return fmt.Errorf("scan: %d destinations for %d values", len(dest), len(values))
	}
	for i, v := range values {
		var ok bool
		switch d := dest[i].(type) {
		case *uuid.UUID:
			var got uuid.UUID
			got, ok = v.(uuid.UUID)
			*d = got
		case *time.Time:
			var got time.Time
			got, ok = v.(time.Time)
			*d = got
		case *decimal.Decimal:
			var got decimal.Decimal
			got, ok = v.(decimal.Decimal)
			*d = got
		case *string:
			var got string
			got, ok = v.(string)
			*d = got
		case *int64:
			var got int64
			got, ok = v.(int64)
			*d = got
		default:
			return fmt.Errorf("scan: destination %d has unsupported type %T", i, dest[i])
		}
		if !ok {
			return fmt.Errorf("scan: destination %d is %T, value is %T", i, dest[i], v)
		}
	}
	return nil
}

// quoteRow is one row of the column list Store.LatestQuotes selects.
func quoteRow(q marketdata.Quote) scanRow {
	return func(dest ...any) error {
		return assign(dest, q.InstrumentID, q.On, q.Price, q.Currency, q.Source)
	}
}

// fxRateRow is one row of the column list Store.LatestFxRates selects.
func fxRateRow(r marketdata.FxRate) scanRow {
	return func(dest ...any) error {
		return assign(dest, r.Base, r.Quote, r.On, r.Rate, r.Source)
	}
}

// fxRateByOrdinalRow is one row of the column list Store.FxRatesOn selects:
// the caller's key position first, then the resolved rate's own columns.
func fxRateByOrdinalRow(ord int64, r marketdata.FxRate) scanRow {
	return func(dest ...any) error {
		return assign(dest, ord, r.On, r.Rate, r.Source)
	}
}

func TestLatestQuotesRefusesATruncatedRead(t *testing.T) {
	boom := errors.New("connection reset while streaming rows")
	ids := []uuid.UUID{uuid.New(), uuid.New()}
	store := storeOver(boom, quoteRow(marketdata.Quote{
		InstrumentID: ids[0], On: date("2026-07-01"),
		Price: dec("100.5"), Currency: "RUB", Source: "moex",
	}))

	got, err := store.LatestQuotes(context.Background(), ids)
	if !errors.Is(err, boom) {
		t.Fatalf("LatestQuotes err = %v, want %v: a read that failed after its first row is a failure, not a short answer", err, boom)
	}
	if got != nil {
		t.Fatalf("LatestQuotes returned %d quotes alongside the failure; a caller that reads the map anyway would take the missing instruments for unquoted ones", len(got))
	}
}

func TestLatestFxRatesRefusesATruncatedRead(t *testing.T) {
	boom := errors.New("connection reset while streaming rows")
	store := storeOver(boom, fxRateRow(marketdata.FxRate{
		Base: "USD", Quote: "RUB", On: date("2026-07-01"),
		Rate: dec("90.5"), Source: "cbr",
	}))

	got, err := store.LatestFxRates(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("LatestFxRates err = %v, want %v: a read that failed after its first row is a failure, not a short answer", err, boom)
	}
	if got != nil {
		t.Fatalf("LatestFxRates returned %d rates alongside the failure; the pairs missing from that slice never had a rate, as far as any caller can tell", len(got))
	}
}

func TestFxRatesOnRefusesATruncatedRead(t *testing.T) {
	boom := errors.New("connection reset while streaming rows")
	keys := []marketdata.FxRateKey{
		{Base: "USD", Quote: "RUB", On: date("2026-07-01")},
		{Base: "EUR", Quote: "RUB", On: date("2026-07-01")},
	}
	store := storeOver(boom, fxRateByOrdinalRow(1, marketdata.FxRate{
		On: date("2026-07-01"), Rate: dec("90.5"), Source: "cbr",
	}))

	got, err := store.FxRatesOn(context.Background(), keys)
	if !errors.Is(err, boom) {
		t.Fatalf("FxRatesOn err = %v, want %v: a read that failed after its first row is a failure, not a short answer", err, boom)
	}
	if got != nil {
		t.Fatalf("FxRatesOn returned %d rates alongside the failure; a key absent from that map reads as a pair with no rate at all", len(got))
	}
}
