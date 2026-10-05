package marketdata

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"babki.my/babki/internal/instrument"
)

// RefreshFxArgs refreshes today's FX rates.
type RefreshFxArgs struct{}

func (RefreshFxArgs) Kind() string { return "marketdata.refresh_fx" }

// RefreshQuotesArgs refreshes quotes for every tradable instrument.
type RefreshQuotesArgs struct{}

func (RefreshQuotesArgs) Kind() string { return "marketdata.refresh_quotes" }

// BackfillFxArgs downloads the rate history the journal needs: one request per
// currency in use, covering the oldest operation to today.
type BackfillFxArgs struct{}

func (BackfillFxArgs) Kind() string { return "marketdata.backfill_fx" }

// quoteCurrency is what every stored rate is quoted in; it is never fetched.
const quoteCurrency = "RUB"

// backfillTimeout raises River's one-minute default: each request returns a
// multi-year series (~400 KB for thirteen years of one currency).
const backfillTimeout = 15 * time.Minute

// backfillFloor is the earliest date worth fetching: an older operation is
// more likely a mistyped date than history.
var backfillFloor = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// backfillLeadDays is how far before the earliest operation to fetch; see rangeStart.
const backfillLeadDays = 31

// operationCurrencies is what the backfill needs from operation.Store.
type operationCurrencies interface {
	EarliestRecordedDay(ctx context.Context) (time.Time, error)
	DistinctCurrencies(ctx context.Context) ([]string, error)
}

type accountCurrencies interface {
	DistinctCurrencies(ctx context.Context) ([]string, error)
}

type spaceCurrencies interface {
	DistinctBaseCurrencies(ctx context.Context) ([]string, error)
}

// instrumentLister is what the quotes worker needs from instrument.Store.
type instrumentLister interface {
	ListTradable(ctx context.Context) ([]instrument.Instrument, error)
}

// storableRates drops rates the table refuses (CHECK rate > 0), logging each:
// the upsert is one batch, so one bad rate would fail every currency and River
// would retry into the same poison forever (#28). A dropped pair keeps its
// earlier rate. Only the sign is checked, not NUMERIC overflow.
func storableRates(rates []FxRate, provider string, log *slog.Logger) []FxRate {
	kept := make([]FxRate, 0, len(rates))
	for _, r := range rates {
		if r.Rate.Sign() <= 0 {
			log.Warn("marketdata: source published a rate that is not positive, dropping it (this pair keeps whatever earlier rate it already has)",
				"provider", provider, "base", r.Base, "quote", r.Quote,
				"on", r.On.Format(time.DateOnly), "rate", r.Rate.String())
			continue
		}
		kept = append(kept, r)
	}
	return kept
}

type fxWorker struct {
	river.WorkerDefaults[RefreshFxArgs]
	store    *Store
	provider FxProvider
	log      *slog.Logger
}

// NewFxWorker builds the worker that refreshes daily FX rates.
func NewFxWorker(store *Store, provider FxProvider, log *slog.Logger) river.Worker[RefreshFxArgs] {
	return &fxWorker{store: store, provider: provider, log: log}
}

// Work fetches and stores today's rates. A provider error is returned so River
// retries. Rates the table would refuse are dropped first (see storableRates),
// and the log reports both what was stored and what was dropped.
func (w *fxWorker) Work(ctx context.Context, _ *river.Job[RefreshFxArgs]) error {
	on := time.Now().UTC()
	published, err := w.provider.RatesOn(ctx, on)
	if err != nil {
		w.log.Error("marketdata: fetch fx rates failed", "provider", w.provider.Name(), "err", err)
		return err
	}
	rates := storableRates(published, w.provider.Name(), w.log)
	if err := w.store.UpsertFxRates(ctx, rates); err != nil {
		w.log.Error("marketdata: store fx rates failed", "provider", w.provider.Name(), "err", err)
		return err
	}
	w.log.Info("marketdata: refreshed fx rates", "provider", w.provider.Name(),
		"count", len(rates), "dropped", len(published)-len(rates))
	return nil
}

type quotesWorker struct {
	river.WorkerDefaults[RefreshQuotesArgs]
	store       *Store
	instruments instrumentLister
	provider    QuoteProvider
	log         *slog.Logger
}

