package marketdata

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/shopspring/decimal"
)

// Benchmark is a total-return index the family's return is weighed against
// (#401): what the same money would have made in it.
type Benchmark struct {
	Code     string
	Currency string
	// Source is the feed it comes from, Symbol what the feed calls it.
	Source, Symbol string
}

// Benchmarks are the indices offered, Russian shares and government bonds
// first, the S&P 500 with dividends for money kept in dollars.
var Benchmarks = []Benchmark{
	{Code: "MCFTR", Currency: "RUB", Source: "moex", Symbol: "MCFTR"},
	{Code: "RGBITR", Currency: "RUB", Source: "moex", Symbol: "RGBITR"},
	{Code: "SP500TR", Currency: "USD", Source: "yahoo", Symbol: "^SP500TR"},
}

// IndexFeed publishes an index's daily closes from from to till, oldest
// first.
type IndexFeed interface {
	IndexHistory(ctx context.Context, symbol string, from, till time.Time) ([]DayPrice, error)
}

// closesFeed reads an index off a feed of closing prices.
type closesFeed struct {
	closes func(ctx context.Context, symbol string, from, to time.Time) ([]DayPrice, error)
}

func (f closesFeed) IndexHistory(ctx context.Context, symbol string, from, till time.Time) ([]DayPrice, error) {
	return f.closes(ctx, symbol, from, till)
}

// ClosesAsIndex lets a feed of foreign closing prices answer for an index.
func ClosesAsIndex(p ForeignQuoteProvider) IndexFeed {
	return closesFeed{closes: p.Closes}
}

// UpsertIndexValues stores an index's closes; a later fetch of a day wins.
func (s *Store) UpsertIndexValues(ctx context.Context, code, source string, days []DayPrice) error {
	batch := &pgx.Batch{}
	for _, d := range days {
		if !d.Price.IsPositive() {
			continue
		}
		batch.Queue(`
			INSERT INTO index_values (code, on_date, value, source) VALUES ($1, $2, $3, $4)
			ON CONFLICT (code, on_date) DO UPDATE SET value = EXCLUDED.value, source = EXCLUDED.source, fetched_at = now()`,
			code, d.Day, d.Price, source)
	}
	if batch.Len() == 0 {
		return nil
	}
	return runBatch(ctx, s.db, batch, batch.Len())
}

// LatestIndexDay is the last day an index is stored for; ok false with none.
func (s *Store) LatestIndexDay(ctx context.Context, code string) (time.Time, bool, error) {
	var on *time.Time
	if err := s.db.QueryRow(ctx, `SELECT MAX(on_date) FROM index_values WHERE code = $1`, code).Scan(&on); err != nil {
		return time.Time{}, false, fmt.Errorf("marketdata: index %s: %w", code, err)
	}
	if on == nil {
		return time.Time{}, false, nil
	}
	return *on, true, nil
}

// IndexValueOn is an index's close on the day or the latest before it within
// a week (a weekend, a holiday); ok false when there is none that near.
func (s *Store) IndexValueOn(ctx context.Context, code string, on time.Time) (decimal.Decimal, bool, error) {
	var v decimal.Decimal
	err := s.db.QueryRow(ctx, `
		SELECT value FROM index_values WHERE code = $1 AND on_date <= $2 AND on_date > $2::date - 7
		ORDER BY on_date DESC LIMIT 1`, code, on).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return decimal.Zero, false, nil
	}
	if err != nil {
		return decimal.Zero, false, fmt.Errorf("marketdata: index %s on %s: %w", code, on.Format(time.DateOnly), err)
	}
	return v, true, nil
}

// RefreshIndexesArgs is the daily job that stores the benchmarks' closes.
type RefreshIndexesArgs struct{}

func (RefreshIndexesArgs) Kind() string { return "marketdata.refresh_indexes" }

type earliestDay interface {
	EarliestRecordedDay(ctx context.Context) (time.Time, error)
}

type indexesWorker struct {
	river.WorkerDefaults[RefreshIndexesArgs]
	store *Store
	ops   earliestDay
	feeds map[string]IndexFeed
	log   *slog.Logger
	now   func() time.Time
}

// NewIndexesWorker stores each benchmark's closes from its feed, from the day
// after the last stored — or from a week before the earliest journal row —
// to today. A benchmark without a feed is skipped.
func NewIndexesWorker(store *Store, ops earliestDay, feeds map[string]IndexFeed, log *slog.Logger) river.Worker[RefreshIndexesArgs] {
	return &indexesWorker{store: store, ops: ops, feeds: feeds, log: log, now: time.Now}
}

func (w *indexesWorker) Timeout(*river.Job[RefreshIndexesArgs]) time.Duration { return backfillTimeout }

func (w *indexesWorker) Work(ctx context.Context, _ *river.Job[RefreshIndexesArgs]) error {
	now := w.now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	start, err := w.ops.EarliestRecordedDay(ctx)
	if err != nil {
		return err
	}
	if start.IsZero() {
		return nil
	}
	start = start.AddDate(0, 0, -7)
	var last error
	for _, b := range Benchmarks {
		feed, ok := w.feeds[b.Source]
		if !ok {
			continue
		}
		from := start
		if latest, ok, err := w.store.LatestIndexDay(ctx, b.Code); err != nil {
			return err
		} else if ok {
			from = latest.AddDate(0, 0, 1)
		}
		if from.After(today) {
			continue
		}
		days, err := feed.IndexHistory(ctx, b.Symbol, from, today)
		if err != nil {
			w.log.Warn("benchmark closes", "index", b.Code, "error", err)
			last = err
			continue
		}
		if err := w.store.UpsertIndexValues(ctx, b.Code, b.Source, days); err != nil {
			return err
		}
	}
	return last
}
