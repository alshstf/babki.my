// Package money holds the one step that turns an exact decimal figure into
// the int64 minor units this codebase stores and publishes money in.
//
// It is a package of its own because that step is taken in four others —
// marketdata converts an amount at a rate, portfolio values a holding and sums
// a basis, operation converts a journal entry, account converts a balance —
// and it has to be taken the same way in all of them. Two statements of one
// rule drift; this codebase has been bitten by that before.
package money

import (
	"errors"
	"math"

	"github.com/shopspring/decimal"
)

// ErrOverflow reports a figure too large in magnitude to be money here: past
// int64, minor units.
//
// IT IS NOT A MISSING VALUE, and a caller must never render it as one. Every
// screen in this application has a shape for "we could not work this out" — a
// null with a marker saying whether the rate or the date was missing — and
// those markers make a promise: the figure is absent because DATA is absent,
// and will appear when the data does. An overflow is the opposite kind of
// news. The inputs are all present and one of them is broken, nothing will
// arrive to fix it, and the honest treatment is to fail loudly rather than to
// take a seat among the gaps.
var ErrOverflow = errors.New("money: figure does not fit in int64 minor units")

// Bounds of the int64 the whole codebase keeps money in, as decimals, so the
// comparison below is exact at both ends rather than routed through a float.
var (
	maxMinor = decimal.NewFromInt(math.MaxInt64)
	minMinor = decimal.NewFromInt(math.MinInt64)
)

// Minor rounds an exact decimal figure of minor units to a whole minor unit
// and returns it as an int64, or ErrOverflow if it does not fit.
//
// Rounding is half away from zero (decimal.Decimal.Round's own behavior at 0
// decimal places): 150.5 becomes 151 and -150.5 becomes -151, so rounding
// never shrinks the magnitude of a debt. Callers pass the figure UNROUNDED and
// let this round it — one rounding, at the last moment, on the value actually
// published.
//
// The range is checked AFTER rounding, because the rounded figure is the one
// that gets published: a value a fraction above the maximum rounds down onto
// it and is perfectly publishable, and refusing it would withhold a real
// number over a fraction of a kopeck.
//
// Why this exists at all: decimal.IntPart() does not fail past int64, it
// WRAPS. decimal("1e30").IntPart() is 5076944270305263616 — a positive,
// ordinary-looking sum of money that is not the figure and not even the right
// sign in the general case. A price and a quantity that each look reasonable
// on their own can multiply past the edge, so nothing upstream can be relied
// on to have caught it (#27).
//
// The error names no figure. The decimals that reach here can carry an
// arbitrary exponent (a Postgres numeric holds thousands of digits), and
// formatting one of those to say how far past the edge it landed would build a
// string nobody reads out of a number nobody can read. Callers add the context
// that identifies WHICH figure failed, which is the part a person needs.
func Minor(d decimal.Decimal) (int64, error) {
	rounded := d.Round(0)
	if rounded.GreaterThan(maxMinor) || rounded.LessThan(minMinor) {
		return 0, ErrOverflow
	}
	return rounded.IntPart(), nil
}
