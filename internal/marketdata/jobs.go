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

// RefreshFxArgs triggers a refresh of today's FX rates from FxProvider. Kind
// is namespaced with "marketdata." so job kinds registered by different
// domain modules never collide in the shared River queue.
type RefreshFxArgs struct{}

func (RefreshFxArgs) Kind() string { return "marketdata.refresh_fx" }

// RefreshQuotesArgs triggers a refresh of quotes, via QuoteProvider, for
// every tradable instrument in the catalog.
type RefreshQuotesArgs struct{}

func (RefreshQuotesArgs) Kind() string { return "marketdata.refresh_quotes" }

// BackfillFxArgs triggers a download of the whole FX rate history the
// journal needs: for every currency actually in use, one request covering the
// entire range from the oldest operation to today.
type BackfillFxArgs struct{}

func (BackfillFxArgs) Kind() string { return "marketdata.backfill_fx" }

// quoteCurrency is the currency every stored rate is quoted in ("USD -> RUB"
// and so on). It is never downloaded: the rouble against itself is not a
// rate, and the source does not quote it.
const quoteCurrency = "RUB"

// backfillTimeout overrides River's one-minute default job timeout. A run
// makes one request per currency in use — under a dozen in practice, and
// bounded by the fifty-odd currencies the source quotes at all — but each
// answer is a whole multi-year series: measured at ~400KB for thirteen years
// of one currency, versus ~2KB for a single day's document. At the 15s
// per-request timeout cmd/babki gives the cbr client, this leaves room for
// far more currencies than any instance will have.
const backfillTimeout = 15 * time.Minute

// backfillFloor is the earliest date worth fetching rates for. An operation
// dated before it is far more likely to be a mistyped date than real
// history, and honouring it would mean asking for decades of series nothing
// will ever be converted at.
var backfillFloor = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// backfillLeadDays is how far before the earliest operation the history is
// fetched from, so that a nearest-earlier lookup always has something to land
// on. See rangeStart for why it is not zero and why a month.
const backfillLeadDays = 31

// operationCurrencies is the subset of operation.Store the backfill worker
// needs — the same locally declared interface pattern as instrumentLister
// below, which additionally keeps marketdata free of an import of the
// operation package. *operation.Store satisfies it structurally.
type operationCurrencies interface {
	EarliestOccurredOn(ctx context.Context) (time.Time, error)
	DistinctCurrencies(ctx context.Context) ([]string, error)
}

// accountCurrencies is the subset of account.Store the backfill worker needs;
// *account.Store satisfies it structurally.
type accountCurrencies interface {
	DistinctCurrencies(ctx context.Context) ([]string, error)
}

// spaceCurrencies is the subset of family.Store the backfill worker needs;
// *family.Store satisfies it structurally.
type spaceCurrencies interface {
	DistinctBaseCurrencies(ctx context.Context) ([]string, error)
}

// instrumentLister is the subset of instrument.Store the quotes worker
// needs. Declared locally rather than depending on *instrument.Store
// directly — the same pattern journalStore uses in portfolio/http.go — so
// this package only commits to the one method it actually calls.
// *instrument.Store satisfies it structurally; callers pass it in with no
// conversion needed.
type instrumentLister interface {
	ListTradable(ctx context.Context) ([]instrument.Instrument, error)
}

// storableRates is every rate in rates that the fx_rates table will accept,
// plus one log line for each one it leaves behind.
//
// fx_rates.rate carries CHECK (rate > 0) (migration 0006), and UpsertFxRates
// queues the whole set as one pgx batch, which Postgres runs inside a single
// implicit transaction. So ONE unusable rate does not cost one currency, it
// costs every currency in the call: nothing at all is written. River then
// retries the job, the source answers with the identical set, and it fails
// again — the same poison for as long as the source keeps publishing it (#28).
// Neither zero nor a negative is far-fetched: zero is what a source can put
// against a currency it has stopped quoting, and nothing between the wire and
// here has ever checked the sign.
//
// The alternative was to let runBatch tolerate a per-row failure, and it was
// rejected on two counts. It would make a partial write the ordinary outcome
// of a store method whose entire contract is "these rows are now stored",
// for quotes as well as rates; and it would still have to say something about
// the row it dropped, from a place that knows only an SQLSTATE — not which
// source, which currency, or what value. Filtering inside the cbr client was
// rejected for the mirror reason: that package has no logger, and dropping a
// rate there without a word is the silence this one exists to prevent.
//
// Dropping a rate is a loss of data, so each one gets a line, at Warn rather
// than Debug because Debug is off exactly where this can happen. What the line
// says is bounded by what is true: the pair is NOT left unconvertible, because
// FxRateOn resolves the nearest earlier date, so it keeps whatever earlier
// rate it already has (possibly none, if this was to be its first).
//
// Only the CHECK is answered here, and deliberately: a rate whose integer part
// overflows NUMERIC(30,10) would poison a batch in the same way, and this does
// not catch it. Saying "every row the table would refuse" would be a wider
// claim than the code makes.
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

