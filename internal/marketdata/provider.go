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
