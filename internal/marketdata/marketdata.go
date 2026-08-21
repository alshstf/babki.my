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
// GoldCode is the code this program files gold rates under, and it is the
// broker's code rather than ISO 4217's meaning of it.
//
// ISO says XAU is a troy OUNCE. The broker uses it for the exchange's spot gold,
// whose unit is a GRAM, and the owner's journal counts grams because his
// purchases are of that instrument. So the code travels through this program
// meaning what his operations mean by it, and the one source that could
// contradict it publishes no gold rate at all (see moex.GoldRates).
const GoldCode = "XAU"

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
