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

// ErrNotRequested is returned by Rates.For when the triple asked for was not
// among the queries RatesOn was given.
//
// It is the caller-facing twin of errNotPrefetched: both say "nobody ever
// worked this answer out", and both refuse to let that pass for "this pair has
// no rate", which is an entirely different statement and one the user is shown
// as an honest gap. A batch that answered an unasked triple with a zero-valued
// result would put a confident 0,00 on the screen instead — the failure class
// this codebase treats as worse than an error page.
var ErrNotRequested = errors.New("marketdata: rate was not among the resolved queries")

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
// That memoization only helps while the pair and the date repeat. When they
// vary from row to row — a page of the journal, a list of positions — RatesOn
// resolves the whole set in one round trip and gives the same answers.
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
// reaches for a direct row, an inverse or a RUB bridge any other way, so that
// order has exactly one statement to keep right.
//
// The identity rule is the one exception, and a deliberate one: convert
// short-circuits from == to before it ever calls rateVia, so an amount already
// in the target currency is returned untouched rather than multiplied by a
// decimal 1 and rounded (see convert's doc). That is a second statement of the
// same rule, and the two are held together only by pinning both copies —
// TestConvertSameCurrencyIsIdentity for convert's, TestRateIdentityIsOneWithZeroDate
// for this one.
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
// Only the CALENDAR DAY of On matters. Two time.Time values naming the same
// day are the same query however each was built — a different *time.Location,
// a time of day, a monotonic reading picked up from time.Now() — because
// Rates keys by the day itself (see rateLookupKey), and because the day is
// also all the database is asked for: a `date` column carries no clock and no
// zone.
type RateQuery struct {
	From string
	To   string
	On   time.Time
}

// rateLookupKey is what Rates indexes by: the pair, plus the query date
// reduced to its YYYY-MM-DD calendar day instead of kept as a time.Time.
//
// A time.Time makes a treacherous map key. Two values that print alike and
// answer true to Equal are still DIFFERENT keys when they differ in
// *time.Location or in monotonic reading, so a caller that builds a query one
// way and looks it up another — time.Now().UTC() evaluated twice, a date that
// passed through .In() or .Truncate() on only one of the two paths — misses.
// operation.rateKey (internal/operation/http.go) and portfolio.rateKey
// (internal/portfolio/http.go) already hold their dates as YYYY-MM-DD strings
// for exactly this hazard; there a miss costs a redundant query and nothing
// else, here it would decide what number reaches the screen, so the same
// normalization stops being an optimization and becomes the contract.
type rateLookupKey struct {
	from, to string
	on       string // YYYY-MM-DD
}

// lookupKey is the only place a query date becomes a Rates key — not the only
// place a query date becomes a key at all: recordingRows.rateOn and
// prefetchedRows.rateOn each build an FxRateKey holding the raw time.Time
// too. That use is safe for a narrower reason, not the same one: both passes
// of RatesOn key off the identical value (the caller's own time.Time, carried
// unchanged from candidates.keys into the prefetch — see recordingRows' doc),
// so there is no second spelling of the day for it to drift against. The
// day-vs-time.Time hazard this key exists to close is specifically about a
// caller building a query one way and looking it up another, which is a
// question only Rates.For answers.
//
// The value stored on the way in and the value searched for on the way out
// are therefore normalized by one statement and cannot drift apart — the
// whole point of keying by the day rather than by the time.Time.
func (q RateQuery) lookupKey() rateLookupKey {
	return rateLookupKey{from: q.From, to: q.To, on: q.On.Format("2006-01-02")}
}

// Rates holds everything RatesOn resolved, one entry per distinct query, and
// is reached only through For.
//
// It is a value with an accessor rather than the plain map it used to be
// because a map answers a miss with the zero RateResult: rate zero, date zero,
// error nil. A caller writing rates[q].Rate without the comma-ok cannot tell
// that apart from a pair that genuinely resolved to zero, and would render
// every row of a page as 0,00 with no gap marker and no error — a fabricated
// number under a confident caption, which is the worst failure this codebase
// has (four times now, and every time the number was wrong while the caption
// said it was fine). For makes the miss impossible to ignore even for a
// caller that discards its error return: the same error rides along in the
// returned RateResult's Err field, which every RateResult miss already
// requires checking (see RateResult's doc).
//
// The zero Rates is not "empty but usable": it answers every For with
// ErrNotRequested, which is the right treatment for a caller that ignored the
// error RatesOn returned alongside it.
type Rates struct {
	byKey map[rateLookupKey]RateResult
}