// NewQuotesWorker builds the worker that refreshes quotes for tradable instruments.
func NewQuotesWorker(store *Store, instruments instrumentLister, provider QuoteProvider, log *slog.Logger) river.Worker[RefreshQuotesArgs] {
	return &quotesWorker{store: store, instruments: instruments, provider: provider, log: log}
}

// Work prices the tradable catalog. Answers are matched to catalog rows by
// ISIN, falling back to ticker and currency; every instrument that cannot be
// matched is logged with its reason. Each quote is stored under the day the
// provider says its price belongs to, and a quote with no date or dated after
// today is refused: one future row would outrank every later refresh.
//
// It runs every half hour around the clock, so an instrument added today is
// priced before tomorrow; the MOEX price itself is the previous session's.
func (w *quotesWorker) Work(ctx context.Context, _ *river.Job[RefreshQuotesArgs]) error {
	insts, err := w.instruments.ListTradable(ctx)
	if err != nil {
		w.log.Error("marketdata: list tradable instruments failed", "err", err)
		return err
	}
	if len(insts) == 0 {
		w.log.Debug("marketdata: no tradable instruments, skipping quotes refresh")
		return nil
	}

	// Match by ISIN, which names the security: a ticker names a listing, and two
	// exchanges give unrelated companies the same one (AT&T and Т-Технологии are
	// both "T", #26). Ticker and currency is the fallback for rows without an ISIN.
	// The request is still by ticker, which is all the provider speaks.
	byISIN := make(map[string]uuid.UUID, len(insts))
	byTickerCurrency := make(map[tickerCurrency]uuid.UUID, len(insts))
	// ambiguous holds ticker-and-currency pairs shared by two rows: neither is
	// priced by ticker, since picking one would be a guess.
	ambiguous := make(map[tickerCurrency][]uuid.UUID)
	// A row with an ISIN is never matched by ticker: the exchange would have
	// named a different security.
	instISIN := make(map[uuid.UUID]string, len(insts))
	tickers := make([]string, 0, len(insts))
	asked := make(map[string]bool, len(insts))
	for _, inst := range insts {
		if inst.Ticker == "" {
			// An empty ticker means the instrument is not exchange-quoted; nothing to
			// ask. Debug: no instrument loses a price over it.
			w.log.Debug("marketdata: instrument has no ticker, there is nothing to ask a price for",
				"instrument_id", inst.ID)
			continue
		}
		// Register the ISIN before the ticker bookkeeping can skip the row: two rows
		// sharing a ticker are not ambiguous if each has its own ISIN.
		instISIN[inst.ID] = inst.ISIN
		if inst.ISIN != "" {
			// ISINs are unique in the catalog (migration 0020).
			byISIN[inst.ISIN] = inst.ID
		}

		key := tickerCurrency{ticker: inst.Ticker, currency: inst.Currency}
		if priced, taken := byTickerCurrency[key]; taken {
			// The catalog's unique index (migration 0011) makes this impossible for
			// ListTradable, but the worker is handed a list, so it still says so (#26).
			// Warn and carry on: a retry cannot fix it, and failing would stop all pricing.
			// Rows with an ISIN are still priced by it.
			w.log.Warn("marketdata: two instruments share a ticker AND a currency, so neither can be priced by that alone",
				"ticker", inst.Ticker,
				"currency", inst.Currency,
				"instrument_id", priced,
				"other_instrument_id", inst.ID)
			ambiguous[key] = append(ambiguous[key], inst.ID)
			continue
		}
		byTickerCurrency[key] = inst.ID
		if !asked[inst.Ticker] {
			asked[inst.Ticker] = true
			tickers = append(tickers, inst.Ticker)
		}
	}
	// Removed only now, so the first row of a pair does not win by being seen
	// first.
	for key := range ambiguous {
		delete(byTickerCurrency, key)
	}

	tickerQuotes, err := w.provider.QuotesFor(ctx, tickers)
	if err != nil {
		w.log.Error("marketdata: fetch quotes failed", "provider", w.provider.Name(), "err", err)
		return err
	}

	today := utcDay(time.Now())
	seen := make(map[string]bool, len(tickerQuotes))
	quotes := make([]Quote, 0, len(tickerQuotes))
	for _, tq := range tickerQuotes {
		id, ok := byISIN[tq.ISIN]
		if !ok {
			// Fall back to ticker and currency only when one side has no ISIN.
			id, ok = byTickerCurrency[tickerCurrency{ticker: tq.Ticker, currency: tq.Currency}]
			if ok && instISIN[id] != "" && tq.ISIN != "" {
				ok = false
			}
		}
		if !ok {
			// A ticker we did not ask about. Debug, like the "no price" case below: no
			// instrument loses a price over it, but a spelling mismatch shows up here.
			w.log.Debug("marketdata: provider reported a ticker the catalog does not hold, ignoring it",
				"provider", w.provider.Name(), "ticker", tq.Ticker)
			continue
		}
		seen[tq.Ticker] = true
		if tq.On.IsZero() || tq.On.After(today) {
			// Refuse a price with no date or dated after today (as a UTC day): no source
			// has priced a future session. This assumes sources do not date sessions ahead
			// of UTC, true of MOEX. Warn, because an impossible value is what a production
			// log must show. seen is already set, so "no price" does not also fire.
			w.log.Warn("marketdata: provider reported a quote with no date or dated after today, refusing to store it (this instrument keeps whatever earlier quote it already has)",
				"provider", w.provider.Name(), "ticker", tq.Ticker, "on", tq.On.Format(time.DateOnly))
			continue
		}
		quotes = append(quotes, Quote{
			InstrumentID: id,
			On:           tq.On,
			Price:        tq.Price,
			Currency:     tq.Currency,
			Source:       w.provider.Name(),
		})
	}
	for _, t := range tickers {
		if !seen[t] {
			w.log.Debug("marketdata: no price for ticker, skipping", "ticker", t)
		}
	}

	if err := w.store.StoreLatestQuotes(ctx, quotes); err != nil {
		w.log.Error("marketdata: store quotes failed", "err", err)
		return err
	}
	w.log.Info("marketdata: refreshed quotes",
		"provider", w.provider.Name(), "requested", len(tickers), "matched", len(quotes))
	return nil
}

