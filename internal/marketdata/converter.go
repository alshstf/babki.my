package marketdata

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/platform/money"
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
//
// A product too large for an int64 is refused (money.ErrOverflow) rather than
// wrapped. Neither input has to look unusual for that to happen — an amount
// near the top of its column at a rate of 2 is enough — and the wrapped
// answer is a small, plausible figure of the wrong sign (see money.Minor).
func (c *Converter) Convert(ctx context.Context, amountMinor int64, from, to string, on time.Time) (int64, error) {
	converted, _, err := c.convert(ctx, c.rows(), amountMinor, from, to, on)
	return converted, err
}

// convert is Convert's implementation, additionally returning the date of
// the fx rate actually used to produce converted — the zero time.Time for
// an identity conversion (from == to), which resolves no rate at all. It
// backs both Convert (which discards the date) and ConvertMany (which
// tracks the oldest date across every entry it converts).
//
// It takes the source of rows to resolve through rather than reaching for
// c.rows() itself, which is the whole of the difference between the two: a
// lone Convert queries the store as it goes, while ConvertMany hands every
// entry the same prewarmed source (see prewarm). The arithmetic below — one
// multiplication, one rounding, one overflow refusal — is the same statement
// either way, so no number depends on which source resolved the rate.
func (c *Converter) convert(ctx context.Context, rows fxRateRows, amountMinor int64, from, to string, on time.Time) (converted int64, rateDate time.Time, err error) {
	if from == to {
		return amountMinor, time.Time{}, nil
	}
	rate, rateDate, err := rateVia(ctx, rows, from, to, on)
	if err != nil {
		return 0, time.Time{}, err
	}
	converted, err = money.Minor(decimal.NewFromInt(amountMinor).Mul(rate))
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("%w: %d %s at the %s rate of %s", err, amountMinor, from, to, on.Format("2006-01-02"))
	}
	return converted, rateDate, nil
}

