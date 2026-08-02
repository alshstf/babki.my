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
	rate, rateDate, err := rateVia(ctx, c.rows(), from, to, on)
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

// Rate resolves the from->to conversion rate ("to units per 1 from unit")
// in effect on date on, plus the date of the underlying fx_rates row(s) it
// came from — the same direct/inverse/RUB-bridge resolution Convert uses
// (see its doc for the full order), just without applying it to any
// particular amount.
//
// It exists for callers that need to convert many different amounts that
// share the same currency pair and date — e.g. one space's accounts, several
// of them denominated in the same non-base currency — without paying for
// the underlying rate lookup once per amount. Convert (and ConvertMany)
// still do exactly that internally, at one amount per call; Rate lets a
// caller resolve the rate once, cache it in a local map keyed by currency,
// and apply it to each amount itself via the identical
// decimal.Mul(...).Round(0) step convert uses, so the result matches calling
// Convert per amount bit-for-bit — only the redundant DB round trips are
// removed.
//
// If from == to, rate is 1 and rateDate is the zero time.Time, mirroring
// Convert's identity short-circuit: no lookup happens for an
// already-in-target-currency amount.
func (c *Converter) Rate(ctx context.Context, from, to string, on time.Time) (rate decimal.Decimal, rateDate time.Time, err error) {
	return rateVia(ctx, c.rows(), from, to, on)
}