// tickerCurrency is the fallback key for matching a price to a catalog row.
type tickerCurrency struct{ ticker, currency string }

// BackfillGoldArgs downloads the exchange's gold history; the CBR publishes none.
type BackfillGoldArgs struct{}

func (BackfillGoldArgs) Kind() string { return "marketdata.backfill_gold" }

// GoldRateProvider supplies gold rates per gram (see moex.GoldRates).
type GoldRateProvider interface {
	GoldRates(ctx context.Context, from, to time.Time) ([]FxRate, error)
}

// backfillGoldWorker keeps XAU->RUB in the fx table, so gold is valued by the
// same lookup as any currency. It is its own job because the source differs.
type backfillGoldWorker struct {
	river.WorkerDefaults[BackfillGoldArgs]
	store    *Store
	ops      operationCurrencies
	provider GoldRateProvider
	log      *slog.Logger
	now      func() time.Time
}

func NewBackfillGoldWorker(store *Store, ops operationCurrencies, provider GoldRateProvider, log *slog.Logger) river.Worker[BackfillGoldArgs] {
	if log == nil {
		log = slog.Default()
	}
	return &backfillGoldWorker{store: store, ops: ops, provider: provider, log: log, now: time.Now}
}

func (w *backfillGoldWorker) Timeout(*river.Job[BackfillGoldArgs]) time.Duration {
	return backfillTimeout
}

// Work asks for the whole range at once, from a month before the earliest
// operation (see rangeStart); re-running overwrites the same rows.
func (w *backfillGoldWorker) Work(ctx context.Context, _ *river.Job[BackfillGoldArgs]) error {
	earliest, err := w.ops.EarliestRecordedDay(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		w.log.Debug("marketdata: no operations yet, skipping the gold backfill")
		return nil
	}
	if err != nil {
		w.log.Error("marketdata: read earliest operation date failed", "err", err)
		return err
	}
	from := utcDay(earliest).AddDate(0, 0, -backfillLeadDays)
	if from.Before(backfillFloor) {
		from = backfillFloor
	}
	to := utcDay(w.now())
	if from.After(to) {
		from = to
	}

	rates, err := w.provider.GoldRates(ctx, from, to)
	if err != nil {
		w.log.Error("marketdata: fetch gold history failed",
			"from", from.Format(time.DateOnly), "to", to.Format(time.DateOnly), "err", err)
		return err
	}
	if len(rates) == 0 {
		// Nothing over the whole range: gold amounts stay unconverted, which must be
		// visible.
		w.log.Warn("marketdata: the exchange published no gold prices over the whole range (amounts in gold stay unconverted)",
			"from", from.Format(time.DateOnly), "to", to.Format(time.DateOnly))
		return nil
	}
	if err := w.store.UpsertFxRates(ctx, rates); err != nil {
		w.log.Error("marketdata: store gold history failed", "err", err)
		return err
	}
	w.log.Info("marketdata: downloaded gold history",
		"from", from.Format(time.DateOnly), "to", to.Format(time.DateOnly), "rates", len(rates))
	return nil
}

