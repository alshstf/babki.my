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

// Quote is the price of an instrument, in Currency, on date On.
type Quote struct {
	InstrumentID uuid.UUID
	On           time.Time
	Price        decimal.Decimal
	Currency     string
	Source       string
}
