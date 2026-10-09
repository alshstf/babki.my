package tinvest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/jobs"
	"babki.my/babki/internal/platform/secretbox"
)

// HistorySource is the quotes source of a close from the broker's daily
// candles.
const HistorySource = "tinvest_history"

// candleWindow: the broker answers a year of daily candles per request.
const candleWindow = 365

// historyLeadDays: history starts this many days before a paper's first
// operation, so that day has a price.
const historyLeadDays = 31

// DayClose is one trading day's closing price of an instrument at the broker.
type DayClose struct {
	Day   time.Time
	Close decimal.Decimal
}

type wireGetCandlesResponse struct {
	Candles       []wireCandle `json:"candles"`
	PriceCurrency string       `json:"priceCurrency"`
}

type wireCandle struct {
	Close      *wireQuotation `json:"close"`
	Time       string         `json:"time"`
	IsComplete bool           `json:"isComplete"`
}

// DailyCloses returns an instrument's daily closes from from to to and their
// currency, leaving out unclosed days.
func (c *Client) DailyCloses(ctx context.Context, instrumentUID string, from, to time.Time) ([]DayClose, string, error) {
	var resp wireGetCandlesResponse
	req := struct {
		InstrumentID string `json:"instrumentId"`
		From         string `json:"from"`
		To           string `json:"to"`
		Interval     string `json:"interval"`
	}{instrumentUID, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339), "CANDLE_INTERVAL_DAY"}
	if err := c.do(ctx, "MarketDataService/GetCandles", req, &resp); err != nil {
		return nil, "", err
	}
	out := make([]DayClose, 0, len(resp.Candles))
	for _, w := range resp.Candles {
		if !w.IsComplete || w.Close == nil {
			continue
		}
		q, err := w.Close.parse()
		if err != nil {
			return nil, "", fmt.Errorf("tinvest: candle of %s: %w", instrumentUID, err)
		}
		at, err := parseWireTime(w.Time)
		if err != nil {
			return nil, "", fmt.Errorf("tinvest: candle time of %s: %w", instrumentUID, err)
		}
		if price := q.Decimal(); price.IsPositive() {
			out = append(out, DayClose{Day: mskDay(at), Close: price})
		}
	}
	return out, strings.ToUpper(resp.PriceCurrency), nil
}

// BackfillQuotesArgs asks for closes of connection papers the exchange
// history does not cover, foreign papers above all.
type BackfillQuotesArgs struct{}

func (BackfillQuotesArgs) Kind() string { return "tinvest.backfill_quotes" }

type historyQuotes interface {
	UpsertQuotes(ctx context.Context, quotes []marketdata.Quote) error
	HistoryCoverage(ctx context.Context, ids []uuid.UUID, source string) (map[uuid.UUID]time.Time, error)
}

type firstDays interface {
	FirstDaysByInstrument(ctx context.Context) (map[uuid.UUID]time.Time, error)
}

type backfillQuotesWorker struct {
	river.WorkerDefaults[BackfillQuotesArgs]
	store     *Store
	quotes    historyQuotes
	ops       firstDays
	box       *secretbox.Box
	newClient clientFactory
	log       *slog.Logger
	now       func() time.Time
}

// NewBackfillQuotesWorker builds the worker that downloads daily candles for
// every active connection's listings.
func NewBackfillQuotesWorker(store *Store, quotes historyQuotes, ops firstDays, box *secretbox.Box, newClient clientFactory, log *slog.Logger) river.Worker[BackfillQuotesArgs] {
	return &backfillQuotesWorker{store: store, quotes: quotes, ops: ops, box: box, newClient: newClient, log: log, now: time.Now}
}

// Timeout: a first run asks for years of every listing, a year per
// request.
func (w *backfillQuotesWorker) Timeout(*river.Job[BackfillQuotesArgs]) time.Duration {
	return 15 * time.Minute
}

// Work fetches, for each listing of each active connection whose paper is in a
// journal without exchange history, the days after the last downloaded one (from
// a month before the first operation the first time), and likewise for papers
// the space holds that no import mapped. Prices are stored only in the listing's
// own currency.
func (w *backfillQuotesWorker) Work(ctx context.Context, _ *river.Job[BackfillQuotesArgs]) error {
	conns, err := w.store.ListActiveConnections(ctx)
	if err != nil {
		return err
	}
	if len(conns) == 0 {
		return nil
	}
	first, err := w.ops.FirstDaysByInstrument(ctx)
	if err != nil {
		return err
	}
	var lastErr error
	for _, conn := range conns {
		if err := w.backfillConnection(ctx, conn, first); err != nil {
			if errors.Is(err, ErrTokenInvalid) {
				w.log.Warn("tinvest: the broker rejected this connection's token while fetching history",
					"connection_id", conn.ID)
				continue
			}
			lastErr = err
			w.log.Error("tinvest: fetching a connection's price history failed", "connection_id", conn.ID, "err", err)
		}
	}
	return lastErr
}