// fxWorker refreshes today's FX rates via FxProvider and stores them.
type fxWorker struct {
	river.WorkerDefaults[RefreshFxArgs]
	store    *Store
	provider FxProvider
	log      *slog.Logger
}

// NewFxWorker builds the River worker that refreshes daily FX rates via
// provider and upserts them into store. Register it with river.AddWorker.
func NewFxWorker(store *Store, provider FxProvider, log *slog.Logger) river.Worker[RefreshFxArgs] {
	return &fxWorker{store: store, provider: provider, log: log}
}

// Work fetches today's rates and upserts them. A provider error is returned
// (not swallowed) so River retries the job per its backoff policy; it is
// also logged here since River's own failure log lacks provider context.
//
// Rates the table would refuse are dropped before the upsert (see
// storableRates) so that one of them cannot cost the whole day's set. The
// count reported here is what was stored, and `dropped` is what it took to
// get there, so the two add up to what the source published rather than
// leaving a shrunken figure to explain itself.
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

// quotesWorker refreshes quotes for every tradable instrument via
// QuoteProvider and stores them.
type quotesWorker struct {
	river.WorkerDefaults[RefreshQuotesArgs]
	store       *Store
	instruments instrumentLister
	provider    QuoteProvider
	log         *slog.Logger
}

// NewQuotesWorker builds the River worker that refreshes quotes, via
// provider, for every tradable instrument (share/bond/etf with a ticker)
// and upserts them into store. Register it with river.AddWorker.
func NewQuotesWorker(store *Store, instruments instrumentLister, provider QuoteProvider, log *slog.Logger) river.Worker[RefreshQuotesArgs] {
	return &quotesWorker{store: store, instruments: instruments, provider: provider, log: log}
}

