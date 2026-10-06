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

// RefreshReferencePricesArgs is the daily job that stores the reference prices
// of every paper the journals hold (decision Р-11): a fund's net asset value
// per unit, and a foreign share's price on its home exchange.
type RefreshReferencePricesArgs struct{}

func (RefreshReferencePricesArgs) Kind() string { return "marketdata.refresh_reference_prices" }

// NAVProvider publishes funds' net asset values per unit.
type NAVProvider interface {
	// Funds is the ticker each fund it values is kept under, by ISIN.
	Funds(ctx context.Context) (map[string]string, error)
	// NAVHistory is a fund's value per unit from from on, oldest first.
	NAVHistory(ctx context.Context, ticker string, from time.Time) ([]DayPrice, error)
	Name() string
}

// ForeignQuoteProvider is a price feed of shares on their home exchanges.
type ForeignQuoteProvider interface {
	// SymbolFor names a share by its ISIN; ok is false when the feed has none.
	SymbolFor(ctx context.Context, isin string) (symbol string, ok bool, err error)
	// Closes is the share's closing prices from from to to, oldest first.
	Closes(ctx context.Context, symbol string, from, to time.Time) ([]DayPrice, error)
	Name() string
}

type referencePricesWorker struct {
	river.WorkerDefaults[RefreshReferencePricesArgs]
	store       *Store
	ops         instrumentFirstDays
	instruments instrumentsByID
	nav         NAVProvider
	foreign     ForeignQuoteProvider
	log         *slog.Logger
	now         func() time.Time
}

// NewReferencePricesWorker builds the River worker that stores reference
// prices. Either provider may be nil, and its kind is then not fetched.
func NewReferencePricesWorker(store *Store, ops instrumentFirstDays, instruments instrumentsByID,
	nav NAVProvider, foreign ForeignQuoteProvider, log *slog.Logger,
) river.Worker[RefreshReferencePricesArgs] {
	return &referencePricesWorker{store: store, ops: ops, instruments: instruments, nav: nav, foreign: foreign, log: log, now: time.Now}
}

func (w *referencePricesWorker) Timeout(*river.Job[RefreshReferencePricesArgs]) time.Duration {
	return backfillTimeout
}

// Work fetches, for every paper the journals hold, the days after the last one
// stored — from a month before its first operation the first time. A fund is
// matched to the NAV source by ISIN, a share to the home-exchange feed by ISIN
// when the ISIN is not Russian. One paper's failure does not stop the others;
// the last error is returned at the end so River retries.
func (w *referencePricesWorker) Work(ctx context.Context, _ *river.Job[RefreshReferencePricesArgs]) error {
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
	navCovered, err := w.store.ReferenceCoverage(ctx, ids, ReferenceNAV)
	if err != nil {
		return err
	}
	foreignCovered, err := w.store.ReferenceCoverage(ctx, ids, ReferenceForeign)
	if err != nil {
		return err
	}

	today := utcDay(w.now())
	var (
		funds   map[string]string
		lastErr error
	)
	for _, id := range ids {
		paper, ok := papers[id]
		if !ok {
			continue
		}
		isin := strings.ToUpper(paper.ISIN)
		from := utcDay(first[id]).AddDate(0, 0, -backfillLeadDays)
		if from.Before(backfillFloor) {
			from = backfillFloor
		}
		var (
			kind     ReferenceKind
			source   string
			days     []DayPrice
			fetchErr error
		)
		switch {
		case paper.Type == instrument.TypeETF && isin != "" && w.nav != nil:
			if funds == nil {
				if funds, err = w.nav.Funds(ctx); err != nil {
					w.log.Warn("marketdata: list the funds with a published NAV failed", "source", w.nav.Name(), "err", err)
					lastErr, funds = err, map[string]string{}
				}
			}
			ticker, ok := funds[isin]
			if !ok {
				continue
			}
			kind, source = ReferenceNAV, w.nav.Name()
			from = after(from, navCovered, id)
			if from.After(today) {
				continue
			}
			days, fetchErr = w.nav.NAVHistory(ctx, ticker, from)
		case paper.Type == instrument.TypeShare && instrument.ForeignISIN(isin) && w.foreign != nil:
			kind, source = ReferenceForeign, w.foreign.Name()
			from = after(from, foreignCovered, id)
			if from.After(today) {
				continue
			}
			symbol, found, err := w.foreign.SymbolFor(ctx, isin)
			if err != nil {
				fetchErr = err
				break
			}
			if !found {
				w.log.Debug("marketdata: no home-exchange price for a foreign share", "instrument", id, "isin", isin)
				continue
			}
			days, fetchErr = w.foreign.Closes(ctx, symbol, from, today)
		default:
			continue
		}
		if fetchErr != nil {
			w.log.Warn("marketdata: fetch reference prices failed", "instrument", id, "kind", kind, "source", source, "err", fetchErr)
			lastErr = fetchErr
			continue
		}
		prices := make([]ReferencePrice, 0, len(days))
		for _, d := range days {
			if d.Day.Before(from) || d.Day.After(today) || !d.Price.IsPositive() {
				continue
			}
			prices = append(prices, ReferencePrice{InstrumentID: id, Kind: kind, On: d.Day, Price: d.Price, Currency: d.Currency, Source: source})
		}
		if err := w.store.UpsertReferencePrices(ctx, prices); err != nil {
			return err
		}
		w.log.Info("marketdata: stored reference prices", "instrument", id, "kind", kind, "source", source, "days", len(prices))
	}
	return lastErr
}

// after is from moved past the last day already stored for id.
func after(from time.Time, covered map[uuid.UUID]time.Time, id uuid.UUID) time.Time {
	if last, ok := covered[id]; ok && !last.Before(from) {
		return last.AddDate(0, 0, 1)
	}
	return from
}