// ConvertMany converts every entry of amounts into currency to (via
// Convert, on date on) and sums the results.
//
// Currencies with no resolvable rate are skipped and returned in missing
// rather than failing the whole call: a portfolio summary should show a
// partial total plus a "N currencies not converted" note, not an error
// page, just because one obscure holding lacks a fresh quote. err is
// reserved for genuine failures — a DB error, a canceled context, an amount
// whose converted value does not fit in an int64, or a TOTAL that does not
// (money.ErrOverflow either way) — that make converted untrustworthy; when err
// is non-nil, converted, missing and ratesOn are all zero-valued and must be
// ignored.
//
// An overflow deliberately fails the whole call rather than joining missing:
// missing is published beside the total as "these currencies were left out",
// a note a reader takes as complete for everything else, and a total quietly
// short of the one holding too big to convert would be exactly the kind of
// smaller-than-truth figure that looks perfectly ordinary on screen.
//
// Every rate the whole call needs is resolved in ONE round trip before the
// loop starts (see prewarm), so what a caller pays does not grow with the
// number of currencies — the axis that matters for GET /summary, where the
// data is one row per currency and the currencies are the whole of it (#72).
// That is a cost change and nothing else: the loop resolves and rounds exactly
// as it always did, over the same rules, and a currency the prewarm did not
// cover is simply looked up as it was before.
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
	// One round trip for every currency in amounts, after which the loop below
	// is unchanged — same resolution, same arithmetic, same numbers (#72). The
	// prewarm is a cache, not a contract: whatever it does not hold, the loop
	// asks the store for, exactly as it did before this line existed.
	rows := c.prewarm(ctx, amounts, to, on)
	total := decimal.Zero
	for currency, amountMinor := range amounts {
		got, rateDate, cErr := c.convert(ctx, rows, amountMinor, currency, to, on)
		if cErr == nil {
			total = total.Add(decimal.NewFromInt(got))
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
	// The guard sits on the TOTAL as well as on each term, because the total is
	// what gets published: two balances that each convert to a perfectly
	// ordinary int64 can still sum past the edge, and `converted` is
	// total_in_base_minor on GET /summary (see account.Handler.handleSummary) —
	// a plain += would put a small negative number on the one screen whose whole
	// job is to say how much there is.
	//
	// Every term added above is already a whole number of minor units, so the
	// rounding money.Minor does changes nothing here and the range check is its
	// only effect. That is the difference from sumInBase in the positions
	// handler, which accumulates UNROUNDED products and therefore rounds once
	// for the whole sum; here each amount was rounded as it was converted, which
	// is what this function has always published.
	converted, err = money.Minor(total)
	if err != nil {
		return 0, nil, time.Time{}, fmt.Errorf("%w: %d converted amounts totalling %s %s", err, len(amounts)-len(missing), total, to)
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
// the underlying rate lookup once per amount. Convert still does exactly that
// internally, at one amount per call — ConvertMany no longer does, having
// prewarmed its whole set first, but it only ever converts one amount per
// currency anyway; Rate lets a caller resolve the rate once, cache it in a
// local map keyed by currency, and apply it to each amount itself — only the
// redundant DB round trips are removed.
//
// Applying it means decimal.NewFromInt(amountMinor).Mul(rate) handed to
// money.Minor, which is the whole of what convert does with a rate once it has
// one. money.Minor and not .Round(0).IntPart(): it rounds identically, but it
// also REFUSES a product too large for an int64 instead of wrapping it to a
// small figure of arbitrary sign (money.ErrOverflow, #27). A caller that
// rounds by hand therefore does not match Convert — it publishes a wrapped
// number exactly where Convert would have failed, which is the whole reason
// package money exists.
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

// rows is the source Convert and Rate resolve through, and the one ConvertMany
// falls back to: every row resolution asks for costs its own query, up to six
// of them for a pair that ends up bridged. That is the cost RatesOn exists to
// collapse for callers resolving a whole page of pairs at once, and prewarm for
// ConvertMany's own set.
func (c *Converter) rows() fxRateRows { return storeRows{store: c.store} }

// fetchRates is the one place a batch of rate rows is fetched, and therefore the
// one place a failure of the batch STATEMENT — a timeout on the large query, a
// problem encoding its array arguments — can be reported (#70).
//
// IT IS REPORTED BECAUSE NOBODY ELSE WILL. Both callers survive this failure by
// design, and so does everyone above them: prewarm below returns the
// un-prefetched row source and ConvertMany converts through per-pair lookups
// exactly as it did before batching existed, while RatesOn returns the error to
// three HTTP handlers that each drop it on the floor on purpose (an error page
// would be a worse outcome than a slow correct one — see their prewarmRates).
// The result is the failure this line exists for: the screen is right, the
// screen is slow, and every trace of the optimization having died is gone. The
// per-pair path is between one and six statements per pair, so what is lost is
// not a rounding error in the request's cost — it is the whole of what batching
// bought.
//
// WARN, deliberately, and not either neighbour. It is not an ERROR: nothing the
// user asked for failed, and ERROR is what family.WriteError writes when a
// request really did fail — spending it here would blunt it there. It is not
// INFO or DEBUG either: this application runs at INFO by default (see
// config.Config.LogLevel), so DEBUG would not be printed at all, which is
// indistinguishable from the silence being fixed, and INFO would put a dead
// optimization in the same stream as every ordinary request. The level is
// pinned, structurally, by TestRatesOnLogsADeadBatchAsAWarning and its
// ConvertMany twin.
//
// A CANCELED REQUEST IS NOT THIS NEWS AND DOES NOT GET THIS LINE (#98). The
// reader navigated away, the request context was canceled, and the batch
// statement died with it — the same branch, and a completely different event.
// Nothing degraded, because there was no longer a request to be slow: the whole
// harm this warning reports is "the screen came out right and dear", and there
// is no screen. Writing it up as a breakage would put an alarm nobody can act on
// beside the one that matters, and a stream of false alarms is how a line added
// to be noticed stops being read. So a cancellation goes to DEBUG under its own
// message: still there for whoever turns the level up while chasing an abandoned
// request, absent from the stream the warning lives in.
//
// Only context.Canceled, and NOT context.DeadlineExceeded. A deadline that
// expired is the program's own limit running out — precisely the slowness this
// warning exists to reveal, and the batch failure most worth hearing about.
// Pinned by TestRatesOnStillWarnsWhenADeadlineExpires.
//
// THAT RULE IS NOT MADE GENERAL, and the omission is deliberate: there is no
// cancellation convention in this codebase, and family.WriteError logs ERROR for
// a canceled request too. The two lines answer different questions, so one rule
// would not fit both. This one asks "did the optimization die?", to which a
// cancellation is not a lesser yes but a no. WriteError's asks "did a request
// this server accepted fail to be answered?", to which a cancellation is a
// truthful yes; whether that deserves ERROR is a judgement about that handler's
// own taxonomy, made in the same switch that picks the 500 a departed client
// will never read, and changing it is changing a status-code decision from
// inside a marketdata fix. Left as it is, on purpose, rather than spread from
// here.
//
// slog.Default rather than a logger of its own: a Converter is built once in
// production (cmd/babki, mountModules) and threaded through three handlers, so
// the obstacle is not the wiring — it is that a logger parameter on
// NewConverter would have to be supplied by every test that builds one, and
// there are about twenty. cmd/babki installs the configured logger as the
// default precisely so code that holds none can still be heard (see
// cmd/babki/runtime.go and family.WriteError, the other user of that
// arrangement).
func (c *Converter) fetchRates(ctx context.Context, keys []FxRateKey) (map[FxRateKey]FxRate, error) {
	rows, err := c.store.FxRatesOn(ctx, keys)
	if err != nil {
		if canceled(ctx, err) {
			slog.Default().Debug("batched fx rate lookup canceled", "keys", len(keys), "err", err.Error())
		} else {
			slog.Default().Warn("batched fx rate lookup failed", "keys", len(keys), "err", err.Error())
		}
		return nil, err
	}
	return rows, nil
}

// canceled reports whether a lookup failed because the caller had already gone
// away, rather than because anything is wrong.
//
// Both the error and the context are consulted, and they are asked different
// questions. The error is the ordinary route — pgx reports a statement killed by
// its context with the context's own error, so errors.Is finds it. The context
// is asked as well because a driver is free to report the same event in words of
// its own ("conn closed") once the connection is torn down underneath it, and
// then the error alone would not say. Asking both errs toward silence, which is
// the right direction here: a warning missed while nobody was waiting costs
// nothing, and a false one costs the credibility of every true one.
func canceled(ctx context.Context, err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled)
}

// prewarm resolves, in one round trip, the rows converting every currency in
// amounts is about to consult, and returns a source that answers from them.
//
// IT IS A CACHE, NOT A CONTRACT. Anything the batch does not hold — a row the
// enumeration did not name, a batch that failed outright, a call with nothing
// to look up — is answered by the store exactly as it was before this existed
// (see warmRows and the two early returns below). So no figure ConvertMany
// publishes depends on the prewarm being complete or even on its having
// succeeded; only the number of round trips does. That is also why a failed
// FxRatesOn is not returned as an error here: a genuine outage is met again by
// the very next lookup and reported from convert, which knows which amount it
// was converting, while a batch-specific failure the per-pair path can survive
// must not become an error page.
//
// The enumeration is the resolution itself, run against recordingRows — the
// same trick RatesOn uses, and for the same reason: a hand-written list of the
// rows the rules imply would be a second statement of those rules, and it would
// rot the day they changed. recordingRows never finds anything, so the pass
// walks every branch resolveRate can take; a real resolution only ever prunes
// branches, so what the loop consults is always a subset of what was asked for.
func (c *Converter) prewarm(ctx context.Context, amounts map[string]int64, to string, on time.Time) fxRateRows {
	store := c.rows()
	candidates := &recordingRows{}
	for currency := range amounts {
		// The errors are discarded because the pass is run for what it records,
		// not for what it returns: over recordingRows every resolution ends in
		// ErrNoRate by construction.
		_, _, _ = rateVia(ctx, candidates, currency, to, on)
	}
	if len(candidates.keys) == 0 {
		// Nothing to look up at all — an empty call, or one holding nothing but
		// identity conversions, which rateVia short-circuits. Asking the store
		// for no rows would be a round trip bought for nothing.
		return store
	}
	rows, err := c.fetchRates(ctx, candidates.keys)
	if err != nil {
		// Survived here and reported by fetchRates, which is the only place that
		// can: from this line on the failure is gone for good, since the loop
		// that follows resolves every rate itself and returns no trace of the
		// batch having been tried at all.
		return store
	}
	return warmRows{asked: candidates.seen, rows: rows, fallback: store}
}

// warmRows answers from a batch prefetched for one ConvertMany call, and asks
// fallback for anything that batch was not asked for.
//
// That fallback is the one thing it does differently from prefetchedRows, which
// treats the same case as a loud bug. The difference is not a relaxed standard,
// it is a different question: prefetchedRows has no way to ANSWER an unasked
// key, so its only choices are an error and a fabricated "this pair has no
// rate" — and dressing a bug as an honest gap on a page where nothing looks
// wrong is the failure this codebase guards hardest against. warmRows can
// simply ask, and the answer it gets back is the right one. A prefetch that
// falls behind the rules therefore costs round trips here and cannot cost
// correctness.
//
// The key is asked, not rows, for the same reason prefetchedRows keeps the two
// apart: a key that WAS requested and came back with nothing means the pair
// truly has no row on or before that date — an honest answer that must be
// passed on as one, not retried against the store as though it had been missed.
type warmRows struct {
	asked    map[FxRateKey]struct{}
	rows     map[FxRateKey]FxRate
	fallback fxRateRows
}

func (w warmRows) rateOn(ctx context.Context, base, quote string, on time.Time) (FxRate, bool, error) {
	key := FxRateKey{Base: base, Quote: quote, On: on}
	if _, requested := w.asked[key]; !requested {
		return w.fallback.rateOn(ctx, base, quote, on)
	}
	r, found := w.rows[key]
	return r, found, nil
}

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

// Answered walks queries in the order given and hands back only the ones this
// batch actually resolved. It is the one statement of how a prefetch is
// consumed, shared by every caller that warms a memo from RatesOn — the
// accounts screen, the journal page and the positions screen (see their
// prewarmRates). Their memo keys are three different unexported types, so what
// they can share is the WALK and not the filing: each still writes its own entry
// under its own key, from the pair this yields.
//
// A query the batch never answered is SKIPPED, not handed back. For reports that
// as ErrNotRequested, which says the enumeration that built the queries and the
// batch that resolved them disagree — a bug in the calling code, never a gap in
// the data. Handing it to a caller that files whatever it is given would put
// that bug in the memo, where it reads as an answer. Skipping it leaves the memo
// cold, and the caller's own per-pair fallback resolves the very same figure one
// round trip dearer. That is the whole arrangement a prefetch here rests on: it
// can cost round trips and it cannot cost a number.
//
// A query that resolved to no rate IS handed back, carrying ErrNoRate in
// RateResult.Err. That is an honest domain answer — nothing connects this pair
// on this date — and filing it is what makes the figure it belongs to come out
// as the gap it should be, without a second lookup that could only reach the
// same conclusion. Skipping it would change no number and would quietly cost a
// round trip per gap, which is why the distinction is pinned by a test of its
// own (TestAnsweredHandsBackAGenuineGapAsAnAnswer) rather than left to the
// handlers' figures to reveal.
//
// A batch that failed outright never reaches here as anything but the zero
// Rates, which answers every For with ErrNotRequested — so the walk yields
// nothing, no entry is filed, and every figure falls back. Callers still check
// RatesOn's error in their own right; this makes the one that forgets cost round
// trips rather than numbers.
func (r Rates) Answered(queries []RateQuery) iter.Seq2[RateQuery, RateResult] {
	return func(yield func(RateQuery, RateResult) bool) {
		for _, q := range queries {
			res, err := r.For(q.From, q.To, q.On)
			if err != nil {
				continue
			}
			if !yield(q, res) {
				return
			}
		}
	}
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

	rows, err := c.fetchRates(ctx, candidates.keys)
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
// "Anything else" can only be errNotPrefetched — the two passes of RatesOn
// disagreeing about what resolution consults. That is a bug in this file, and
// the one outcome it must not have is to pass for a missing rate on one row of
// a page that otherwise looks perfectly fine.
//
// The parameter is prefetchedRows and not the fxRateRows interface for exactly
// that sentence's sake (#97). It is what makes the sentence TRUE rather than
// merely intended: a source that queries a database could fail for a dozen
// ordinary reasons, and the error line below — which says a bug happened — would
// then be saying it about a database having a bad day. The type is the argument.
//
// AND IT SAYS SO OUT LOUD, because nobody above will. RatesOn hands this error
// back beside the zero Rates, and the three HTTP handlers that call it drop it
// on the floor by design (an error page is a worse outcome than a slow correct
// one — see their prewarmRates). The memo then stays cold and every figure on
// the page falls back to a lookup of its own: symptom for symptom the dead batch
// #70 closed, and before this line just as invisible.
//
// ERROR, and here the neighbour that is wrong is WARN. The dead batch is warned
// about because the database may be perfectly well tomorrow; this cannot come
// right on its own — the code is wrong, the next request degrades exactly as
// this one did, and it will go on doing so until somebody reads this line. That
// is the same distinction money.ErrOverflow draws against a missing rate: data
// that is absent will come back, code that is wrong will not. The level is
// pinned by TestResolveQueriesLogsAnUncoveredQueryAsAnError, and its silence on
// a healthy batch by the sibling test beside it.
//
// A canceled request cannot reach this line: prefetchedRows answers from a map
// and never touches ctx, so the cancellation rule fetchRates states has nothing
// to do here.
func resolveQueries(ctx context.Context, rows prefetchedRows, queries []RateQuery) (Rates, error) {
	out := Rates{byKey: make(map[rateLookupKey]RateResult, len(queries))}
	for _, q := range queries {
		rate, rateDate, err := rateVia(ctx, rows, q.From, q.To, q.On)
		switch {
		case err == nil:
			out.byKey[q.lookupKey()] = RateResult{Rate: rate, RateDate: rateDate}
		case errors.Is(err, ErrNoRate):
			out.byKey[q.lookupKey()] = RateResult{Err: err}
		default:
			// The error already names the pair and the date it was asked for
			// (see prefetchedRows.rateOn); the query names what the caller
			// wanted, which is not always the same triple once a bridge is
			// involved. Both, because chasing this bug means knowing which
			// branch of the resolution reached past the enumeration.
			slog.Default().Error("fx rate prefetch did not cover the resolution",
				"query", fmt.Sprintf("%s -> %s on %s", q.From, q.To, q.On.Format("2006-01-02")),
				"err", err.Error())
			return Rates{}, err
		}
	}
	return out, nil
}