// NewRates builds a Rates from caller-supplied results, exactly as RatesOn
// would have keyed them — every entry routed through the same lookupKey()
// normalization RatesOn itself uses, in one statement, so a Rates built here
// cannot key its entries any differently than one RatesOn produced. Two
// results whose RateQuery keys name the same calendar day by different
// time.Time spellings collapse onto one entry here exactly as they do inside
// RatesOn (see rateLookupKey); which of the two survives is whichever the map
// iteration writes last, so callers should not supply two different results
// for what is, to this type, one query.
//
// It exists for code outside this package that needs to hand a Rates to
// something expecting one without going through a real RatesOn call — chiefly
// test fakes standing in for the small structural interface the position and
// journal HTTP handlers hide *Converter behind, so tests can inject a fake
// converter that forces ErrNoRate for one pair, fails one date, or counts
// lookups (see portfolio.converter and its fakes failingConverter,
// historicalFailingConverter, oneDateConverter, and
// operation.countingConverter). Once RatesOn joins that interface, every one
// of those fakes must return a Rates from its own method, and outside this
// package the only otherwise-obtainable Rates is the zero value, which
// answers every For with ErrNotRequested no matter what the fake means to
// simulate — this constructor is what makes a fake able to say anything else.
func NewRates(results map[RateQuery]RateResult) Rates {
	byKey := make(map[rateLookupKey]RateResult, len(results))
	for q, res := range results {
		byKey[q.lookupKey()] = res
	}
	return Rates{byKey: byKey}
}

// For returns what RatesOn resolved for this triple.
//
// The returned error is not the resolution's — it says the triple was never
// among the queries (ErrNotRequested), which is a bug in the calling code. A
// pair that simply HAS no rate comes back as a RateResult carrying ErrNoRate
// in Err with err nil: an ordinary outcome, shown to the user as a gap. That
// is the same line errNotPrefetched draws inside RatesOn, drawn again at the
// boundary the caller stands on.
func (r Rates) For(from, to string, on time.Time) (RateResult, error) {
	q := RateQuery{From: from, To: to, On: on}
	res, ok := r.byKey[q.lookupKey()]
	if !ok {
		// The error is built once and placed in BOTH return slots: a caller
		// that discards the second one (res, _ := rates.For(...)) still finds
		// it in res.Err, the same field every RateResult miss must already be
		// checked through. Returning RateResult{} here would hand that caller
		// rate zero, Err nil — the exact fabricated-zero shape this type
		// exists to make impossible.
		err := fmt.Errorf("%w: %s -> %s on %s", ErrNotRequested, from, to, on.Format("2006-01-02"))
		return RateResult{Err: err}, err
	}
	return res, nil
}

// Len is how many distinct queries were resolved — distinct after the calendar
// day normalization, so an exact duplicate and two spellings of one day each
// count once.
func (r Rates) Len() int { return len(r.byKey) }

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
// that make the whole batch untrustworthy — a DB error, a canceled context —
// and when it is non-nil the returned Rates is the zero value and must be
// ignored, as ConvertMany's total is. (Ignoring it is not silently
// survivable: the zero Rates answers every For with ErrNotRequested.)
//
// Queries may repeat: duplicates collapse onto one entry, however many
// queries name them — an exact repeat and two differently built time.Time
// values naming the same calendar day are duplicates in exactly this sense
// (see rateLookupKey). That collapse is at the ENTRY, not at the fetch. An
// exact repeat costs nothing extra there either, because the first pass
// (recordingRows) records each distinct key once. But two spellings of one
// day are not one key to that first pass — candidates.keys still holds the
// caller's raw time.Time (see recordingRows' doc) — so each spelling earns
// its own prefetch key and its own row, fetched under the identical on_date
// and so answering identically. That is a cost, not a correctness gap: the
// same rows can be fetched twice this way, and the entry both spellings
// collapse onto is right regardless. from == to short-circuits as it does in
// Rate — rate 1, zero date, no lookup — so a call holding nothing but
// identity queries never reaches the store at all.
func (c *Converter) RatesOn(ctx context.Context, queries []RateQuery) (Rates, error) {
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
		return Rates{}, err
	}

	// Second pass: the same rules, now answered from what the first pass
	// asked for.
	return resolveQueries(ctx, prefetchedRows{asked: candidates.seen, rows: rows}, queries)
}

// resolveQueries answers every query from rows, sorting the two kinds of
// failure: ErrNoRate rides along in the query's own result, anything else
// fails the call and voids the whole batch.
//
// Over a prefetched source, "anything else" can only be errNotPrefetched —
// the two passes of RatesOn disagreeing about what resolution consults. That
// is a bug in this file, and the one outcome it must not have is to pass for
// a missing rate on one row of a page that otherwise looks perfectly fine.
func resolveQueries(ctx context.Context, rows fxRateRows, queries []RateQuery) (Rates, error) {
	out := Rates{byKey: make(map[rateLookupKey]RateResult, len(queries))}
	for _, q := range queries {
		rate, rateDate, err := rateVia(ctx, rows, q.From, q.To, q.On)
		switch {
		case err == nil:
			out.byKey[q.lookupKey()] = RateResult{Rate: rate, RateDate: rateDate}
		case errors.Is(err, ErrNoRate):
			out.byKey[q.lookupKey()] = RateResult{Err: err}
		default:
			return Rates{}, err
		}
	}
	return out, nil
}
