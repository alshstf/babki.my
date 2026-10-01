package portfolio

import (
	"time"

	"github.com/shopspring/decimal"
)

// DatelessBasis reports whether a piece of basis stops a conversion into
// another currency: it has money in it and nobody recorded when it was bought,
// so there is no day whose rate could value it.
//
// A piece that arrived with no purchase price is not one. It is counted as
// bought for nothing — which is how a broker counts shares handed to it without
// their cost — and nought is nought at any day's rate, so it needs no date.
func DatelessBasis(acquiredOn *time.Time, costMinor int64) bool {
	return acquiredOn == nil && costMinor != 0
}

// UnknownCost reports whether a piece of basis is shares that arrived with no
// purchase price and are counted as bought for nothing: their whole value, and
// the whole of what they sell for, counts as profit.
func UnknownCost(quantity decimal.Decimal, costMinor int64) bool {
	return costMinor == 0 && quantity.IsPositive()
}