// rateVia is the single entry point into rate resolution: the identity
// short-circuit plus resolveRate, over whichever source of rows it is handed.
// Rate goes through it a pair at a time against the store; RatesOn goes
// through it twice per query against, in turn, the source that records what
// resolution consults and the source that answers from the prefetch. Nothing
// resolves a rate any other way, so there is exactly one statement of the
// rules to keep right.
//
// from == to is rate 1 on a zero date, resolved without consulting anything:
// an amount already in the target currency needs no rate, and inventing a
// date for one would make callers disclose a staleness that does not exist.
func rateVia(ctx context.Context, rows fxRateRows, from, to string, on time.Time) (rate decimal.Decimal, rateDate time.Time, err error) {
	if from == to {
		return decimal.NewFromInt(1), time.Time{}, nil
	}
	return resolveRate(ctx, rows, from, to, on)
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
func resolveRate(ctx context.Context, rows fxRateRows, from, to string, on time.Time) (rate decimal.Decimal, rateDate time.Time, err error) {
	rate, rateDate, ok, err := directOrInverse(ctx, rows, from, to, on)
	if err != nil {
		return decimal.Decimal{}, time.Time{}, err
	}
	if ok {
		return rate, rateDate, nil
	}

	fromToRUB, date1, ok1, err := directOrInverse(ctx, rows, from, rubCode, on)
	if err != nil {
		return decimal.Decimal{}, time.Time{}, err
	}
	rubToTo, date2, ok2, err := directOrInverse(ctx, rows, rubCode, to, on)
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
// direction has data on or before on; a genuine source failure is returned
// as err instead.
func directOrInverse(ctx context.Context, rows fxRateRows, from, to string, on time.Time) (rate decimal.Decimal, rateDate time.Time, ok bool, err error) {
	direct, ok, err := rows.rateOn(ctx, from, to, on)
	if err != nil {
		return decimal.Decimal{}, time.Time{}, false, err
	}
	if ok {
		return direct.Rate, direct.On, true, nil
	}

	reverse, ok, err := rows.rateOn(ctx, to, from, on)
	if err != nil {
		return decimal.Decimal{}, time.Time{}, false, err
	}
	if ok {
		return decimal.NewFromInt(1).Div(reverse.Rate), reverse.On, true, nil
	}

	return decimal.Decimal{}, time.Time{}, false, nil
}

// fxRateRows is where resolution gets the individual fx_rates rows it
// consults. It exists so that resolving one rate and resolving a page of them
// run the same code: which rows to consult and in what order stays in
// resolveRate alone, and only "where does a row come from" varies — one query
// per row for storeRows, one prefetched map for prefetchedRows. A second copy
// of the direct/inverse/bridge order would be a second answer to a question
// that has one right answer, and the two would drift.
type fxRateRows interface {
	// rateOn answers for (base, quote, on) what Store.FxRateOn answers: the
	// row on that exact date or the nearest earlier one. ok is false — not
	// an error — when the pair has nothing on or before on, because "no
	// rate" is an ordinary outcome this codebase shows to the user; err is
	// for failures that make any answer untrustworthy.
	rateOn(ctx context.Context, base, quote string, on time.Time) (FxRate, bool, error)
}

// rows is the source Convert and Rate resolve through: every row resolution
// asks for costs its own query, up to six of them for a pair that ends up
// bridged. That is the cost RatesOn exists to collapse for callers resolving
// a whole page of pairs at once.
func (c *Converter) rows() fxRateRows { return storeRows{store: c.store} }

// storeRows answers each lookup with a query of its own.
type storeRows struct{ store *Store }

func (s storeRows) rateOn(ctx context.Context, base, quote string, on time.Time) (FxRate, bool, error) {
	r, err := s.store.FxRateOn(ctx, base, quote, on)
	switch {
	case err == nil:
		return r, true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return FxRate{}, false, nil
	default:
		return FxRate{}, false, err
	}
}

// errNotPrefetched reports that resolution asked prefetchedRows for a row the
// prefetch never requested — the two halves of RatesOn disagreeing about what
// resolution consults. It is unexported because no caller can act on it and
// none should ever see it: it means this file is wrong, not that the rate is
// missing.
var errNotPrefetched = errors.New("marketdata: fx rate was not prefetched")

// prefetchedRows answers from a map filled by a single Store.FxRatesOn call.
//
// asked (what the prefetch requested) is kept apart from rows (what came
// back) because they answer different questions. A key that was requested and
// returned nothing means the pair truly has no rate on or before that date —
// an honest answer, shown to the user as a gap. A key nobody requested means
// the enumeration and the resolution disagree, and answering "no rate" there
// would dress a bug as that same honest gap, on a page where nothing looks
// wrong. So it is a loud error instead.
type prefetchedRows struct {
	asked map[FxRateKey]struct{}
	rows  map[FxRateKey]FxRate
}

func (p prefetchedRows) rateOn(_ context.Context, base, quote string, on time.Time) (FxRate, bool, error) {
	key := FxRateKey{Base: base, Quote: quote, On: on}
	if _, requested := p.asked[key]; !requested {
		return FxRate{}, false, fmt.Errorf("%w: %s -> %s on %s", errNotPrefetched, base, quote, on.Format("2006-01-02"))
	}
	r, found := p.rows[key]
	return r, found, nil
}

// recordingRows answers every lookup with "nothing here" and remembers what
// it was asked for. It is how RatesOn learns which rows to prefetch: rather
// than a hand-written second list of the candidates the rules imply — a list
// that would rot silently the day the rules change — the enumeration IS the
// resolution, run against a source that never finds anything so that it walks
// every branch resolveRate can take. A source that does find something only
// ever prunes branches, so what a real resolution consults is always a subset
// of what this pass recorded; anything else is caught by errNotPrefetched.
//
// The recorded key holds the caller's own time.Time value, and resolution
// later looks it up with that same value, so the two cannot miss each other
// over a *time.Location or monotonic-reading difference the way a key rebuilt
// from elsewhere could — see FxRateKey's doc for that hazard at the storage
// boundary.
type recordingRows struct {
	seen map[FxRateKey]struct{}
	keys []FxRateKey // in the order asked, so the prefetch is deterministic
}

func (r *recordingRows) rateOn(_ context.Context, base, quote string, on time.Time) (FxRate, bool, error) {
	key := FxRateKey{Base: base, Quote: quote, On: on}
	if _, dup := r.seen[key]; dup {
		return FxRate{}, false, nil
	}
	if r.seen == nil {
		r.seen = make(map[FxRateKey]struct{})
	}
	r.seen[key] = struct{}{}
	r.keys = append(r.keys, key)
	return FxRate{}, false, nil
}

// RateQuery names one rate resolution for RatesOn: the same pair and date
// Rate takes as arguments.
//
// It is also the key of the map RatesOn returns, so a caller indexes the
// result with the very value it asked with. Build the date the same way for
// every query (as the rest of this codebase does — one value per calendar
// day): two time.Time values that print alike but differ in *time.Location or
// monotonic reading are two different map keys, which costs a caller its
// lookup, not its correctness.
type RateQuery struct {
	From string
	To   string
	On   time.Time
}

// RateResult is what Rate would have returned for one RateQuery: the rate,
// the date of the fx_rates row(s) behind it, and the resolution error.
//
// Err carries ErrNoRate when nothing connects the pair — a per-query outcome
// rather than a failed call, since the rest of the page is still answerable.
// When Err is non-nil, Rate and RateDate are zero-valued and must be ignored.
type RateResult struct {
	Rate     decimal.Decimal
	RateDate time.Time
	Err      error
}

// RatesOn resolves every query in one trip to the store, answering each
// exactly as Rate would answer it on its own — same rate, same date, same
// error — because both go through the same resolution over different sources
// of rows (see fxRateRows). This is a round-trip count change and nothing
// else: no number it produces differs from Rate's.
//
// It exists for the screens that resolve a rate per row — the journal, the
// positions list — where the pair and the date vary from row to row, so
// memoizing Rate by currency still leaves a query per distinct date, and a
// page of thirty rows can spend a hundred of them.
//
// A pair nothing connects fails on its own line, as that query's ErrNoRate:
// one exotic ticker must not blank out a page. err is reserved for failures
// that make the whole map untrustworthy — a DB error, a canceled context —
// and when it is non-nil the map is nil and must be ignored, as ConvertMany's
// total is.
//
// Queries may repeat: duplicates collapse onto one entry, and no row is
// fetched twice however many queries need it. from == to short-circuits as it
// does in Rate — rate 1, zero date, no lookup — so a call holding nothing but
// identity queries never reaches the store at all.
func (c *Converter) RatesOn(ctx context.Context, queries []RateQuery) (map[RateQuery]RateResult, error) {
	// First pass: let the rules say for themselves which rows they will want.
	// Every resolution here ends in ErrNoRate by construction — recordingRows
	// finds nothing, ever — and the errors are discarded because the pass is
	// run for what it records, not for what it returns.
	candidates := &recordingRows{}
	for _, q := range queries {
		_, _, _ = rateVia(ctx, candidates, q.From, q.To, q.On)
	}

	rows, err := c.store.FxRatesOn(ctx, candidates.keys)
	if err != nil {
		return nil, err
	}

	// Second pass: the same rules, now answered from what the first pass
	// asked for.
	return resolveQueries(ctx, prefetchedRows{asked: candidates.seen, rows: rows}, queries)
}

// resolveQueries answers every query from rows, sorting the two kinds of
// failure: ErrNoRate rides along in the query's own result, anything else
// fails the call and voids the map.
//
// Over a prefetched source, "anything else" can only be errNotPrefetched —
// the two passes of RatesOn disagreeing about what resolution consults. That
// is a bug in this file, and the one outcome it must not have is to pass for
// a missing rate on one row of a page that otherwise looks perfectly fine.
func resolveQueries(ctx context.Context, rows fxRateRows, queries []RateQuery) (map[RateQuery]RateResult, error) {
	out := make(map[RateQuery]RateResult, len(queries))
	for _, q := range queries {
		rate, rateDate, err := rateVia(ctx, rows, q.From, q.To, q.On)
		switch {
		case err == nil:
			out[q] = RateResult{Rate: rate, RateDate: rateDate}
		case errors.Is(err, ErrNoRate):
			out[q] = RateResult{Err: err}
		default:
			return nil, err
		}
	}
	return out, nil
}
