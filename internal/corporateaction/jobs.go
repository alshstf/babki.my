package corporateaction

import (
	"context"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"babki.my/babki/internal/marketdata/moex"
)

// RefreshMoexSplitsArgs asks the exchange what securities have been divided,
// and records what it says in the registry. Kind is namespaced with the
// module's name so job kinds from different modules cannot collide in the
// shared queue.
type RefreshMoexSplitsArgs struct{}

func (RefreshMoexSplitsArgs) Kind() string { return "corporateaction.refresh_moex_splits" }

// MaterializeAllArgs re-derives every journal row the registry asks for. See
// Materializer.All: it is the safety net behind the synchronous triggers, not
// the normal path.
type MaterializeAllArgs struct{}

func (MaterializeAllArgs) Kind() string { return "corporateaction.materialize_all" }

// SplitsProvider is the slice of the exchange client this worker needs;
// *moex.Client satisfies it structurally. Narrow and local for the same reason
// marketdata declares its own: what this package wants of the exchange is two
// calls.
type SplitsProvider interface {
	Splits(ctx context.Context) ([]moex.Split, error)
	ISINBySecID(ctx context.Context, secid string) (string, error)
}

// refreshTimeout overrides River's one-minute default. A run reads one small
// table and then asks the exchange to identify each security it has not
// identified before — 56 rows on 2026-08-22, so at most 56 small requests on
// the very first run and none at all on a run that learns nothing new.
const refreshTimeout = 10 * time.Minute

type refreshMoexSplitsWorker struct {
	river.WorkerDefaults[RefreshMoexSplitsArgs]
	store        *Store
	materializer *Materializer
	provider     SplitsProvider
	log          *slog.Logger
}

func NewRefreshMoexSplitsWorker(store *Store, materializer *Materializer,
	provider SplitsProvider, log *slog.Logger,
) river.Worker[RefreshMoexSplitsArgs] {
	if log == nil {
		log = slog.Default()
	}
	return &refreshMoexSplitsWorker{store: store, materializer: materializer, provider: provider, log: log}
}

func (w *refreshMoexSplitsWorker) Timeout(*river.Job[RefreshMoexSplitsArgs]) time.Duration {
	return refreshTimeout
}

