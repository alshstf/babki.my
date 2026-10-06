package marketdata

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"babki.my/babki/internal/instrument"
)

// RefreshDividendCalendarArgs is the daily job that stores, from a share feed,
// the dividend calendar of the foreign papers no broker's calendar covers
// (decision Р-14): without one, the tax a foreign dividend lost abroad is
// unknown.
type RefreshDividendCalendarArgs struct{}

func (RefreshDividendCalendarArgs) Kind() string { return "marketdata.refresh_dividend_calendar" }

// DividendFeed publishes the dividends shares and funds declared.
type DividendFeed interface {
	// SymbolFor names a paper by its ISIN; ok is false when the feed has none.
	SymbolFor(ctx context.Context, isin string) (symbol string, ok bool, err error)
	// Dividends is the paper's dividends dated from from to to, oldest first.
	Dividends(ctx context.Context, symbol string, from, to time.Time) ([]Dividend, error)
	Name() string
}

// dividendLookback: a calendar reaches a year before the paper's first entry,
// as a dividend is paid weeks after its record date.
const dividendLookback = 365

type dividendCalendarWorker struct {
	river.WorkerDefaults[RefreshDividendCalendarArgs]
	store       *Store
	ops         instrumentFirstDays
	instruments instrumentsByID
	feed        DividendFeed
	log         *slog.Logger
	now         func() time.Time
}

// NewDividendCalendarWorker builds the River worker that stores the feed's
// dividend calendars; with a nil feed it stores nothing.
func NewDividendCalendarWorker(store *Store, ops instrumentFirstDays, instruments instrumentsByID,
	feed DividendFeed, log *slog.Logger,
) river.Worker[RefreshDividendCalendarArgs] {
	return &dividendCalendarWorker{store: store, ops: ops, instruments: instruments, feed: feed, log: log, now: time.Now}
}

func (w *dividendCalendarWorker) Timeout(*river.Job[RefreshDividendCalendarArgs]) time.Duration {
	return backfillTimeout
}

// Work stores the whole calendar of every foreign share and fund the journals
// hold, unless a broker's calendar already covers it. One paper's failure does
// not stop the others; the last error is returned at the end so River retries.
func (w *dividendCalendarWorker) Work(ctx context.Context, _ *river.Job[RefreshDividendCalendarArgs]) error {
	if w.feed == nil {
		return nil
	}
	first, err := w.ops.FirstDaysByInstrument(ctx)
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, 0, len(first))
	for id := range first {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	papers, err := w.instruments.ByIDs(ctx, ids)
	if err != nil {
		return err
	}
	sources, err := w.store.CalendarSources(ctx, ids)
	if err != nil {
		return err
	}
	now := w.now()
	var lastErr error
	for _, id := range ids {
		paper, ok := papers[id]
		isin := strings.ToUpper(paper.ISIN)
		if !ok || (paper.Type != instrument.TypeShare && paper.Type != instrument.TypeETF) || !instrument.ForeignISIN(isin) {
			continue
		}
		if slices.ContainsFunc(sources[id], func(s string) bool { return s != w.feed.Name() }) {
			// A broker's calendar arrived: the feed's own, now a second opinion, goes.
			if slices.Contains(sources[id], w.feed.Name()) {
				if err := w.store.ReplaceDividends(ctx, id, w.feed.Name(), nil, now); err != nil {
					return err
				}
			}
			continue
		}
		symbol, found, err := w.feed.SymbolFor(ctx, isin)
		if err != nil {
			w.log.Warn("marketdata: look a paper up for its dividends failed", "instrument", id, "source", w.feed.Name(), "err", err)
			lastErr = err
			continue
		}
		if !found {
			w.log.Debug("marketdata: no dividend calendar for a foreign paper", "instrument", id, "isin", isin)
			continue
		}
		dividends, err := w.feed.Dividends(ctx, symbol, utcDay(first[id]).AddDate(0, 0, -dividendLookback), now)
		if err != nil {
			w.log.Warn("marketdata: fetch a dividend calendar failed", "instrument", id, "source", w.feed.Name(), "err", err)
			lastErr = err
			continue
		}
		for i := range dividends {
			dividends[i].InstrumentID, dividends[i].Source = id, w.feed.Name()
		}
		if err := w.store.ReplaceDividends(ctx, id, w.feed.Name(), dividends, now); err != nil {
			return err
		}
		w.log.Info("marketdata: stored a dividend calendar", "instrument", id, "source", w.feed.Name(), "dividends", len(dividends))
	}
	return lastErr
}
