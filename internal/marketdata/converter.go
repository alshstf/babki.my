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

// ErrNoRate means nothing connects the two currencies on the date: no direct
// rate, no inverse, no bridge through RUB.
var ErrNoRate = errors.New("marketdata: no fx rate available")

// ErrNotRequested means Rates.For was asked a triple RatesOn was not given — a
// bug in the caller, never to be shown as a missing rate.
var ErrNotRequested = errors.New("marketdata: rate was not among the resolved queries")

// rubCode is the bridge currency: the CBR publishes only <currency>/RUB rates.
const rubCode = "RUB"

// Converter converts minor-unit amounts between currencies using stored
// rates. It assumes both currencies have two decimal places of minor units;
// zero-decimal currencies such as JPY would be off by a factor of 100.
type Converter struct {
	store *Store
}

func NewConverter(store *Store) *Converter {
	return &Converter{store: store}
}

// Convert converts amountMinor from one currency to another at the rate in
// effect on the date (or the nearest earlier one). The rate is resolved as a
// direct rate, the inverse of the reverse rate, or a bridge through RUB, else
// ErrNoRate. Identity conversions return the amount untouched. The result is
// rounded once, half away from zero, and a product past int64 is
// money.ErrOverflow.
func (c *Converter) Convert(ctx context.Context, amountMinor int64, from, to string, on time.Time) (int64, error) {
	converted, _, err := c.convert(ctx, c.rows(), amountMinor, from, to, on)
	return converted, err
}

// convert is Convert over a given row source, also returning the date of the
// rate used (zero for an identity conversion).
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

// ConvertMany converts every amount into one currency and sums them.
// Currencies with no rate are skipped and listed in missing, so the total can
// be shown with a note; an overflow fails the whole call instead, since a
// total quietly short of one holding would look ordinary. ratesOn is the date
// of the oldest rate used — zero if none was — so callers can say how fresh
// the total is. All rates are prefetched in one round trip (#72).
func (c *Converter) ConvertMany(ctx context.Context, amounts map[string]int64, to string, on time.Time) (converted int64, missing []string, ratesOn time.Time, err error) {
	// Prefetch every rate the loop will need; anything missed is looked up as
	// usual.
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
	// The total is guarded too: terms that each fit can sum past int64. Each term
	// is already whole, so only the range check matters here.
	converted, err = money.Minor(total)
	if err != nil {
		return 0, nil, time.Time{}, fmt.Errorf("%w: %d converted amounts totalling %s %s", err, len(amounts)-len(missing), total, to)
	}
	sort.Strings(missing)
	return converted, missing, ratesOn, nil
}

// Rate resolves the from->to rate ("to units per 1 from unit") and the date of
// the row(s) behind it, as Convert does, for callers converting many amounts
// of one pair. Apply it with money.Minor(decimal.NewFromInt(amount).Mul(rate)),
// not Round(0).IntPart(), which wraps. For pairs and dates that vary by row,
// use RatesOn. from == to is rate 1 with a zero date.
func (c *Converter) Rate(ctx context.Context, from, to string, on time.Time) (rate decimal.Decimal, rateDate time.Time, err error) {
	return rateVia(ctx, c.rows(), from, to, on)
}

// rateVia is the single entry point to resolution: the identity rule, then
// resolveRate, over the given row source. convert applies the identity rule
// itself so an identity amount is never multiplied and rounded.
func rateVia(ctx context.Context, rows fxRateRows, from, to string, on time.Time) (rate decimal.Decimal, rateDate time.Time, err error) {
	if from == to {
		return decimal.NewFromInt(1), time.Time{}, nil
	}
	return resolveRate(ctx, rows, from, to, on)
}

// resolveRate finds the from->to rate and its date without rounding. For a
// bridge the date is the older of the two legs.
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
		// Multiply the legs as decimals so the bridge adds no rounding.
		bridgeDate := date1
		if date2.Before(bridgeDate) {
			bridgeDate = date2
		}
		return fromToRUB.Mul(rubToTo), bridgeDate, nil
	}

	return decimal.Decimal{}, time.Time{}, fmt.Errorf("%w: %s -> %s on %s", ErrNoRate, from, to, on.Format("2006-01-02"))
}

// directOrInverse looks up a direct row or the inverse of the reverse row. ok
// is false, not an error, when neither exists on or before on.
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

// fxRateRows is where resolution reads rows from: the store, a prefetched map,
// or a recorder. The resolution order lives only in resolveRate.
type fxRateRows interface {
	// rateOn answers like Store.FxRateOn: the row on the date or the nearest
	// earlier one. ok is false when there is none; err is a real failure.
	rateOn(ctx context.Context, base, quote string, on time.Time) (FxRate, bool, error)
}

// rows is the per-query source behind Convert and Rate.
func (c *Converter) rows() fxRateRows { return storeRows{store: c.store} }

// fetchRates fetches a batch of rate rows. Callers fall back to per-row
// lookups when it fails, so the failure is invisible except for this log line:
// WARN, since nothing the user asked for failed (#70). A canceled request is
// not a degradation and logs at DEBUG (#98); an expired deadline still warns.
// It logs through slog.Default, which cmd/babki configures.
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

// canceled reports whether a lookup failed because the caller went away. Both
// the error and the context are checked, since a driver may report a torn-down
// connection in its own words.
func canceled(ctx context.Context, err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled)
}