// Work records every split the exchange publishes and then brings the journals
// of the papers that changed into line.
//
// THE SECID IS RESOLVED THROUGH THE EXCHANGE, NEVER THROUGH THIS CATALOG. See
// moex.ISINBySecID: a ticker is not an identity, and the owner's own catalog
// holds AT&T and Т-Технологии both under "T".
//
// The resolution is cached in the row it produced, so a secid the registry has
// already seen costs nothing on later runs. The cache is read from the registry
// itself rather than kept in memory: a worker is per run, and the whole point
// is that the second run is cheap.
//
// A security the exchange will not identify is skipped and counted. Recording
// the split against the secid instead is exactly the wrong repair — it would
// key a fact about a security to a code that names a different security on
// another exchange.
func (w *refreshMoexSplitsWorker) Work(ctx context.Context, _ *river.Job[RefreshMoexSplitsArgs]) error {
	splits, err := w.provider.Splits(ctx)
	if err != nil {
		w.log.Error("corporateaction: fetch the exchange's splits failed", "err", err)
		return err
	}
	if len(splits) == 0 {
		// The table has held tens of rows since 2021 and only ever grows, so
		// an empty answer is a changed endpoint rather than a quiet quarter.
		// Warn and record nothing: an empty registry silently un-splits every
		// holding it had been correcting.
		w.log.Warn("corporateaction: the exchange published no splits at all, which its table has never done")
		return nil
	}

	known, err := w.knownISINs(ctx)
	if err != nil {
		return err
	}

	var stored, skipped, kept int
	touched := map[string]bool{}
	for _, s := range splits {
		isin, ok := known[s.SecID]
		if !ok {
			resolved, err := w.provider.ISINBySecID(ctx, s.SecID)
			if err != nil {
				w.log.Debug("corporateaction: the exchange would not say what one of its own secids is",
					"secid", s.SecID, "err", err)
				skipped++
				continue
			}
			if resolved == "" {
				w.log.Debug("corporateaction: the exchange names no ISIN for a security it published a split for",
					"secid", s.SecID)
				skipped++
				continue
			}
			isin = resolved
			known[s.SecID] = isin
		}
		e := Event{
			Kind:        KindSplit,
			ISIN:        isin,
			EffectiveOn: s.EffectiveOn,
			RatioFrom:   s.From,
			RatioTo:     s.To,
			Source:      SourceMOEX,
			SourceRef:   moex.DefaultBaseURL + splitsSourceRef,
			MOEXSecID:   s.SecID,
		}
		if err := e.Validate(); err != nil {
			// The exchange's own rules are not this program's, and a row it
			// publishes that this registry would refuse from a person is
			// refused from the exchange too — a ratio of 1 to 1, a date in the
			// future. Skipped and counted rather than stored unchecked.
			w.log.Debug("corporateaction: the exchange published a split this registry will not hold",
				"secid", s.SecID, "isin", isin, "err", err)
			skipped++
			continue
		}
		_, written, err := w.store.Upsert(ctx, e)
		if err != nil {
			w.log.Error("corporateaction: store a split failed", "secid", s.SecID, "isin", isin, "err", err)
			return err
		}
		if !written {
			// A hand-recorded event of the same paper, kind and day is already
			// there. Somebody wrote down what a registrar told them, with a
			// link to it; the exchange's table does not overrule that.
			kept++
			continue
		}
		stored++
		touched[isin] = true
	}

	var totals Stats
	for isin := range touched {
		s, err := w.materializer.ForISIN(ctx, isin)
		if err != nil {
			w.log.Error("corporateaction: carrying a split into the journals failed", "isin", isin, "err", err)
			return err
		}
		totals.add(s)
	}

	w.log.Info("corporateaction: refreshed the exchange's splits",
		"published", len(splits), "stored", stored, "left_to_hand_records", kept, "unidentified", skipped,
		"journal_rows_added", totals.Added, "journal_rows_removed", totals.Removed)
	return nil
}

// splitsSourceRef is what a row written from the exchange links to as its
// evidence: the very table it was read from.
const splitsSourceRef = "/iss/statistics/engines/stock/splits.json"

// knownISINs is the secid -> ISIN cache, read back from the rows earlier runs
// wrote. Only rows the exchange job itself wrote carry a secid, so nothing a
// person recorded can teach this cache anything.
func (w *refreshMoexSplitsWorker) knownISINs(ctx context.Context) (map[string]string, error) {
	events, err := w.store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(events))
	for _, e := range events {
		if e.MOEXSecID != "" && e.ISIN != "" {
			out[e.MOEXSecID] = e.ISIN
		}
	}
	return out, nil
}

type materializeAllWorker struct {
	river.WorkerDefaults[MaterializeAllArgs]
	materializer *Materializer
	log          *slog.Logger
}

func NewMaterializeAllWorker(materializer *Materializer, log *slog.Logger) river.Worker[MaterializeAllArgs] {
	if log == nil {
		log = slog.Default()
	}
	return &materializeAllWorker{materializer: materializer, log: log}
}

func (w *materializeAllWorker) Timeout(*river.Job[MaterializeAllArgs]) time.Duration {
	return refreshTimeout
}

func (w *materializeAllWorker) Work(ctx context.Context, _ *river.Job[MaterializeAllArgs]) error {
	stats, err := w.materializer.All(ctx)
	if err != nil {
		w.log.Error("corporateaction: the registry sweep failed", "err", err)
		return err
	}
	// Debug when it did nothing, which is what a healthy instance does every
	// day: the sweep exists to catch a trigger that failed, and a daily Info
	// line saying "nothing" would bury the day it says something.
	if stats.Added == 0 && stats.Removed == 0 && stats.Refused == 0 {
		w.log.Debug("corporateaction: the registry sweep found every journal already in line")
		return nil
	}
	w.log.Info("corporateaction: the registry sweep changed journals",
		"added", stats.Added, "removed", stats.Removed, "refused", stats.Refused)
	return nil
}
