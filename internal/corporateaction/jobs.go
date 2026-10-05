package corporateaction

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"babki.my/babki/internal/marketdata/moex"
)

// RefreshMoexSplitsArgs asks the exchange which securities were split and
// records the answer in the registry.
type RefreshMoexSplitsArgs struct{}

func (RefreshMoexSplitsArgs) Kind() string { return "corporateaction.refresh_moex_splits" }

// MaterializeAllArgs re-derives every registry row (see Materializer.All).
type MaterializeAllArgs struct{}

func (MaterializeAllArgs) Kind() string { return "corporateaction.materialize_all" }

// MaterializeISINArgs carries one paper's events into its holders' journals
// when the request that recorded or deleted an event could not. Unique by ISIN:
// a run recomputes the whole paper.
type MaterializeISINArgs struct {
	ISIN string `json:"isin" river:"unique"`
}

func (MaterializeISINArgs) Kind() string { return "corporateaction.materialize_isin" }

// materializeISINAttempts: River waits attempt⁴ seconds, so eight attempts
// span about 78 minutes; the daily sweep stands behind them.
const materializeISINAttempts = 8

// MaterializeISINInsertOpts queues one job per paper among unfinished jobs,
// with bounded attempts.
func MaterializeISINInsertOpts() *river.InsertOpts {
	return &river.InsertOpts{
		MaxAttempts: materializeISINAttempts,
		UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
			rivertype.JobStateAvailable,
			rivertype.JobStatePending,
			rivertype.JobStateRetryable,
			rivertype.JobStateRunning,
			rivertype.JobStateScheduled,
		}},
	}
}

// jobInserter is the queue as this package uses it.
type jobInserter interface {
	Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

type materializeISINWorker struct {
	river.WorkerDefaults[MaterializeISINArgs]
	materializer *Materializer
	log          *slog.Logger
}

func NewMaterializeISINWorker(materializer *Materializer, log *slog.Logger) river.Worker[MaterializeISINArgs] {
	if log == nil {
		log = slog.Default()
	}
	return &materializeISINWorker{materializer: materializer, log: log}
}

func (w *materializeISINWorker) Timeout(*river.Job[MaterializeISINArgs]) time.Duration {
	return refreshTimeout
}

// Work brings the paper's holders into line. A failed account is retried by
// the queue; recheck is requested now for the accounts that succeeded.
func (w *materializeISINWorker) Work(ctx context.Context, job *river.Job[MaterializeISINArgs]) error {
	stats, err := w.materializer.ForISIN(ctx, job.Args.ISIN)
	w.materializer.RequestRecheck(ctx, stats)
	if err != nil {
		w.log.Error("corporateaction: carrying an event into the journals failed again",
			"isin", job.Args.ISIN, "attempt", job.Attempt, "err", err)
		return err
	}
	w.log.Info("corporateaction: an event that could not be applied on the spot is now in the journals",
		"isin", job.Args.ISIN, "added", stats.Added, "removed", stats.Removed, "refused", stats.Refused)
	return nil
}

// SplitsProvider is the part of *moex.Client this worker needs.
type SplitsProvider interface {
	Splits(ctx context.Context) ([]moex.Split, error)
	ISINBySecID(ctx context.Context, secid string) (string, error)
}

// refreshTimeout overrides River's one-minute default: a first run may resolve
// every unseen secid (56 rows on 2026-08-22).
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

// Work records every split the exchange publishes and brings the changed
// papers' journals into line. Secids are resolved through the exchange, never
// this catalog (a ticker is not an identity: AT&T and Т-Технологии are both "T"),
// and cached in the rows they produced. A security the exchange will not identify
// is skipped and counted.
func (w *refreshMoexSplitsWorker) Work(ctx context.Context, _ *river.Job[RefreshMoexSplitsArgs]) error {
	splits, err := w.provider.Splits(ctx)
	if err != nil {
		w.log.Error("corporateaction: fetch the exchange's splits failed", "err", err)
		return err
	}
	if len(splits) == 0 {
		// The table has only grown since 2021, so an empty answer means a changed
		// endpoint. Record nothing: an empty registry would un-split every
		// holding.
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
			// A row this registry would refuse from a person (1:1, a future date)
			// is refused from the exchange too.
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
			// A hand-recorded event of the same paper, kind and day is kept.
			kept++
			continue
		}
		stored++
		touched[isin] = true
	}

	// Every paper goes as far as it can; failures are reported at the end.
	var totals Stats
	var failed []error
	for isin := range touched {
		s, err := w.materializer.ForISIN(ctx, isin)
		totals.add(s)
		if err != nil {
			w.log.Error("corporateaction: carrying a split into the journals failed", "isin", isin, "err", err)
			failed = append(failed, err)
		}
	}

	w.materializer.RequestRecheck(ctx, totals)

	w.log.Info("corporateaction: refreshed the exchange's splits",
		"published", len(splits), "stored", stored, "left_to_hand_records", kept, "unidentified", skipped,
		"journal_rows_added", totals.Added, "journal_rows_removed", totals.Removed)
	return errors.Join(failed...)
}

// splitsSourceRef is the evidence link of an exchange row: the table it
// came from.
const splitsSourceRef = "/iss/statistics/engines/stock/splits.json"

// knownISINs is the secid -> ISIN cache, read from rows earlier runs wrote.
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
	// Before the error check: a sweep that failed on one account changed the
	// others.
	w.materializer.RequestRecheck(ctx, stats)
	if err != nil {
		w.log.Error("corporateaction: the registry sweep failed", "err", err)
		return err
	}
	// Debug on a no-op day, so the day it does something stands out.
	if stats.Added == 0 && stats.Removed == 0 && stats.Refused == 0 {
		w.log.Debug("corporateaction: the registry sweep found every journal already in line")
		return nil
	}
	w.log.Info("corporateaction: the registry sweep changed journals",
		"added", stats.Added, "removed", stats.Removed, "refused", stats.Refused)
	return nil
}
