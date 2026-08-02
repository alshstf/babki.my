// Package marketdata owns daily FX rates and instrument quotes. Both are
// append-mostly time series keyed by date: FxRate converts between two
// currency codes, Quote prices an instrument. Lookups by date resolve to the
// exact day or, if missing, the nearest earlier day (weekends/holidays have
// no fresh data, so callers fall back to the last known value).
package marketdata

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// FxRate is the number of Quote units per 1 Base unit on date On.
type FxRate struct {
	Base   string
	Quote  string
	On     time.Time
	Rate   decimal.Decimal
	Source string
}

// FxRateKey names a single rate lookup — the (Base, Quote) pair and the date
// it should be resolved as of, following FxRateOn's semantics (exact date, or
// the nearest earlier one). It is also the map key Store.FxRatesOn returns
// results under, so a caller doing many lookups can index back into the
// result by the same key it asked with. FxRatesOn hands back the caller's
// own key value (via ordinal position in the slice the caller passed in),
// not a key rebuilt from the columns Postgres returns — On does not
// round-trip byte-identical through the database (it goes out as `date`,
// comes back as midnight UTC), so rebuilding it from the wire would only
// match a caller whose On already happened to be exactly midnight in
// time.UTC. Keep it that way: re-reading On off the result row is the
// "simplification" that reintroduces the bug.
type FxRateKey struct {
	Base  string
	Quote string
	On    time.Time
}

// Quote is the price of an instrument, in Currency, on date On.
type Quote struct {
	InstrumentID uuid.UUID
	On           time.Time
	Price        decimal.Decimal
	Currency     string
	Source       string
}
