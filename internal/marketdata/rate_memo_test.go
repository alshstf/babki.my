package marketdata_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
)

// countingSource answers every pair at rate 2 and counts both kinds of call.
type countingSource struct {
	single  int
	batches [][]marketdata.RateQuery
	fail    error
}

func (s *countingSource) Rate(_ context.Context, _, _ string, on time.Time) (decimal.Decimal, time.Time, error) {
	s.single++
	return decimal.NewFromInt(2), on, nil
}

func (s *countingSource) RatesOn(_ context.Context, queries []marketdata.RateQuery) (marketdata.Rates, error) {
	s.batches = append(s.batches, queries)
	if s.fail != nil {
		return marketdata.Rates{}, s.fail
	}
	out := map[marketdata.RateQuery]marketdata.RateResult{}
	for _, q := range queries {
		out[q] = marketdata.RateResult{Rate: decimal.NewFromInt(2), RateDate: q.On}
	}
	return marketdata.NewRates(out), nil
}

// One batch asks each pair and day once, however many times and spellings it
// was listed; what it answered costs no further call, and a miss costs one.
func TestARateMemoAsksEachPairAndDayOnce(t *testing.T) {
	ctx := context.Background()
	day := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	sameDayLater := day.Add(15 * time.Hour)
	src := &countingSource{}
	memo := marketdata.NewRateMemo(src)

	memo.Prefetch(ctx, []marketdata.RateQuery{
		{From: "USD", To: "RUB", On: day},
		{From: "USD", To: "RUB", On: sameDayLater},
		{From: "EUR", To: "RUB", On: day},
	})
	memo.Prefetch(ctx, []marketdata.RateQuery{{From: "USD", To: "RUB", On: day}})
	if len(src.batches) != 1 || len(src.batches[0]) != 2 {
		t.Fatalf("batches = %v, want one batch of the two distinct pairs", src.batches)
	}

	if res := memo.Rate(ctx, "USD", "RUB", sameDayLater); res.Err != nil || !res.Rate.Equal(decimal.NewFromInt(2)) {
		t.Fatalf("prefetched USD = %+v, want rate 2", res)
	}
	memo.Rate(ctx, "GBP", "RUB", day)
	memo.Rate(ctx, "GBP", "RUB", day)
	if src.single != 1 {
		t.Errorf("single lookups = %d, want 1: only the pair the batch missed, once", src.single)
	}
}

// A failed batch fails nothing: every rate is then resolved on its own.
func TestARateMemoSurvivesAFailedBatch(t *testing.T) {
	ctx := context.Background()
	day := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	src := &countingSource{fail: errors.New("database is down")}
	memo := marketdata.NewRateMemo(src)

	memo.Prefetch(ctx, []marketdata.RateQuery{{From: "USD", To: "RUB", On: day}})
	if res := memo.Rate(ctx, "USD", "RUB", day); res.Err != nil {
		t.Fatalf("after a failed batch, Rate = %+v, want the single lookup's answer", res)
	}
	if src.single != 1 {
		t.Errorf("single lookups = %d, want 1", src.single)
	}
}
