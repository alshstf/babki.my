package marketdata

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
)

// RefreshCryptoPricesArgs is the daily job that prices the cryptocurrencies
// the journals hold from a coin feed (decision Р-20): the last year's daily
// prices and today's.
type RefreshCryptoPricesArgs struct{}

func (RefreshCryptoPricesArgs) Kind() string { return "marketdata.refresh_crypto_prices" }

// CryptoQuoteProvider is a coin price feed; *coingecko.Client satisfies it.
type CryptoQuoteProvider interface {
	// CoinFor names the coin a ticker most likely means; ok is false for none.
	CoinFor(ctx context.Context, ticker string) (coin string, ok bool, err error)
	// History is a coin's daily prices in currency from from to till, oldest first.
	History(ctx context.Context, coin, currency string, from, till time.Time) ([]DayPrice, error)
	// Current is the coins' latest prices in currency, by coin.
	Current(ctx context.Context, coins []string, currency string) (map[string]DayPrice, error)
	// HistoryWindow is how far back History answers.
	HistoryWindow() time.Duration
	Name() string
}

// coinCatalog reads the papers and keeps the coin picked for one.
type coinCatalog interface {
	ByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]instrument.Instrument, error)
	Update(ctx context.Context, id uuid.UUID, upd instrument.Update) (instrument.Instrument, error)
}

type cryptoPricesWorker struct {
	river.WorkerDefaults[RefreshCryptoPricesArgs]
	store   *Store
	ops     instrumentFirstDays
	catalog coinCatalog
	feed    CryptoQuoteProvider
	log     *slog.Logger
	now     func() time.Time
}

// NewCryptoPricesWorker builds the River worker; with a nil feed it prices
// nothing.
func NewCryptoPricesWorker(store *Store, ops instrumentFirstDays, catalog coinCatalog, feed CryptoQuoteProvider,
	log *slog.Logger,
) river.Worker[RefreshCryptoPricesArgs] {
	return &cryptoPricesWorker{store: store, ops: ops, catalog: catalog, feed: feed, log: log, now: time.Now}
}

func (w *cryptoPricesWorker) Timeout(*river.Job[RefreshCryptoPricesArgs]) time.Duration {
	return backfillTimeout
}

// Work prices every cryptocurrency the journals hold. A paper with no coin
// gets the one its ticker most likely names, kept for next time. The whole
// window is asked again each day: the feed's last year is one request a coin.
// One coin's failure does not stop the others; the last error is returned so
// River retries.
func (w *cryptoPricesWorker) Work(ctx context.Context, _ *river.Job[RefreshCryptoPricesArgs]) error {
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
	papers, err := w.catalog.ByIDs(ctx, ids)
	if err != nil {
		return err
	}

	today := utcDay(w.now())
	window := today.Add(-w.feed.HistoryWindow())
	var (
		lastErr error
		stored  int
		// byCurrency is the coins to price today, per currency they are kept in.
		byCurrency = map[string]map[string][]uuid.UUID{}
	)
	for _, id := range ids {
		paper, ok := papers[id]
		if !ok || paper.Type != instrument.TypeCrypto {
			continue
		}
		coin := paper.CoinGeckoID
		if coin == "" {
			found, ok, err := w.feed.CoinFor(ctx, paper.Ticker)
			if err != nil {
				w.log.Warn("marketdata: look a coin up by its ticker failed", "ticker", paper.Ticker, "err", err)
				lastErr = err
				continue
			}
			if !ok {
				w.log.Info("marketdata: the coin feed knows no coin by this ticker", "ticker", paper.Ticker)
				continue
			}
			if _, err := w.catalog.Update(ctx, id, instrument.Update{CoinGeckoID: &found}); err != nil {
				return err
			}
			coin = found
		}
		currency := strings.ToUpper(paper.Currency)
		if byCurrency[currency] == nil {
			byCurrency[currency] = map[string][]uuid.UUID{}
		}
		byCurrency[currency][coin] = append(byCurrency[currency][coin], id)

		from := utcDay(first[id])
		if from.Before(window) {
			from = window
		}
		days, err := w.feed.History(ctx, coin, currency, from, today)
		if err != nil {
			w.log.Warn("marketdata: fetch a coin's prices failed", "coin", coin, "currency", currency, "err", err)
			lastErr = err
			continue
		}
		quotes := make([]Quote, 0, len(days))
		for _, d := range days {
			quotes = append(quotes, Quote{InstrumentID: id, On: d.Day, Price: d.Price, Currency: currency, Source: w.feed.Name()})
		}
		if err := w.store.UpsertQuotes(ctx, quotes); err != nil {
			return err
		}
		stored += len(quotes)
	}

	for currency, coins := range byCurrency {
		names := make([]string, 0, len(coins))
		for coin := range coins {
			names = append(names, coin)
		}
		slices.Sort(names)
		prices, err := w.feed.Current(ctx, names, currency)
		if err != nil {
			w.log.Warn("marketdata: fetch coins' prices for today failed", "currency", currency, "err", err)
			lastErr = errors.Join(lastErr, err)
			continue
		}
		var quotes []Quote
		for coin, p := range prices {
			for _, id := range coins[coin] {
				quotes = append(quotes, Quote{InstrumentID: id, On: p.Day, Price: p.Price, Currency: currency, Source: w.feed.Name()})
			}
		}
		if err := w.store.UpsertQuotes(ctx, quotes); err != nil {
			return err
		}
		stored += len(quotes)
	}
	w.log.Info("marketdata: priced the cryptocurrencies", "source", w.feed.Name(), "quotes", stored)
	return lastErr
}