// backfillFxWorker downloads each currency's whole rate history.
type backfillFxWorker struct {
	river.WorkerDefaults[BackfillFxArgs]
	store    *Store
	ops      operationCurrencies
	accounts accountCurrencies
	spaces   spaceCurrencies
	provider FxHistoryProvider
	log      *slog.Logger
	// now is the clock, so tests can pin today.
	now func() time.Time
}

// NewBackfillFxWorker builds the history download worker. ops, accounts and
// spaces supply the oldest date and the currencies in use.
func NewBackfillFxWorker(
	store *Store,
	ops operationCurrencies,
	accounts accountCurrencies,
	spaces spaceCurrencies,
	provider FxHistoryProvider,
	log *slog.Logger,
) river.Worker[BackfillFxArgs] {
	return &backfillFxWorker{
		store: store, ops: ops, accounts: accounts, spaces: spaces,
		provider: provider, log: log, now: time.Now,
	}
}

// Timeout raises River's one-minute default; see backfillTimeout.
func (w *backfillFxWorker) Timeout(*river.Job[BackfillFxArgs]) time.Duration {
	return backfillTimeout
}

// Work downloads every currency's whole series, which also heals any hole an
// outage left. A provider or store error fails the job; earlier currencies'
// rates stay stored.
func (w *backfillFxWorker) Work(ctx context.Context, _ *river.Job[BackfillFxArgs]) error {
	from, earliest, wanted, err := w.rangeStart(ctx)
	if err != nil || !wanted {
		return err
	}

	// Checked before the currency set, so a future-dated operation is reported
	// even when there is nothing to fetch.
	to := utcDay(w.now())
	// Compare the earliest operation itself, not the padded start, which would
	// hide an operation a few days in the future.
	if earliest.After(to) {
		// Usually a typo, though an owner ahead of UTC may legitimately be here.
		// Fetch today only: a backwards range would fail the job forever.
		w.log.Warn("marketdata: earliest operation is in the future, fetching today only",
			"earliest_operation", from.Format(time.DateOnly), "today", to.Format(time.DateOnly))
		from = to
	}

	codes, err := w.wantedCurrencies(ctx)
	if err != nil {
		return err
	}
	if len(codes) == 0 {
		w.log.Debug("marketdata: nothing but the quote currency is in use, skipping fx backfill",
			"quote", quoteCurrency)
		return nil
	}

	ids, err := w.provider.CurrencyIDs(ctx)
	if err != nil {
		w.log.Error("marketdata: fetch currency ids failed", "provider", w.provider.Name(), "err", err)
		return err
	}

	for _, code := range codes {
		// Gold is fetched from the exchange (see backfillGoldWorker), not reported as
		// unquoted.
		if code == GoldCode {
			continue
		}
		id, ok := ids[code]
		if !ok {
			// The source does not quote this currency: its amounts stay unconverted, and
			// the log says so.
			w.log.Warn("marketdata: source does not quote currency, skipping it (its amounts stay unconverted)",
				"provider", w.provider.Name(), "currency", code)
			continue
		}
		published, err := w.provider.RatesRange(ctx, code, id, from, to)
		if err != nil {
			w.log.Error("marketdata: fetch fx history failed",
				"provider", w.provider.Name(), "currency", code,
				"from", from.Format(time.DateOnly), "to", to.Format(time.DateOnly), "err", err)
			return err
		}
		// Checked before filtering: it is a claim about the source, and checked after
		// it would blame the source for values it did publish.
		if len(published) == 0 {
			// An identifier with no rates over the whole range was most likely retired;
			// as visible as an unquoted currency.
			w.log.Warn("marketdata: source published no rates for currency over the whole range (its amounts stay unconverted)",
				"provider", w.provider.Name(), "currency", code, "id", id,
				"from", from.Format(time.DateOnly), "to", to.Format(time.DateOnly))
			continue
		}
		rates := storableRates(published, w.provider.Name(), w.log)
		if err := w.store.UpsertFxRates(ctx, rates); err != nil {
			w.log.Error("marketdata: store fx history failed",
				"provider", w.provider.Name(), "currency", code, "err", err)
			return err
		}
		if len(rates) == 0 {
			// Every rate refused: as loud as an empty series, with its own reason, in one
			// line.
			w.log.Warn("marketdata: every rate the source published for this currency was refused as not positive (its amounts keep whatever earlier rates they already have)",
				"provider", w.provider.Name(), "currency", code, "id", id,
				"from", from.Format(time.DateOnly), "to", to.Format(time.DateOnly),
				"published", len(published))
			continue
		}
		w.log.Info("marketdata: downloaded fx history",
			"provider", w.provider.Name(), "currency", code,
			"from", from.Format(time.DateOnly), "to", to.Format(time.DateOnly),
			"rates", len(rates), "dropped", len(published)-len(rates))
	}
	return nil
}

