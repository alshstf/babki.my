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
	Ticker string
	// ISIN identifies the SECURITY, where the ticker identifies a listing of
	// one. Two exchanges give unrelated companies the same ticker — and two
	// exchanges in one currency zone give it to them in one currency, so a
	// ticker and a currency together do not settle it either. Empty when the
	// source sends none, and then the ticker is all there is.
	ISIN     string
	Price    decimal.Decimal
	Currency string
	// On is the trading day this price belongs to, as stated by the source —
	// never the day it was fetched on. The two are not the same day: a source
	// asked at any hour of Monday may well answer with Friday's price, and
	// #90 is what storing such a price under Monday cost. A provider that
	// cannot say which day a price belongs to must leave that price out
	// rather than supply a day of its own.
	On time.Time
}

// QuoteProvider fetches recent prices for a set of exchange tickers (e.g.
// MOEX ISS). Declared here alongside FxProvider so both provider kinds share
// one home.
type QuoteProvider interface {
	// QuotesFor fetches whatever price the source currently publishes for
	// each ticker, each one dated by the source itself (see TickerQuote.On).
	// Tickers with no usable price are simply absent from the result — not an
	// error.
	//
	// There is deliberately no date argument. This method used to take the
	// date to stamp on the results, and every caller passed today, which is
	// how the previous session's price came to be stored as today's. A caller
	// cannot know what day a price it has not fetched yet belongs to, so
	// asking it is asking for an invented answer.
	QuotesFor(ctx context.Context, tickers []string) ([]TickerQuote, error)
	// Name identifies the provider; used as Quote.Source.
	Name() string
}
