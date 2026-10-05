// Package marketdata owns daily FX rates and instrument quotes, both resolved
// by date to the exact day or the nearest earlier one.
package marketdata

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// GoldCode is the code gold rates are filed under. It is the broker's "XAU",
// which means a gram of the exchange's spot gold here, not ISO 4217's troy
// ounce (see moex.GoldRates).
const GoldCode = "XAU"

// FxRate is the number of Quote units per one Base unit on date On.
type FxRate struct {
	Base   string
	Quote  string
	On     time.Time
	Rate   decimal.Decimal
	Source string
}

// FxRateKey names one lookup: the pair and the date to resolve as of.
// Store.FxRatesOn returns results under the caller's own key values, never keys
// rebuilt from the database's dates.
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
