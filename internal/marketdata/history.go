package marketdata

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
)

// BackfillQuotesArgs downloads the closing prices of every paper in the
// journals, from its first operation to today.
type BackfillQuotesArgs struct{}

func (BackfillQuotesArgs) Kind() string { return "marketdata.backfill_quotes" }

// QuoteHistorySource marks a closing price from the exchange's history.
const QuoteHistorySource = "moex_history"

// DayPrice is one day's closing price of a security on an exchange.
type DayPrice struct {
	Day      time.Time
	Price    decimal.Decimal
	Currency string
	// Bond is a bond's face and accrued interest that day, without its paper,
	// day or source; nil for anything else or when not stated.
	Bond *BondDay
}

// HistoryProvider is an exchange's price history.
type HistoryProvider interface {
	// Locate names the exchange's security for an ISIN or a ticker, the market
	// its history is kept under and the currency it trades in; ok is false
	// when the exchange has none on a board this program prices.
	Locate(ctx context.Context, code string) (secid, market, currency string, ok bool, err error)
	// History is that security's closing prices from from to till, oldest first.
	History(ctx context.Context, market, secid string, from, till time.Time) ([]DayPrice, error)
}

type instrumentFirstDays interface {
	FirstDaysByInstrument(ctx context.Context) (map[uuid.UUID]time.Time, error)
}

type instrumentsByID interface {
	ByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]instrument.Instrument, error)
}

type backfillQuotesWorker struct {
	river.WorkerDefaults[BackfillQuotesArgs]
	store       *Store
	ops         instrumentFirstDays
	instruments instrumentsByID
	provider    HistoryProvider
	log         *slog.Logger
	now         func() time.Time
}

// NewBackfillQuotesWorker builds the River worker that downloads closing
// prices for the papers in the journals and stores them.
func NewBackfillQuotesWorker(store *Store, ops instrumentFirstDays, instruments instrumentsByID, provider HistoryProvider, log *slog.Logger) river.Worker[BackfillQuotesArgs] {
	return &backfillQuotesWorker{store: store, ops: ops, instruments: instruments, provider: provider, log: log, now: time.Now}
}

// Timeout raises River's one-minute default: the first run asks for years of
// history of every paper, a hundred days a request.
func (w *backfillQuotesWorker) Timeout(*river.Job[BackfillQuotesArgs]) time.Duration {
	return backfillTimeout
}

// Work fetches, for every paper the journals hold, the days after the last
// one already downloaded — from a month before its first operation the first
// time. A paper is found on the exchange by its ISIN, or by its ticker when it
// has none, and only when it trades there in the paper's own currency: a
// ticker alone names different companies on different exchanges.
func (w *backfillQuotesWorker) Work(ctx context.Context, _ *river.Job[BackfillQuotesArgs]) error {
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
	covered, err := w.store.HistoryCoverage(ctx, ids, QuoteHistorySource)
	if err != nil {
		return err
	}
	// A bond's history is complete only with its face and interest, which were
	// not kept at first: one downloaded before them is downloaded again.
	bondCovered, err := w.store.BondDaysCoverage(ctx, ids, QuoteHistorySource)
	if err != nil {
		return err
	}
	today := utcDay(w.now())
	for _, id := range ids {
		paper, ok := papers[id]
		if !ok || !historyKind(paper.Type) {
			continue
		}
		from := utcDay(first[id]).AddDate(0, 0, -backfillLeadDays)
		if from.Before(backfillFloor) {
			from = backfillFloor
		}
		last, ok := covered[id]
		if paper.Type == instrument.TypeBond {
			last, ok = bondCovered[id]
		}
		if ok && !last.Before(from) {
			from = last.AddDate(0, 0, 1)
		}
		if from.After(today) {
			continue
		}
		code := paper.ISIN
		if code == "" {
			code = paper.Ticker
		}
		secid, market, currency, found, err := w.provider.Locate(ctx, code)
		if err != nil {
			return err
		}
		if !found || currency != paper.Currency {
			w.log.Debug("marketdata: paper has no price history on the exchange",
				"instrument", id, "code", code, "found", found, "currency", currency)
			continue
		}
		days, err := w.provider.History(ctx, market, secid, from, today)
		if err != nil {
			w.log.Error("marketdata: fetch quote history failed", "instrument", id, "secid", secid, "err", err)
			return err
		}
		quotes := make([]Quote, 0, len(days))
		var bondDays []BondDay
		for _, d := range days {
			quotes = append(quotes, Quote{InstrumentID: id, On: d.Day, Price: d.Price, Currency: d.Currency, Source: QuoteHistorySource})
			if d.Bond != nil && paper.Type == instrument.TypeBond {
				b := *d.Bond
				b.InstrumentID, b.On, b.Source = id, d.Day, QuoteHistorySource
				bondDays = append(bondDays, b)
			}
		}
		if err := w.store.UpsertQuotes(ctx, quotes); err != nil {
			return err
		}
		if err := w.store.UpsertBondDays(ctx, bondDays); err != nil {
			return err
		}
		w.log.Info("marketdata: downloaded quote history", "instrument", id, "secid", secid,
			"from", from.Format(time.DateOnly), "to", today.Format(time.DateOnly), "days", len(quotes))
	}
	return nil
}

// historyKind reports whether the exchange keeps a price history for a kind
// of paper.
func historyKind(t instrument.Type) bool {
	return t == instrument.TypeShare || t == instrument.TypeBond || t == instrument.TypeETF
}
