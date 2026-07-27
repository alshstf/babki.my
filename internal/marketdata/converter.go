package marketdata

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

// ErrNoRate is returned by Convert when no path connects the two
// currencies on the given date: no direct rate, no inverse of a reverse
// rate, and no bridge through RUB.
var ErrNoRate = errors.New("marketdata: no fx rate available")

// rubCode is the hub currency for bridging. cbr (the only FxProvider today)
// only ever publishes <currency>/RUB rates, so any two non-RUB currencies
// are connected only via their shared RUB leg — see resolveRate.
const rubCode = "RUB"

// Converter turns minor-unit amounts (kopecks, cents, ...) of one currency
// into another using rates from a Store.
//
// Simplification (MVP; revisited in plan 4b): both currencies are assumed
// to use 2 decimal digits of minor units, the convention amountMinor uses
// everywhere else in this codebase. Multiplying minor units directly by the
// major-unit exchange rate is only correct when both currencies share that
// scale. Currencies with a different number of decimal digits — JPY, KRW,
// and similar zero-decimal currencies — will be converted at the wrong
// scale (off by a factor of 100) until that's addressed.
type Converter struct {
	store *Store
}

// NewConverter returns a Converter backed by store.
func NewConverter(store *Store) *Converter {
	return &Converter{store: store}
}

// Convert converts amountMinor minor units of currency from into currency
// to, using the rate in effect on (or the nearest earlier date, per
// Store.FxRateOn's semantics).
//
// If from == to, amountMinor is returned unchanged — no rate lookup, no
// rounding, so identity conversions can never introduce drift. Otherwise
// the rate is resolved as, in order: a direct from->to rate; the inverse of
// a reverse to->from rate (1/rate); or a bridge through RUB chaining a
// from->RUB leg and a RUB->to leg (each itself resolved direct-or-inverse).
// If none of those exist, Convert returns ErrNoRate.
//
// A bridge multiplies both legs as decimals before amountMinor is ever
// touched, so exactly one rounding happens — at the very end — regardless
// of which path resolved the rate. Rounding is half-away-from-zero
// (decimal.Decimal.Round's native behavior, applied at 0 decimal places):
// 150.5 rounds to 151, and symmetrically -150.5 rounds to -151. That
// symmetry is a deliberate choice for negative amounts (debts): rounding
// never shrinks the magnitude of a debt.
func (c *Converter) Convert(ctx context.Context, amountMinor int64, from, to string, on time.Time) (int64, error) {
	if from == to {
		return amountMinor, nil
	}
	rate, err := c.resolveRate(ctx, from, to, on)
	if err != nil {
		return 0, err
	}
	return decimal.NewFromInt(amountMinor).Mul(rate).Round(0).IntPart(), nil
}

// ConvertMany converts every entry of amounts into currency to (via
// Convert, on date on) and sums the results.
//
// Currencies with no resolvable rate are skipped and returned in missing
// rather than failing the whole call: a portfolio summary should show a
// partial total plus a "N currencies not converted" note, not an error
// page, just because one obscure holding lacks a fresh quote. err is
// reserved for genuine failures — a DB error, a canceled context — that
// make converted untrustworthy; when err is non-nil, converted and missing
// are both zero-valued and must be ignored.
func (c *Converter) ConvertMany(ctx context.Context, amounts map[string]int64, to string, on time.Time) (converted int64, missing []string, err error) {
	for currency, amountMinor := range amounts {
		got, cErr := c.Convert(ctx, amountMinor, currency, to, on)
		if cErr == nil {
			converted += got
			continue
		}
		if errors.Is(cErr, ErrNoRate) {
			missing = append(missing, currency)
			continue
		}
		return 0, nil, cErr
	}
	sort.Strings(missing)
	return converted, missing, nil
}

// resolveRate finds the from->to conversion rate ("to units per 1 from
// unit") on date on. See Convert's doc for the full resolution order and
// rounding contract; resolveRate itself never rounds.
func (c *Converter) resolveRate(ctx context.Context, from, to string, on time.Time) (decimal.Decimal, error) {
	rate, ok, err := c.directOrInverse(ctx, from, to, on)
	if err != nil {
		return decimal.Decimal{}, err
	}
	if ok {
		return rate, nil
	}

	fromToRUB, ok1, err := c.directOrInverse(ctx, from, rubCode, on)
	if err != nil {
		return decimal.Decimal{}, err
	}
	rubToTo, ok2, err := c.directOrInverse(ctx, rubCode, to, on)
	if err != nil {
		return decimal.Decimal{}, err
	}
	if ok1 && ok2 {
		// Both legs are decimals still; multiplying them here (rather than
		// applying each to amountMinor separately) keeps the bridge free of
		// intermediate rounding — only Convert's final .Round(0) rounds.
		return fromToRUB.Mul(rubToTo), nil
	}

	return decimal.Decimal{}, fmt.Errorf("%w: %s -> %s on %s", ErrNoRate, from, to, on.Format("2006-01-02"))
}

// directOrInverse looks up the from->to rate ("to units per 1 from unit")
// as a direct (base=from, quote=to) row or, failing that, as the inverse of
// a (base=to, quote=from) row. ok is false — not an error — when neither
// direction has data on or before on; a genuine Store failure (anything
// other than pgx.ErrNoRows) is returned as err instead.
func (c *Converter) directOrInverse(ctx context.Context, from, to string, on time.Time) (rate decimal.Decimal, ok bool, err error) {
	direct, err := c.store.FxRateOn(ctx, from, to, on)
	if err == nil {
		return direct.Rate, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return decimal.Decimal{}, false, err
	}

	reverse, err := c.store.FxRateOn(ctx, to, from, on)
	if err == nil {
		return decimal.NewFromInt(1).Div(reverse.Rate), true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return decimal.Decimal{}, false, err
	}

	return decimal.Decimal{}, false, nil
}
