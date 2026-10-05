// Package ratetest holds the converter doubles the handlers' tests share.
package ratetest

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
)

// Counting wraps a real converter and counts what a request asks of the fx
// layer. Keep filters the batch (a hole in the enumeration); BatchErr fails
// the batch alone (#70). The counts are atomic: a handler runs on the
// server's goroutine.
type Counting struct {
	Inner    *marketdata.Converter
	Keep     func(marketdata.RateQuery) bool
	BatchErr error
	// Singles counts one-pair lookups, Batches batch calls, and Queries the
	// rates asked for in batches before Keep drops any.
	Singles, Batches, Queries atomic.Int64
}

// Dropping and FailingBatch tune a Counting: a hole in the enumeration, or a
// batch that dies alone.
func Dropping(keep func(marketdata.RateQuery) bool) func(*Counting) {
	return func(c *Counting) { c.Keep = keep }
}

func FailingBatch(err error) func(*Counting) {
	return func(c *Counting) { c.BatchErr = err }
}

// Reset zeroes the counts, so setup requests do not count.
func (c *Counting) Reset() {
	c.Singles.Store(0)
	c.Batches.Store(0)
	c.Queries.Store(0)
}

func (c *Counting) ConvertMany(ctx context.Context, amounts map[string]int64, to string, on time.Time) (int64, []string, time.Time, error) {
	return c.Inner.ConvertMany(ctx, amounts, to, on)
}

func (c *Counting) Rate(ctx context.Context, from, to string, on time.Time) (decimal.Decimal, time.Time, error) {
	c.Singles.Add(1)
	return c.Inner.Rate(ctx, from, to, on)
}

func (c *Counting) RatesOn(ctx context.Context, queries []marketdata.RateQuery) (marketdata.Rates, error) {
	c.Batches.Add(1)
	c.Queries.Add(int64(len(queries)))
	if c.BatchErr != nil {
		// The zero Rates with the error, as RatesOn returns on failure.
		return marketdata.Rates{}, c.BatchErr
	}
	if c.Keep == nil {
		return c.Inner.RatesOn(ctx, queries)
	}
	kept := make([]marketdata.RateQuery, 0, len(queries))
	for _, q := range queries {
		if c.Keep(q) {
			kept = append(kept, q)
		}
	}
	return c.Inner.RatesOn(ctx, kept)
}

// Fixed answers every pair at one rate, with no date. It is for tests that
// call a handler's conversion directly, so its batch and total panic: an
// empty answer there would read as a missing rate and pass for the wrong
// reason.
type Fixed struct{ At decimal.Decimal }

func (f Fixed) Rate(context.Context, string, string, time.Time) (decimal.Decimal, time.Time, error) {
	return f.At, time.Time{}, nil
}

func (Fixed) RatesOn(context.Context, []marketdata.RateQuery) (marketdata.Rates, error) {
	panic("ratetest.Fixed: RatesOn not used")
}

func (Fixed) ConvertMany(context.Context, map[string]int64, string, time.Time) (int64, []string, time.Time, error) {
	panic("ratetest.Fixed: ConvertMany not used")
}

// Failing fails every lookup with Err: an outage, which a real converter
// cannot be made to have on demand, as opposed to marketdata.ErrNoRate.
type Failing struct{ Err error }

func (f Failing) Rate(context.Context, string, string, time.Time) (decimal.Decimal, time.Time, error) {
	return decimal.Decimal{}, time.Time{}, f.Err
}

func (f Failing) RatesOn(context.Context, []marketdata.RateQuery) (marketdata.Rates, error) {
	return marketdata.Rates{}, f.Err
}

func (f Failing) ConvertMany(context.Context, map[string]int64, string, time.Time) (int64, []string, time.Time, error) {
	return 0, nil, time.Time{}, f.Err
}

// Resolver is the one-pair half of a converter.
type Resolver interface {
	Rate(ctx context.Context, from, to string, on time.Time) (decimal.Decimal, time.Time, error)
}

// BatchFrom answers a batch from r's own Rate, so a double cannot answer the
// batch and the pair differently. ErrNoRate stays with its query; anything
// else voids the batch, as RatesOn does.
func BatchFrom(ctx context.Context, r Resolver, queries []marketdata.RateQuery) (marketdata.Rates, error) {
	out := make(map[marketdata.RateQuery]marketdata.RateResult, len(queries))
	for _, q := range queries {
		rate, on, err := r.Rate(ctx, q.From, q.To, q.On)
		switch {
		case err == nil:
			out[q] = marketdata.RateResult{Rate: rate, RateDate: on}
		case errors.Is(err, marketdata.ErrNoRate):
			out[q] = marketdata.RateResult{Err: err}
		default:
			return marketdata.Rates{}, err
		}
	}
	return marketdata.NewRates(out), nil
}
