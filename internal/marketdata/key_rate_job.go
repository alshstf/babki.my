package marketdata

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// RefreshKeyRatesArgs keeps the key rate: its whole history the first time,
// the last week after.
type RefreshKeyRatesArgs struct{}

func (RefreshKeyRatesArgs) Kind() string { return "marketdata.refresh_key_rates" }

// KeyRateProvider supplies the key rate over a range (see cbr.Client.KeyRates).
type KeyRateProvider interface {
	KeyRates(ctx context.Context, from, to time.Time) ([]KeyRate, error)
}

// keyRateFloor is the key rate's first day.
var keyRateFloor = time.Date(2013, 9, 13, 0, 0, 0, 0, time.UTC)

type keyRatesWorker struct {
	river.WorkerDefaults[RefreshKeyRatesArgs]
	store    *Store
	provider KeyRateProvider
	log      *slog.Logger
	now      func() time.Time
}

func NewKeyRatesWorker(store *Store, provider KeyRateProvider, log *slog.Logger) river.Worker[RefreshKeyRatesArgs] {
	if log == nil {
		log = slog.Default()
	}
	return &keyRatesWorker{store: store, provider: provider, log: log, now: time.Now}
}

func (w *keyRatesWorker) Timeout(*river.Job[RefreshKeyRatesArgs]) time.Duration {
	return backfillTimeout
}

// Work asks for one range: from the rate's first day while nothing is
// stored, else from a week before the last stored day.
func (w *keyRatesWorker) Work(ctx context.Context, _ *river.Job[RefreshKeyRatesArgs]) error {
	to := utcDay(w.now())
	from := keyRateFloor
	latest, err := w.store.LatestKeyRateDay(ctx)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		w.log.Error("marketdata: read the last key rate day failed", "err", err)
		return err
	default:
		from = utcDay(latest).AddDate(0, 0, -metalRefetchDays)
	}
	if from.After(to) {
		from = to
	}
	rates, err := w.provider.KeyRates(ctx, from, to)
	if err != nil {
		w.log.Error("marketdata: fetch key rates failed",
			"from", from.Format(time.DateOnly), "to", to.Format(time.DateOnly), "err", err)
		return err
	}
	if err := w.store.UpsertKeyRates(ctx, rates); err != nil {
		w.log.Error("marketdata: store key rates failed", "err", err)
		return err
	}
	w.log.Info("marketdata: downloaded key rates",
		"from", from.Format(time.DateOnly), "to", to.Format(time.DateOnly), "rates", len(rates))
	return nil
}
