package marketdata

import (
	"context"
	"time"

	"github.com/shopspring/decimal"
)

// FxProvider fetches daily currency exchange rates from an external source
// (e.g. the Bank of Russia). Implementations live in subpackages, one per
// source, and are wired into background jobs — never called directly from
// HTTP handlers.
type FxProvider interface {
	// RatesOn fetches the rates published for date on. Implementations
	// return an error rather than an empty, successful result if the
	// source has nothing to report — a silent empty success would look
	// like "no currencies exist today" to callers.
	RatesOn(ctx context.Context, on time.Time) ([]FxRate, error)
	// Name identifies the provider; used as FxRate.Source.
	Name() string
}

// FxHistoryProvider is an FxProvider whose source can also deliver a whole
// date range of one currency's rates in a single request, instead of one
// request per day. Historical backfill depends on this interface; the daily
// refresh keeps using the plain FxProvider above.
//
// It is a separate interface rather than extra methods on FxProvider because
// not every source has a history endpoint, and the daily refresh must stay
// satisfiable by the ones that don't.
type FxHistoryProvider interface {
	FxProvider
	// CurrencyIDs maps ISO 4217 code (e.g. "USD") to the source's own
	// internal currency identifier (e.g. cbr.ru's "R01235"), which
	// RatesRange needs. A currency the source does not quote is absent
	// from the map — not an error.
	CurrencyIDs(ctx context.Context) (map[string]string, error)
	// RatesRange fetches every rate the source published for one currency
	// between from and to, both ends included. code is the ISO 4217 code
	// the rates are reported under (it becomes FxRate.Base) and currencyID
	// is the same currency's internal identifier from CurrencyIDs — the
	// history response identifies the currency only by the latter, so both
	// have to be supplied.
	//
	// Only the days the source actually published on are returned, in the
	// order the source lists them; weekends and holidays are simply
	// missing, and filling those gaps is not this method's job. A currency
	// with nothing published in the range yields an empty slice rather
	// than an error.
	RatesRange(ctx context.Context, code, currencyID string, from, to time.Time) ([]FxRate, error)
}

// TickerQuote is a single instrument's price as reported by a QuoteProvider,
// keyed by exchange ticker rather than InstrumentID — the provider knows
// nothing about our instrument catalog, so mapping ticker to InstrumentID is
// the caller's job.
type TickerQuote struct {
	Ticker   string
	Price    decimal.Decimal
	Currency string
	On       time.Time
}

// QuoteProvider fetches current prices for a set of exchange tickers (e.g.
// MOEX ISS). Declared here alongside FxProvider so both provider kinds share
// one home; the first implementation is added in a later task.
type QuoteProvider interface {
	// QuotesFor fetches prices for tickers as of date on. Tickers with no
	// price available are simply absent from the result — not an error.
	QuotesFor(ctx context.Context, tickers []string, on time.Time) ([]TickerQuote, error)
	// Name identifies the provider; used as Quote.Source.
	Name() string
}
