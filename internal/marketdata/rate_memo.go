package marketdata

import (
	"context"
	"time"

	"github.com/shopspring/decimal"
)

// RateSource is what a RateMemo resolves through; *Converter is one.
type RateSource interface {
	Rate(ctx context.Context, from, to string, on time.Time) (decimal.Decimal, time.Time, error)
	RatesOn(ctx context.Context, queries []RateQuery) (Rates, error)
}

// RateMemo resolves rates for one request, each pair and day at most once:
// Prefetch fills it in one round trip, and Rate resolves whatever the batch
// missed, so no figure depends on the prefetch. Not safe for concurrent use.
type RateMemo struct {
	source RateSource
	known  map[rateLookupKey]RateResult
}

func NewRateMemo(source RateSource) *RateMemo {
	return &RateMemo{source: source, known: make(map[rateLookupKey]RateResult)}
}

// Prefetch resolves the queries not yet known in one round trip and files the
// answers. Nothing here fails the request: Rate resolves whatever is missing
// and tells a missing rate from an outage. Rates.Answered decides what is
// filed; a failed batch is logged where it dies (#70).
func (m *RateMemo) Prefetch(ctx context.Context, queries []RateQuery) {
	var asked []RateQuery
	seen := make(map[rateLookupKey]bool, len(queries))
	for _, q := range queries {
		k := q.lookupKey()
		if _, known := m.known[k]; known || seen[k] {
			continue
		}
		seen[k] = true
		asked = append(asked, q)
	}
	if len(asked) == 0 {
		return
	}
	resolved, err := m.source.RatesOn(ctx, asked)
	if err != nil {
		return
	}
	for q, res := range resolved.Answered(asked) {
		m.known[q.lookupKey()] = res
	}
}

// Rate is what Converter.Rate answers for the triple, remembered. Err is
// ErrNoRate when nothing connects the pair, which callers publish as a null,
// or a real failure, which fails the request.
func (m *RateMemo) Rate(ctx context.Context, from, to string, on time.Time) RateResult {
	k := RateQuery{From: from, To: to, On: on}.lookupKey()
	res, ok := m.known[k]
	if !ok {
		rate, date, err := m.source.Rate(ctx, from, to, on)
		res = RateResult{Rate: rate, RateDate: date, Err: err}
		m.known[k] = res
	}
	return res
}