// Work looks up the tradable instrument catalog, maps ticker -> InstrumentID
// (the provider knows nothing about our catalog, so this mapping is the
// worker's job), fetches prices, and upserts the ones the provider actually
// reported. Instruments the provider has no price for are skipped with a
// debug log — that's an expected, routine condition (e.g. newly listed or
// suspended instruments), not an error. A provider error is returned (not
// swallowed) so River retries the job.
//
// Every instrument the mapping cannot carry leaves a line behind it, saying
// which of the three reasons it was. Two instruments under one ticker are a
// Warn, because one of them will never be priced while both exist; an
// instrument with no ticker at all, and a ticker the catalog does not hold, are
// Debug, because neither costs any instrument a price. The last two used to be
// dropped in silence, which is how a position could show no quote with no trace
// of the reason anywhere.
//
// Each stored quote carries the day the PROVIDER says its price belongs to,
// never this worker's clock — see marketdata.TickerQuote.On. A repeat refresh
// therefore rewrites one row per instrument instead of adding a row a day, and
// what LatestQuotes then returns is the exchange's own most recent session
// rather than the most recent time this job happened to run.
//
// A quote dated zero or after today is refused rather than stored, here and
// not in the provider. QuotesFor deliberately takes no date argument any
// more (see QuoteProvider.QuotesFor), precisely so a provider cannot invent a
// day of its own — which means it also has no "today" to compare a price's
// date against and catch a glitch like this. The worker does have one. The
// check earns its place because LatestQuotes is `DISTINCT ON (instrument_id)
// ... ORDER BY on_date DESC`: a single row dated, say, a year from now would
// outrank every genuine refresh that follows it for the whole of that year,
// silently, on every position the instrument appears in — a failure mode the
// old time.Now()-stamped quotes could not produce at all, since nothing ever
// asked the exchange what day it thought it was.
//
// The job is enqueued every half hour around the clock and asks on every one
// of those runs — roughly 48 requests a day for a value that moves about once
// a trading day, since the MOEX provider reads a previous-session price (see
// its QuotesFor for what that is exactly). A night-and-weekend window was
// tried here and removed: it was justified by session hours MOEX does not
// keep — there is a morning session from 06:50 MSK and there are weekend
// sessions — so it clipped real trading while still not making the stored
// value any fresher.
//
// The cadence is deliberately left as it is. A full refresh costs about 170 KB
// (all four boards, measured 2026-08-03), and asking often is the only thing that gives an
// instrument added today a price before tomorrow. What #90 was really about —
// the price being stored under the wrong day — is fixed at the provider, not
// by asking less often. Whether to show a price that moves intraday instead of
// the previous session's is a separate product question, and the owner's
// answer for now is to keep the previous session's.
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

	// MATCHED BY ISIN, WHICH NAMES THE SECURITY. A ticker names a LISTING of
	// one: two exchanges give unrelated companies the same ticker — the owner's
	// catalog holds AT&T under "T" and Т-Технологии under "T" — and this map
	// used to hold one of them and silently drop the other (#26).
	//
	// Ticker AND CURRENCY was the first answer to that and is not enough
	// either: two exchanges inside one currency zone hand the same ticker to
	// two companies in the same euros, and nothing about that pair would
	// separate them. The exchange sends the ISIN alongside every price and
	// always did; this program simply did not ask for the column.
	//
	// The ticker map stays as the SECOND key, for catalog rows carrying no ISIN
	// — hand-entered papers, and the kinds no exchange assigns one to. There the
	// ticker really is all there is, and the currency beside it is the most that
	// can be asked of it.
	//
	// The REQUEST is still by ticker, because that is all the provider's
	// interface speaks; the identifiers only enter when the answers come back.
	byISIN := make(map[string]uuid.UUID, len(insts))
	byTickerCurrency := make(map[tickerCurrency]uuid.UUID, len(insts))
	// ambiguous names the pairs no answer can be attached to: two catalog rows
	// with the same ticker AND the same currency. Neither is priced — picking
	// one would be a coin toss over which paper a real price belongs to — and
	// the pair is reported once rather than per answer.
	ambiguous := make(map[tickerCurrency][]uuid.UUID)
	// What each row's ISIN is, so the ticker fallback can refuse to answer for a
	// row that HAS one: there the exchange has named a different security, and
	// a match on the letters would be the very confusion the ISIN settles.
	instISIN := make(map[uuid.UUID]string, len(insts))
	tickers := make([]string, 0, len(insts))
	asked := make(map[string]bool, len(insts))
	for _, inst := range insts {
		if inst.Ticker == "" {
			// The empty string is how "this instrument has no exchange ticker"
			// is written down — cash, hand-made holdings, anything an exchange
			// does not quote — and there is nothing in it to ask a provider
			// for. ListTradable excludes such rows, so this is the same
			// "whatever produced the list" case as the collision below, but a
			// different fact, and the collision's line would state it wrongly:
			// several tickerless rows share no ticker, and none of them is
			// priced either way, so "only one of them can be priced" would be
			// false of all of them.
			//
			// Debug, like the unknown-ticker case further down: no instrument
			// loses a price it could otherwise have had.
			w.log.Debug("marketdata: instrument has no ticker, there is nothing to ask a price for",
				"instrument_id", inst.ID)
			continue
		}
		// THE ISIN IS REGISTERED FIRST AND UNCONDITIONALLY, before any of the
		// ticker bookkeeping below can `continue` past it. Two rows sharing a
		// ticker are not ambiguous when each carries its own ISIN — that is the
		// whole point of matching by one — and an early exit here left the
		// second of such a pair out of the ISIN map entirely, so the exchange's
		// answer about it found nothing.
		instISIN[inst.ID] = inst.ISIN
		if inst.ISIN != "" {
			// One row per ISIN is a rule the database keeps (migration 0020),
			// so this cannot overwrite anything — and if it ever could, the
			// answer would belong to whichever row it named, not to a map.
			byISIN[inst.ISIN] = inst.ID
		}

		key := tickerCurrency{ticker: inst.Ticker, currency: inst.Currency}
		if priced, taken := byTickerCurrency[key]; taken {
			// instruments.ticker carries a partial unique index (migration
			// 0011) over exactly what ListTradable returns, so a read of the
			// catalog cannot produce this any more. The check is here all the
			// same because this worker is handed a LIST, not a table: whatever
			// produced the list — a ListTradable widened to a type the index
			// leaves alone, where two rows reading BTC are legitimate, a second
			// catalog source, an index dropped by hand — a mapping step that
			// loses one of its entries has to say so.
			// Overwriting in silence was the whole of issue #26: the loser was
			// never priced again and nothing anywhere said why.
			//
			// Warn and carry on rather than failing the job: a retry cannot
			// un-duplicate a ticker, so River would retry forever and every
			// other instrument would stop being priced too — one wrong number
			// traded for all of them.
			// Only the ticker fallback is affected: each of these rows is
			// still reachable by its own ISIN, and this says what is lost for
			// the ones that carry none.
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
	// Removed only now, so that the FIRST row of an ambiguous pair does not keep
	// the price by having been seen first. Both go unpriced or neither does.
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
			// No ISIN on one side or the other: fall back to the listing's own
			// name. A row that HAS an ISIN and did not match by it is not this
			// paper — the exchange named a different security — so the fallback
			// is reached only when one of the two carries none.
			id, ok = byTickerCurrency[tickerCurrency{ticker: tq.Ticker, currency: tq.Currency}]
			if ok && instISIN[id] != "" && tq.ISIN != "" {
				ok = false
			}
		}
		if !ok {
			// A ticker we didn't ask about; ignored rather than failed over,
			// but no longer dropped without a word — the sibling case ("we
			// asked and got no price", below) has always logged, and silence
			// has to be a decision rather than an oversight.
			//
			// Debug, and deliberately the same level as that sibling: nothing
			// of ours goes unvalued because of this line, since an instrument
			// that did go unpriced reports itself below, and a louder level
			// would put something nobody can act on into every production log.
			// It still earns a line: a provider spelling a ticker differently
			// from the catalog is visible here and nowhere else.
			w.log.Debug("marketdata: provider reported a ticker the catalog does not hold, ignoring it",
				"provider", w.provider.Name(), "ticker", tq.Ticker)
			continue
		}
		seen[tq.Ticker] = true
		if tq.On.IsZero() || tq.On.After(today) {
			// A price with no date, or one dated after today (today taken as a
			// UTC day, with zero tolerance either side), is refused as a claim
			// that cannot be true — a source has no session in the future to
			// have priced it as of. That assumes a source dates its sessions by
			// a day that does not run ahead of UTC; every provider wired in so
			// far (MOEX) does. A source east of UTC that dates a quote by its
			// own local day — a market in UTC+10..+13, say — could publish a
			// same-day quote this check would see as still in the future and
			// refuse; nothing here has been built or tested against such a
			// source, so this comparison is a standing assumption about the
			// providers this worker has, not a fact proven of every provider it
			// could ever have. That is a different kind of absence from "the
			// provider had nothing to say about this ticker" (Debug, below):
			// here the provider DID answer, with a value this worker believes
			// cannot be right, so it is refused rather than trusted and stored.
			// Warn, not Debug: an ordinary missing price is routine (a new
			// listing, a suspension), but a claim that looks impossible is
			// exactly the kind of thing Debug being off in production would
			// hide.
			//
			// seen was already set above so the "no price for ticker" line
			// below does not also fire for it — the provider did report
			// something, just not something storable.
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

	if err := w.store.UpsertQuotes(ctx, quotes); err != nil {
		w.log.Error("marketdata: store quotes failed", "err", err)
		return err
	}
	w.log.Info("marketdata: refreshed quotes",
		"provider", w.provider.Name(), "requested", len(tickers), "matched", len(quotes))
	return nil
}

// tickerCurrency is what a price is matched to a catalog row by. See the map
// built in refreshQuotesWorker.Work for why the ticker alone will not do.
type tickerCurrency struct{ ticker, currency string }

// BackfillGoldArgs asks for the exchange's gold history, which the central bank
// does not publish at all.
type BackfillGoldArgs struct{}

func (BackfillGoldArgs) Kind() string { return "marketdata.backfill_gold" }

// GoldRateProvider is the subset of the exchange client this worker needs. See
// moex.GoldRates for why the rate it returns is per GRAM and why that matters.
type GoldRateProvider interface {
	GoldRates(ctx context.Context, from, to time.Time) ([]FxRate, error)
}

// backfillGoldWorker keeps XAU->RUB in the same fx table every other currency
// lives in, so that nothing downstream needs to know gold is special: a cash
// balance in XAU is valued by the same lookup a balance in dollars is.
//
// IT IS A SEPARATE JOB FROM THE CENTRAL BANK'S because it is a separate SOURCE.
// The Bank of Russia publishes no gold rate — the backfill there reports XAU as
// a currency it does not quote — and the exchange, which does, speaks a
// different protocol entirely. Folding a second source into that worker would
// have made "the provider" mean two things at once.
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

// Work asks for the whole range at once, exactly as the currency backfill does:
// one request heals any hole an outage left, and re-running overwrites the same
// rows.
//
// The range starts a month before the earliest operation for the reason
// rangeStart states at length — a rate is looked up by nearest EARLIER date, and
// gold trades on business days like everything else.
func (w *backfillGoldWorker) Work(ctx context.Context, _ *river.Job[BackfillGoldArgs]) error {
	earliest, err := w.ops.EarliestOccurredOn(ctx)
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
		// The exchange answered and published nothing across the whole range.
		// Warn rather than Debug, for the reason the currency backfill warns
		// about a currency its source does not quote: amounts in gold stay
		// unconverted, and that has to be visible rather than look like a
		// normal run.
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

// backfillFxWorker downloads the FX rate history the journal needs: the
// whole range at once, one request per currency in use.
type backfillFxWorker struct {
	river.WorkerDefaults[BackfillFxArgs]
	store    *Store
	ops      operationCurrencies
	accounts accountCurrencies
	spaces   spaceCurrencies
	provider FxHistoryProvider
	log      *slog.Logger
	// now stands in for time.Now so tests can pin "today" — the upper end of
	// every requested range — instead of racing the wall clock.
	now func() time.Time
}

// NewBackfillFxWorker builds the River worker that downloads historical FX
// rates via provider and upserts them into store. ops supplies the oldest
// date the journal needs rates for; ops, accounts and spaces together supply
// the currencies those rates are needed in. Register it with river.AddWorker.
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

// Work downloads every currency's whole series and stores it. There is no
// bookkeeping of what is already covered and no follow-up job: a run always
// asks for the full range, which costs one request per currency and heals
// any hole an outage may have left. Re-running simply overwrites the same
// rows.
//
// A provider or store error fails the job so River retries it; whatever was
// stored for the currencies handled before the failure stays in the database.
func (w *backfillFxWorker) Work(ctx context.Context, _ *river.Job[BackfillFxArgs]) error {
	from, earliest, wanted, err := w.rangeStart(ctx)
	if err != nil || !wanted {
		return err
	}

	// Checked before the currency set, and so before the early exit below:
	// a future-dated operation is a data problem in its own right, and must
	// be logged even on an instance where every account and operation is in
	// RUB and there ends up being nothing left to fetch.
	to := utcDay(w.now())
	// AGAINST THE EARLIEST OPERATION, not against the padded start. The lead
	// above would otherwise swallow this: an operation ten days in the future
	// leaves a start three weeks in the past, the range no longer inverts, and
	// a date nobody can have meant goes unreported.
	if earliest.After(to) {
		// The earliest operation is dated after today. Usually a typo, but not
		// always: operation validation allows a day of leeway, so an owner
		// well ahead of UTC entering their local "today" lands here
		// legitimately. Either way, left alone it would make every run ask for
		// a backwards range, which the source rejects, so the job would fail
		// forever instead of keeping the rest of the history fresh.
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
		// Gold is asked of the exchange instead, which is the only source that
		// quotes it — the central bank publishes no rate for XAU at all. Skipped
		// here rather than left to the warning below, because that warning
		// promises the amounts "stay unconverted", and for this one code they
		// do not (see backfillGoldWorker).
		if code == GoldCode {
			continue
		}
		id, ok := ids[code]
		if !ok {
			// The source doesn't quote this currency, so there is no series to
			// ask for. Operations in it stay unconverted, which is the honest
			// outcome — but it must be visible, not silent.
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
		// Asked BEFORE anything is dropped, because it is a claim about the
		// SOURCE and only the source's own answer can settle it. Checked after
		// the filter it would fire for a currency the source published plenty
		// for, sending whoever reads it to look at cbr.ru instead of at the
		// values — the number right, the reason wrong, which is the mistake
		// this repository keeps having to undo.
		if len(published) == 0 {
			// The source has an identifier for this currency yet published
			// nothing across the entire range — its identifier has most
			// likely been retired and re-issued. The outcome matches a
			// currency the source doesn't quote at all (amounts stay
			// unconverted), so it deserves the same visibility rather than
			// an Info line reading "rates=0" that looks like a normal run.
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
			// Nothing survived. The currency ends up exactly where an empty
			// series leaves it, so it is as loud — but for its own reason,
			// which the line above would have stated wrongly. The individual
			// records already have their own lines; this one is here so that a
			// whole currency going missing is one line and not a count the
			// reader has to do.
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

// rangeStart is the oldest date the journal needs rates for: the earliest
// operation, clamped to backfillFloor. wanted is false when there are no
// operations at all — then no amount needs converting at any past date, and
// the source is not contacted even once.
// rangeStart returns the day the history is fetched FROM and the day of the
// EARLIEST OPERATION, which are no longer the same day (see the lead below) and
// answer two different questions: what to ask the source for, and whether the
// journal's own dates make sense at all.
func (w *backfillFxWorker) rangeStart(ctx context.Context) (time.Time, time.Time, bool, error) {
	earliest, err := w.ops.EarliestOccurredOn(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		w.log.Debug("marketdata: no operations yet, skipping fx backfill")
		return time.Time{}, time.Time{}, false, nil
	}
	if err != nil {
		w.log.Error("marketdata: read earliest operation date failed", "err", err)
		return time.Time{}, time.Time{}, false, err
	}
	// A RUN OF DAYS BEFORE THE EARLIEST OPERATION, and it is not slack.
	//
	// A rate is looked up by nearest EARLIER date (Store.FxRateOn), so an
	// operation dated before the first row in the table has nothing to fall
	// back to and stays unconvertible for ever. Asking the source for exactly
	// the operation's own day is not enough: rates are published on business
	// days, so a purchase on a Saturday — or on the second of January — is
	// answered by a range that begins after it.
	//
	// The owner's own journal is the case. Its first operation is dated
	// 2020-10-26 and the Bank of Russia's first published row in that range is
	// the 27th, so one dollar operation had no rate at all — and a total is not
	// published while one of its terms cannot be valued, which took the figure
	// off the whole account over a single day.
	//
	// A month covers a New Year's run, which is the longest stretch the source
	// publishes nothing across, and it costs nothing: the range is one request
	// per currency whatever its length.
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

// wantedCurrencies is the sorted set of currencies rates are needed for:
// everything accounts are denominated in, everything operations are recorded
// in, and every space's base currency — minus quoteCurrency, which rates are
// quoted against rather than fetched for.
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
	// Sorted so a run's requests, and its log, come out in a stable order
	// whichever list a currency first appeared in.
	slices.Sort(out)
	return out, nil
}

// utcDay strips the time of day, so dates coming from the clock compare
// exactly with the midnight-UTC dates coming out of Postgres DATE columns.
func utcDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// daysBetween counts calendar days from -> to. Both are midnight UTC, which
// has no DST jumps, so plain division is exact.
func daysBetween(from, to time.Time) int {
	return int(to.Sub(from).Hours() / 24)
}
