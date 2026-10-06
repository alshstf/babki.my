package corporateaction

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata/yahoo"
)

// RefreshForeignSplitsArgs asks a share feed which foreign papers in the
// journals were split and records the answer in the registry: the exchange's
// table knows only its own listings, and a split nobody records leaves a
// holding short by its ratio (AMZN 20:1 in 2022, NVDA 10:1 in 2024).
type RefreshForeignSplitsArgs struct{}

func (RefreshForeignSplitsArgs) Kind() string { return "corporateaction.refresh_foreign_splits" }

// ForeignSplitsProvider is the part of *yahoo.Client this worker needs.
type ForeignSplitsProvider interface {
	SymbolFor(ctx context.Context, isin string) (string, bool, error)
	Splits(ctx context.Context, symbol string, from, to time.Time) ([]yahoo.Split, error)
	SplitsURL(symbol string) string
}

// heldPapers is what the journals hold and since when.
type heldPapers interface {
	FirstDaysByInstrument(ctx context.Context) (map[uuid.UUID]time.Time, error)
}

// papersByID reads catalog rows; *instrument.Store satisfies it.
type papersByID interface {
	ByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]instrument.Instrument, error)
}

type refreshForeignSplitsWorker struct {
	river.WorkerDefaults[RefreshForeignSplitsArgs]
	store        *Store
	materializer *Materializer
	held         heldPapers
	papers       papersByID
	feed         ForeignSplitsProvider
	log          *slog.Logger
	now          func() time.Time
}

// NewRefreshForeignSplitsWorker builds the worker; with a nil feed it records
// nothing.
func NewRefreshForeignSplitsWorker(store *Store, materializer *Materializer, held heldPapers, papers papersByID,
	feed ForeignSplitsProvider, log *slog.Logger,
) river.Worker[RefreshForeignSplitsArgs] {
	if log == nil {
		log = slog.Default()
	}
	return &refreshForeignSplitsWorker{
		store: store, materializer: materializer, held: held, papers: papers, feed: feed, log: log, now: time.Now,
	}
}

func (w *refreshForeignSplitsWorker) Timeout(*river.Job[RefreshForeignSplitsArgs]) time.Duration {
	return refreshTimeout
}

// Work records, for every foreign share and fund the journals hold, the splits
// since its first entry, and brings the changed papers' journals into line. A
// ratio that is not of whole numbers is the feed's price adjustment for a
// spin-off, not a split, and is left out. One paper's failure does not stop the
// others.
func (w *refreshForeignSplitsWorker) Work(ctx context.Context, _ *river.Job[RefreshForeignSplitsArgs]) error {
	if w.feed == nil {
		return nil
	}
	first, err := w.held.FirstDaysByInstrument(ctx)
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, 0, len(first))
	for id := range first {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	papers, err := w.papers.ByIDs(ctx, ids)
	if err != nil {
		return err
	}
	today := w.now().UTC()
	var (
		failed       []error
		stored, kept int
		touched      = map[string]bool{}
		asked        = map[string]bool{}
	)
	for _, id := range ids {
		paper, ok := papers[id]
		isin := strings.ToUpper(paper.ISIN)
		if !ok || (paper.Type != instrument.TypeShare && paper.Type != instrument.TypeETF) ||
			!instrument.ForeignISIN(isin) || asked[isin] {
			continue
		}
		asked[isin] = true
		symbol, found, err := w.feed.SymbolFor(ctx, isin)
		if err != nil {
			w.log.Warn("corporateaction: look a foreign paper up for its splits failed", "isin", isin, "err", err)
			failed = append(failed, err)
			continue
		}
		if !found {
			continue
		}
		splits, err := w.feed.Splits(ctx, symbol, first[id].AddDate(0, -1, 0), today)
		if err != nil {
			w.log.Warn("corporateaction: fetch a foreign paper's splits failed", "isin", isin, "symbol", symbol, "err", err)
			failed = append(failed, err)
			continue
		}
		for _, s := range splits {
			if !s.Numerator.IsInteger() || !s.Denominator.IsInteger() {
				w.log.Debug("corporateaction: a ratio of fractions is a price adjustment, not a split",
					"isin", isin, "on", s.On.Format(time.DateOnly), "numerator", s.Numerator, "denominator", s.Denominator)
				continue
			}
			e := Event{
				Kind: KindSplit, ISIN: isin, EffectiveOn: s.On,
				RatioFrom: s.Denominator.IntPart(), RatioTo: s.Numerator.IntPart(),
				Source: SourceYahoo, SourceRef: w.feed.SplitsURL(symbol),
			}
			if err := e.Validate(); err != nil {
				w.log.Debug("corporateaction: the feed published a split this registry will not hold", "isin", isin, "err", err)
				continue
			}
			_, written, err := w.store.Upsert(ctx, e)
			if err != nil {
				return err
			}
			if !written {
				// The exchange's or a person's record of the same day is kept.
				kept++
				continue
			}
			stored++
			touched[isin] = true
		}
	}

	var totals Stats
	for isin := range touched {
		s, err := w.materializer.ForISIN(ctx, isin)
		totals.add(s)
		if err != nil {
			w.log.Error("corporateaction: carrying a split into the journals failed", "isin", isin, "err", err)
			failed = append(failed, err)
		}
	}
	w.materializer.RequestRecheck(ctx, totals)
	w.log.Info("corporateaction: refreshed foreign papers' splits",
		"papers", len(asked), "stored", stored, "left_to_other_records", kept,
		"journal_rows_added", totals.Added, "journal_rows_removed", totals.Removed)
	return errors.Join(failed...)
}
