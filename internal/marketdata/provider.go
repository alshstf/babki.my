package marketdata

import (
	"context"
	"time"

	"github.com/shopspring/decimal"
)

// FxProvider fetches daily FX rates from an external source. It is called from
// background jobs only.
type FxProvider interface {
	// RatesOn fetches the rates published for the date. Nothing to report is an
	// error, not an empty success.
	RatesOn(ctx context.Context, on time.Time) ([]FxRate, error)
	// Name identifies the provider; used as FxRate.Source.
	Name() string
}

// FxHistoryProvider is an FxProvider that can return a date range of one
// currency's rates in one request, for the backfill.
type FxHistoryProvider interface {
	FxProvider
	// CurrencyIDs maps ISO codes to the source's own identifiers, which
	// RatesRange needs. Unquoted currencies are absent.
	CurrencyIDs(ctx context.Context) (map[string]string, error)
	// RatesRange returns the rates published for one currency over [from, to],
	// reported under code (the response names the currency only by currencyID).
	// Unpublished days are simply missing; nothing published is an empty slice.
	RatesRange(ctx context.Context, code, currencyID string, from, to time.Time) ([]FxRate, error)
}

// TickerQuote is a price keyed by exchange ticker; mapping it to a catalog
// instrument is the caller's job.
type TickerQuote struct {
	Ticker string
	// ISIN identifies the security, where a ticker identifies a listing; empty
	// when the source sends none.
	ISIN     string
	Price    decimal.Decimal
	Currency string
	// On is the trading day the price belongs to, as the source states it —
	// never the fetch day (#90). For a carried-forward price it is the day the
	// price was made.
	On time.Time
	// Bond is a bond's face and accrued interest as of the fetch, without its
	// paper, day or source; nil for anything else or when not stated.
	Bond *BondDay
}

// QuoteProvider fetches recent prices for exchange tickers.
type QuoteProvider interface {
	// QuotesFor returns the source's current price for each ticker, dated by the
	// source. Tickers without a usable price are absent. It takes no date: the
	// caller cannot know which day a price belongs to.
	QuotesFor(ctx context.Context, tickers []string) ([]TickerQuote, error)
	// Name identifies the provider; used as Quote.Source.
	Name() string
}
