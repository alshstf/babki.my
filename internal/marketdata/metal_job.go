package marketdata

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// RefreshMetalPricesArgs keeps the metals' accounting prices: their whole
// history the first time, the last week after (decisions Р-39, Р-40).
type RefreshMetalPricesArgs struct{}

func (RefreshMetalPricesArgs) Kind() string { return "marketdata.refresh_metal_prices" }

// MetalPriceProvider supplies the metals' accounting prices over a range
// (see cbr.Client.MetalPrices).
type MetalPriceProvider interface {
	MetalPrices(ctx context.Context, from, to time.Time) ([]MetalPrice, error)
}

// metalRefetchDays is how far back a refresh reaches past the last stored
// day: the bank may restate a price, and a missed day is caught up.
const metalRefetchDays = 7

type metalPricesWorker struct {
	river.WorkerDefaults[RefreshMetalPricesArgs]
	store    *Store
	provider MetalPriceProvider
	log      *slog.Logger
	now      func() time.Time
}

func NewMetalPricesWorker(store *Store, provider MetalPriceProvider, log *slog.Logger) river.Worker[RefreshMetalPricesArgs] {
	if log == nil {
		log = slog.Default()
	}
	return &metalPricesWorker{store: store, provider: provider, log: log, now: time.Now}
}

func (w *metalPricesWorker) Timeout(*river.Job[RefreshMetalPricesArgs]) time.Duration {
	return backfillTimeout
}

// Work asks for one range: from the floor while nothing is stored, else from
// a week before the last stored day; re-running overwrites the same rows.
func (w *metalPricesWorker) Work(ctx context.Context, _ *river.Job[RefreshMetalPricesArgs]) error {
	to := utcDay(w.now())
	from := backfillFloor
	latest, err := w.store.LatestMetalPriceDay(ctx)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		w.log.Error("marketdata: read the last metal price day failed", "err", err)
		return err
	default:
		from = utcDay(latest).AddDate(0, 0, -metalRefetchDays)
	}
	if from.After(to) {
		from = to
	}
	prices, err := w.provider.MetalPrices(ctx, from, to)
	if err != nil {
		w.log.Error("marketdata: fetch metal prices failed",
			"from", from.Format(time.DateOnly), "to", to.Format(time.DateOnly), "err", err)
		return err
	}
	if err := w.store.UpsertMetalPrices(ctx, prices); err != nil {
		w.log.Error("marketdata: store metal prices failed", "err", err)
		return err
	}
	w.log.Info("marketdata: downloaded metal prices",
		"from", from.Format(time.DateOnly), "to", to.Format(time.DateOnly), "prices", len(prices))
	return nil
}