// rangeStart returns the day to fetch from (a month before the earliest
// operation, clamped to backfillFloor) and the earliest operation's day.
// wanted is false when there are no operations.
func (w *backfillFxWorker) rangeStart(ctx context.Context) (time.Time, time.Time, bool, error) {
	earliest, err := w.ops.EarliestRecordedDay(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		w.log.Debug("marketdata: no operations yet, skipping fx backfill")
		return time.Time{}, time.Time{}, false, nil
	}
	if err != nil {
		w.log.Error("marketdata: read earliest operation date failed", "err", err)
		return time.Time{}, time.Time{}, false, err
	}
	// Start a month early: rates are looked up by nearest earlier date and
	// published on business days, so an operation before the first row would
	// never convert. The owner's first operation (2020-10-26) preceded the CBR's
	// first row in range (the 27th). A month covers the New Year gap.
	day := utcDay(earliest)
	from := day.AddDate(0, 0, -backfillLeadDays)
	if from.Before(backfillFloor) {
		w.log.Warn("marketdata: earliest operation predates the fx backfill floor, clamping (most likely a mistyped date)",
			"provider", w.provider.Name(),
			"earliest_operation", from.Format(time.DateOnly),
			"floor", backfillFloor.Format(time.DateOnly),
			"days_dropped", daysBetween(from, backfillFloor))
		from = backfillFloor
	}
	return from, day, true, nil
}

// wantedCurrencies is the sorted set of currencies of accounts, operations and
// space base currencies, minus quoteCurrency.
func (w *backfillFxWorker) wantedCurrencies(ctx context.Context) ([]string, error) {
	accountCodes, err := w.accounts.DistinctCurrencies(ctx)
	if err != nil {
		w.log.Error("marketdata: read account currencies failed", "err", err)
		return nil, err
	}
	operationCodes, err := w.ops.DistinctCurrencies(ctx)
	if err != nil {
		w.log.Error("marketdata: read operation currencies failed", "err", err)
		return nil, err
	}
	baseCodes, err := w.spaces.DistinctBaseCurrencies(ctx)
	if err != nil {
		w.log.Error("marketdata: read space base currencies failed", "err", err)
		return nil, err
	}

	total := len(accountCodes) + len(operationCodes) + len(baseCodes)
	seen := make(map[string]bool, total)
	out := make([]string, 0, total)
	for _, codes := range [][]string{accountCodes, operationCodes, baseCodes} {
		for _, code := range codes {
			if code == quoteCurrency || seen[code] {
				continue
			}
			seen[code] = true
			out = append(out, code)
		}
	}
	slices.Sort(out)
	return out, nil
}

// utcDay truncates to midnight UTC, matching Postgres DATE values.
func utcDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// daysBetween counts calendar days; both ends are midnight UTC.
func daysBetween(from, to time.Time) int {
	return int(to.Sub(from).Hours() / 24)
}
