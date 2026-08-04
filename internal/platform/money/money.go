// Package money holds the one step that turns an exact decimal figure into
// the int64 minor units this codebase stores and publishes money in.
//
// It is a package of its own because that step is taken in four others —
// marketdata converts an amount at a rate, portfolio values a holding and sums
// a basis, operation converts a journal entry, account converts a balance —
// and it has to be taken the same way in all of them. Two statements of one
// rule drift; this codebase has been bitten by that before.
//
// Add and Sub carry the same rule to figures that are ALREADY int64 minor
// units. The arithmetic done on Minor's results is not itself safe — Go's + and
// - wrap in silence — and the published figure is often a sum of them.
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
// These are the type's own edges — what a figure must fit in to be published at
// all — and are a different thing entirely from MaxAmountMinor below, which is
// what a WRITE will take. Spelled out as int64's own so no reader has to hold
// the two apart by their names.
var (
	int64MaxMinor = decimal.NewFromInt(math.MaxInt64)
	int64MinMinor = decimal.NewFromInt(math.MinInt64)
)

// MaxAmountMinor caps the magnitude of any single sum of money this program
// ACCEPTS at a write: 10^15 minor units, ten trillion whole roubles or dollars.
// Far above any real holding, and far enough below math.MaxInt64 (≈9.2×10^18)
// that thousands of such figures can be summed without the total wrapping.
//
// ONE NUMBER FOR EVERY WRITE OF MONEY, and it lives here rather than in the
// package that first needed it, because the figures it bounds meet each other:
// an operation's amount_minor and fee_minor (internal/operation), the notional
// its price and quantity multiply to, and the balance a user records for an
// account (internal/account) are all sums of money in an account's currency,
// all summed with one another and multiplied by an fx rate before anything is
// published. A program that took as a balance a figure it refuses as an
// operation — on the very same account, on the very same screen — would have an
// asymmetry nobody could explain later, and two constants spelled out
// separately are two constants that eventually differ. This codebase has been
// bitten by exactly that shape before.
//
// It is NOT the int64 ceiling and must not be mistaken for one. Minor still
// enforces the type's own edges, because what fitted when it was written can
// stop fitting later: an fx rate and a quote price are both unbounded from
// above and both arrive after the fact, a total is a sum of many figures each
// of which passed this cap on its own, and rows written before this cap existed
// are still in the database. A write-time cap only stops one figure from being
// the reason a total cannot be published.
//
// It says nothing about the sign: a debt is a negative balance and an outflow a
// negative amount, so callers compare the MAGNITUDE against it — with explicit
// comparisons at both ends rather than an abs(), since negating math.MinInt64
// is itself an overflow.
const MaxAmountMinor int64 = 1_000_000_000_000_000

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
	if rounded.GreaterThan(int64MaxMinor) || rounded.LessThan(int64MinMinor) {
		return 0, ErrOverflow
	}
	return rounded.IntPart(), nil
}

// Add returns a + b, and Sub returns a - b, as int64 minor units — or
// ErrOverflow if the result does not fit.
//
// They exist because the arithmetic DONE ON figures that already survived Minor
// is not itself safe. Go's int64 + and - wrap silently past the range, in the
// same way and for the same reason decimal.IntPart() does, and a wrapped total
// is a plausible-looking sum of money of the wrong magnitude and often the wrong
// sign. Every term of a total here is by construction an ordinary figure — it
// had to be one to become an int64 at all — and their total need not be:
// THE GUARD BELONGS ON THE PUBLISHED FIGURE, AND THE PUBLISHED FIGURE IS THE
// TOTAL. That is the same argument portfolio.sumInBase and
// marketdata.ConvertMany already stand on; these two carry it to the sums that
// are struck from int64s rather than from decimals (#83).
//
// Both go through Minor rather than through an overflow test of their own, so
// the range is stated once for the whole codebase. The decimals are exact and
// whole, so nothing is rounded on the way. Sub is a real subtraction rather than
// Add with a negated argument, because negating math.MinInt64 is itself an
// overflow.
//
// The error names no figure, as Minor's does not: callers add the context that
// says WHICH total failed. A caller must also never publish the refusal as
// missing data — see ErrOverflow.
func Add(a, b int64) (int64, error) {
	return Minor(decimal.NewFromInt(a).Add(decimal.NewFromInt(b)))
}

// Sub returns a - b in minor units, or ErrOverflow if the difference does not
// fit. See Add.
func Sub(a, b int64) (int64, error) {
	return Minor(decimal.NewFromInt(a).Sub(decimal.NewFromInt(b)))
}
