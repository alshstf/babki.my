// Package money converts exact decimal figures to the int64 minor units money
// is stored and published in, and does int64 arithmetic on them without
// silent wraparound.
package money

import (
	"errors"
	"math"

	"github.com/shopspring/decimal"
)

// ErrOverflow reports a figure too large to fit in int64 minor units.
//
// It is a broken input, not missing data: callers must fail on it rather than
// show it as an absent figure waiting for a rate or a date.
var ErrOverflow = errors.New("money: figure does not fit in int64 minor units")

var (
	int64MaxMinor = decimal.NewFromInt(math.MaxInt64)
	int64MinMinor = decimal.NewFromInt(math.MinInt64)
)

// MaxAmountMinor caps the magnitude of any amount accepted at a write: 10^15
// minor units, ten trillion roubles or dollars. It is far enough below int64's
// range that thousands of such amounts sum without overflow. Operations and
// account balances share it because they are summed together.
//
// It bounds writes only; Minor still guards the int64 range, since rates,
// prices and sums can push a figure past it later.
const MaxAmountMinor int64 = 1_000_000_000_000_000

// Minor rounds d half away from zero to a whole minor unit and returns it as
// int64, or ErrOverflow if the rounded value does not fit. Pass the figure
// unrounded: it is rounded once, here.
//
// decimal.IntPart wraps past int64 instead of failing, which is why this
// exists.
func Minor(d decimal.Decimal) (int64, error) {
	rounded := d.Round(0)
	if rounded.GreaterThan(int64MaxMinor) || rounded.LessThan(int64MinMinor) {
		return 0, ErrOverflow
	}
	return rounded.IntPart(), nil
}

// Add returns a + b in minor units, or ErrOverflow if the sum does not fit.
// Go's int64 addition wraps silently.
func Add(a, b int64) (int64, error) {
	return Minor(decimal.NewFromInt(a).Add(decimal.NewFromInt(b)))
}

// Sub returns a - b in minor units, or ErrOverflow if the difference does not
// fit.
func Sub(a, b int64) (int64, error) {
	return Minor(decimal.NewFromInt(a).Sub(decimal.NewFromInt(b)))
}