func (w *backfillQuotesWorker) backfillConnection(ctx context.Context, conn Connection, first map[uuid.UUID]time.Time) error {
	listings, err := w.store.QuotableByConnection(ctx, conn.ID)
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, 0, len(listings))
	for _, l := range listings {
		ids = append(ids, l.InstrumentID)
	}
	fromExchange, err := w.quotes.HistoryCoverage(ctx, ids, marketdata.QuoteHistorySource)
	if err != nil {
		return err
	}
	covered, err := w.quotes.HistoryCoverage(ctx, ids, HistorySource)
	if err != nil {
		return err
	}
	token, err := w.box.Open(conn.TokenCiphertext)
	if err != nil {
		return fmt.Errorf("tinvest: open token of connection %s: %w", conn.ID, err)
	}
	client, err := w.newClient(string(token))
	if err != nil {
		return err
	}
	today := mskDay(w.now())
	for i, l := range listings {
		jobs.ProgressFrom(ctx).Stage(ctx, "quotes", i, len(listings))
		day, held := first[l.InstrumentID]
		if !held || l.Currency == "" {
			continue
		}
		if _, ok := fromExchange[l.InstrumentID]; ok {
			continue
		}
		if err := w.fetch(ctx, client, l.InstrumentID, l.InstrumentUID, l.Currency, l.Bond, w.since(day, covered[l.InstrumentID]), today); err != nil {
			return err
		}
	}
	return w.backfillUnmapped(ctx, conn, client, first, today)
}

// backfillUnmapped fetches history for papers no import mapped (entered by
// hand or from another broker's file), at the listing the price worker finds by
// ISIN (resolveListing); bounded per run.
func (w *backfillQuotesWorker) backfillUnmapped(ctx context.Context, conn Connection, client *Client, first map[uuid.UUID]time.Time, today time.Time) error {
	want, err := w.store.UnmappedHeldInstruments(ctx, conn.SpaceID, conn.ID)
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, 0, len(want))
	for _, u := range want {
		ids = append(ids, u.InstrumentID)
	}
	fromExchange, err := w.quotes.HistoryCoverage(ctx, ids, marketdata.QuoteHistorySource)
	if err != nil {
		return err
	}
	covered, err := w.quotes.HistoryCoverage(ctx, ids, HistorySource)
	if err != nil {
		return err
	}
	searched := 0
	for _, u := range want {
		day, held := first[u.InstrumentID]
		if !held {
			continue
		}
		if _, ok := fromExchange[u.InstrumentID]; ok {
			continue
		}
		if searched == unmappedSearchesPerRun {
			w.log.Info("tinvest: more unmapped holdings than one run searches for, the rest wait for the next",
				"connection_id", conn.ID)
			break
		}
		searched++
		listing, _, currency, ok, err := resolveListing(ctx, client, w.log, u)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		bond := u.Type == string(instrument.TypeBond)
		if err := w.fetch(ctx, client, u.InstrumentID, listing.UID, currency, bond, w.since(day, covered[u.InstrumentID]), today); err != nil {
			return err
		}
	}
	return nil
}

// since is the first day to fetch.
func (w *backfillQuotesWorker) since(firstOperation, lastFetched time.Time) time.Time {
	from := mskDay(firstOperation).AddDate(0, 0, -historyLeadDays)
	if !lastFetched.IsZero() && !lastFetched.Before(from) {
		from = lastFetched.AddDate(0, 0, 1)
	}
	return from
}

// bondCandleUnit is how the broker names the unit of a bond's candles: points,
// a percentage of the face, as its last prices for bonds are (quotes.go).
const bondCandleUnit = "PT."

// fetch stores a listing's daily closes from from to today in the listing's
// currency; candles in another currency are skipped. A bond's candles are in
// points and are stored like its current price, as a percentage the valuation
// applies to the face: skipping them left a bond the exchange has no history
// for (a redeemed one, one in yuan) without any past price.
func (w *backfillQuotesWorker) fetch(ctx context.Context, client *Client, instrumentID uuid.UUID, uid, listingCurrency string,
	bond bool, from, today time.Time,
) error {
	for start := from; !start.After(today); start = start.AddDate(0, 0, candleWindow) {
		end := start.AddDate(0, 0, candleWindow)
		if tomorrow := today.AddDate(0, 0, 1); end.After(tomorrow) {
			end = tomorrow
		}
		closes, currency, err := client.DailyCloses(ctx, uid, start, end)
		if err != nil {
			return err
		}
		inPoints := bond && currency == bondCandleUnit
		if currency != "" && currency != listingCurrency && !inPoints {
			w.log.Warn("tinvest: candles in another currency than the listing's, not stored",
				"instrument_uid", uid, "candles", currency, "listing", listingCurrency)
			return nil
		}
		quotes := make([]marketdata.Quote, 0, len(closes))
		for _, c := range closes {
			if c.Day.After(today) {
				continue
			}
			quotes = append(quotes, marketdata.Quote{
				InstrumentID: instrumentID, On: c.Day, Price: c.Close, Currency: listingCurrency, Source: HistorySource,
			})
		}
		if err := w.quotes.UpsertQuotes(ctx, quotes); err != nil {
			return err
		}
	}
	return nil
}
