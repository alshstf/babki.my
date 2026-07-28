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
	converted, _, err := c.convert(ctx, amountMinor, from, to, on)
	return converted, err
}

// convert is Convert's implementation, additionally returning the date of
// the fx rate actually used to produce converted — the zero time.Time for
// an identity conversion (from == to), which resolves no rate at all. It
// backs both Convert (which discards the date) and ConvertMany (which
// tracks the oldest date across every entry it converts).
func (c *Converter) convert(ctx context.Context, amountMinor int64, from, to string, on time.Time) (converted int64, rateDate time.Time, err error) {
	if from == to {
		return amountMinor, time.Time{}, nil
	}
	rate, rateDate, err := c.resolveRate(ctx, from, to, on)
	if err != nil {
		return 0, time.Time{}, err
	}
	return decimal.NewFromInt(amountMinor).Mul(rate).Round(0).IntPart(), rateDate, nil
}

// ConvertMany converts every entry of amounts into currency to (via
// Convert, on date on) and sums the results.
//
// Currencies with no resolvable rate are skipped and returned in missing
// rather than failing the whole call: a portfolio summary should show a
// partial total plus a "N currencies not converted" note, not an error
// page, just because one obscure holding lacks a fresh quote. err is
// reserved for genuine failures — a DB error, a canceled context — that
// make converted untrustworthy; when err is non-nil, converted, missing and
// ratesOn are all zero-valued and must be ignored.
//
// ratesOn is the date of the OLDEST fx rate actually used across every
// converted entry — never today's date, since FxRateOn resolves to the
// nearest date on or before on, which for a stale rate table can be
// arbitrarily far in the past. It lets a caller disclose "how fresh is this
// total" rather than silently implying "today's rate" for a courtesy that
// may be weeks stale. Identity entries (currency == to) resolve no rate at
// all and so never move ratesOn; if every entry was identity, or amounts is
// empty, or every entry ended up in missing, ratesOn is the zero time.Time
// — the caller must render that as "null", not as a date.
func (c *Converter) ConvertMany(ctx context.Context, amounts map[string]int64, to string, on time.Time) (converted int64, missing []string, ratesOn time.Time, err error) {
	for currency, amountMinor := range amounts {
		got, rateDate, cErr := c.convert(ctx, amountMinor, currency, to, on)
		if cErr == nil {
			converted += got
			if !rateDate.IsZero() && (ratesOn.IsZero() || rateDate.Before(ratesOn)) {
				ratesOn = rateDate
			}
			continue
		}
		if errors.Is(cErr, ErrNoRate) {
			missing = append(missing, currency)
			continue
		}
		return 0, nil, time.Time{}, cErr
	}
	sort.Strings(missing)
	return converted, missing, ratesOn, nil
}

// resolveRate finds the from->to conversion rate ("to units per 1 from
// unit") on date on, plus the date of the underlying fx_rates row(s) it
// came from. See Convert's doc for the full resolution order and rounding
// contract; resolveRate itself never rounds.
//
// For a direct or inverse hit, rateDate is that row's own date. For a
// bridge through RUB, two independent rows back the two legs and may carry
// different dates; rateDate is the OLDER of the two, since the composite
// rate is only as fresh as its stalest leg.
func (c *Converter) resolveRate(ctx context.Context, from, to string, on time.Time) (rate decimal.Decimal, rateDate time.Time, err error) {
	rate, rateDate, ok, err := c.directOrInverse(ctx, from, to, on)
	if err != nil {
		return decimal.Decimal{}, time.Time{}, err
	}
	if ok {
		return rate, rateDate, nil
	}

	fromToRUB, date1, ok1, err := c.directOrInverse(ctx, from, rubCode, on)
	if err != nil {
		return decimal.Decimal{}, time.Time{}, err
	}
	rubToTo, date2, ok2, err := c.directOrInverse(ctx, rubCode, to, on)
	if err != nil {
		return decimal.Decimal{}, time.Time{}, err
	}
	if ok1 && ok2 {
		// Both legs are decimals still; multiplying them here (rather than
		// applying each to amountMinor separately) keeps the bridge free of
		// intermediate rounding — only Convert's final .Round(0) rounds.
		bridgeDate := date1
		if date2.Before(bridgeDate) {
			bridgeDate = date2
		}
		return fromToRUB.Mul(rubToTo), bridgeDate, nil
	}

	return decimal.Decimal{}, time.Time{}, fmt.Errorf("%w: %s -> %s on %s", ErrNoRate, from, to, on.Format("2006-01-02"))
}

// directOrInverse looks up the from->to rate ("to units per 1 from unit")
// as a direct (base=from, quote=to) row or, failing that, as the inverse of
// a (base=to, quote=from) row. rateDate is the matched row's own on_date
// (which, per FxRateOn's nearest-earlier-date semantics, may be earlier
// than the queried on). ok is false — not an error — when neither
// direction has data on or before on; a genuine Store failure (anything
// other than pgx.ErrNoRows) is returned as err instead.
func (c *Converter) directOrInverse(ctx context.Context, from, to string, on time.Time) (rate decimal.Decimal, rateDate time.Time, ok bool, err error) {
	direct, err := c.store.FxRateOn(ctx, from, to, on)
	if err == nil {
		return direct.Rate, direct.On, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return decimal.Decimal{}, time.Time{}, false, err
	}

	reverse, err := c.store.FxRateOn(ctx, to, from, on)
	if err == nil {
		return decimal.NewFromInt(1).Div(reverse.Rate), reverse.On, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return decimal.Decimal{}, time.Time{}, false, err
	}

	return decimal.Decimal{}, time.Time{}, false, nil
}