// prewarm prefetches, in one round trip, every row converting amounts may
// consult, and returns a source that answers from them and falls back to the
// store. It is a cache: no figure depends on it, and its failure is not an
// error. The rows are enumerated by running the resolution against
// recordingRows, so there is no second list of the rules.
func (c *Converter) prewarm(ctx context.Context, amounts map[string]int64, to string, on time.Time) fxRateRows {
	store := c.rows()
	candidates := &recordingRows{}
	for currency := range amounts {
		// Run for what it records; over recordingRows every resolution fails.
		_, _, _ = rateVia(ctx, candidates, currency, to, on)
	}
	if len(candidates.keys) == 0 {
		// Nothing to look up: an empty call or only identity conversions.
		return store
	}
	rows, err := c.fetchRates(ctx, candidates.keys)
	if err != nil {
		// Logged by fetchRates; the loop falls back to the store.
		return store
	}
	return warmRows{asked: candidates.seen, rows: rows, fallback: store}
}

// warmRows answers from a ConvertMany prefetch and asks the store about keys
// the prefetch did not request. A requested key with no row is an honest "no
// rate" and is not retried.
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

// errNotPrefetched means resolution asked prefetchedRows for a row the
// prefetch never requested: a bug in this file, not a missing rate.
var errNotPrefetched = errors.New("marketdata: fx rate was not prefetched")

// prefetchedRows answers from one Store.FxRatesOn batch. A requested key with
// no row is an honest "no rate"; an unrequested key is errNotPrefetched.
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

// recordingRows finds nothing and records every lookup, so running the
// resolution over it enumerates every row a real resolution could consult.
// Keys hold the caller's own time.Time, so the later lookup cannot miss.
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

// RateQuery names one resolution for RatesOn. Only the calendar day of On
// matters.
type RateQuery struct {
	From string
	To   string
	On   time.Time
}

// rateLookupKey keys Rates by pair and YYYY-MM-DD day: time.Time values that
// are Equal can still differ as map keys by location or monotonic reading.
type rateLookupKey struct {
	from, to string
	on       string // YYYY-MM-DD
}

// lookupKey is the only place a query becomes a Rates key.
func (q RateQuery) lookupKey() rateLookupKey {
	return rateLookupKey{from: q.From, to: q.To, on: q.On.Format("2006-01-02")}
}

// Rates holds what RatesOn resolved and is read through For, so a miss cannot
// pass for a zero rate. The zero Rates answers ErrNotRequested to everything.
type Rates struct {
	byKey map[rateLookupKey]RateResult
}

// NewRates builds a Rates keyed as RatesOn would key it, for fakes outside
// this package. Queries naming the same day collapse onto one entry.
func NewRates(results map[RateQuery]RateResult) Rates {
	byKey := make(map[rateLookupKey]RateResult, len(results))
	for q, res := range results {
		byKey[q.lookupKey()] = res
	}
	return Rates{byKey: byKey}
}

// For returns what RatesOn resolved for the triple. A pair with no rate comes
// back with ErrNoRate in the result's Err; the returned error is
// ErrNotRequested, a bug in the caller.
func (r Rates) For(from, to string, on time.Time) (RateResult, error) {
	q := RateQuery{From: from, To: to, On: on}
	res, ok := r.byKey[q.lookupKey()]
	if !ok {
		// Both slots carry the error, so a caller that drops one still sees it.
		err := fmt.Errorf("%w: %s -> %s on %s", ErrNotRequested, from, to, on.Format("2006-01-02"))
		return RateResult{Err: err}, err
	}
	return res, nil
}

// Len is the number of distinct queries, after day normalization.
func (r Rates) Len() int { return len(r.byKey) }

// Answered yields, in order, the queries this batch resolved, for callers
// warming a memo from RatesOn. Unresolved queries are skipped, so a bug costs a
// round trip rather than a number; a resolved "no rate" is yielded, as it is a
// real answer.
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

// RateResult is what Rate would return for one query. Err is ErrNoRate when
// nothing connects the pair; then Rate and RateDate are zero.
type RateResult struct {
	Rate     decimal.Decimal
	RateDate time.Time
	Err      error
}

// RatesOn resolves every query in one round trip, answering each exactly as
// Rate would. A pair with no rate fails only its own query; err is for
// failures of the whole batch, and then Rates is zero. Duplicate queries,
// including different spellings of one day, collapse onto one entry.
func (c *Converter) RatesOn(ctx context.Context, queries []RateQuery) (Rates, error) {
	// First pass: record which rows the rules will consult.
	candidates := &recordingRows{}
	for _, q := range queries {
		_, _, _ = rateVia(ctx, candidates, q.From, q.To, q.On)
	}

	rows, err := c.fetchRates(ctx, candidates.keys)
	if err != nil {
		return Rates{}, err
	}

	// Second pass: answer from what the first pass fetched.
	return resolveQueries(ctx, prefetchedRows{asked: candidates.seen, rows: rows}, queries)
}

// resolveQueries answers every query from rows. ErrNoRate stays with its
// query; any other error can only be errNotPrefetched, a bug in this file, so
// it voids the batch and is logged at ERROR (#97): callers ignore RatesOn's
// error by design and the page would silently fall back.
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
			// Both the requested pair and the query, since a bridge may differ.
			slog.Default().Error("fx rate prefetch did not cover the resolution",
				"query", fmt.Sprintf("%s -> %s on %s", q.From, q.To, q.On.Format("2006-01-02")),
				"err", err.Error())
			return Rates{}, err
		}
	}
	return out, nil
}
