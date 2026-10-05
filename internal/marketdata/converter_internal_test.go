package marketdata

import (
	"context"
	"errors"
	"testing"
	"time"
)

// On the all-absent walk the enumeration and the resolution consult exactly
// the same rows: the full prefetch answers every question, and dropping any
// one candidate fails loudly instead of reading as "no rate". With real rows
// the prefetch is a safe superset.
func TestPrefetchEnumeratesExactlyWhatTheAllAbsentWalkConsults(t *testing.T) {
	ctx := context.Background()
	on := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)

	// USD -> EUR walks the whole tree: direct, inverse, both bridge legs.
	rec := &recordingRows{}
	if _, _, err := rateVia(ctx, rec, "USD", "EUR", on); !errors.Is(err, ErrNoRate) {
		t.Fatalf("enumeration pass: err = %v, want ErrNoRate (recordingRows answers everything with 'absent')", err)
	}
	if len(rec.keys) == 0 {
		t.Fatal("enumeration recorded no candidates at all")
	}

	full := prefetchedRows{asked: rec.seen}
	if _, _, err := rateVia(ctx, full, "USD", "EUR", on); !errors.Is(err, ErrNoRate) {
		t.Fatalf("resolution over the full enumeration: err = %v, want ErrNoRate — every candidate it asks for was enumerated", err)
	}

	for _, dropped := range rec.keys {
		partial := make(map[FxRateKey]struct{}, len(rec.seen))
		for k := range rec.seen {
			if k != dropped {
				partial[k] = struct{}{}
			}
		}
		_, _, err := rateVia(ctx, prefetchedRows{asked: partial}, "USD", "EUR", on)
		if err == nil || errors.Is(err, ErrNoRate) {
			t.Fatalf("resolution with %s/%s dropped from the prefetch: err = %v, want a loud error (never ErrNoRate)",
				dropped.Base, dropped.Quote, err)
		}
	}
}

// A prefetched key with no row is an honest absence; an unprefetched key is an
// error.
func TestPrefetchedRowsTellsAbsenceApartFromIgnorance(t *testing.T) {
	ctx := context.Background()
	on := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	asked := FxRateKey{Base: "USD", Quote: "RUB", On: on}
	unasked := FxRateKey{Base: "EUR", Quote: "RUB", On: on}

	src := prefetchedRows{asked: map[FxRateKey]struct{}{asked: {}}}

	if _, ok, err := src.rateOn(ctx, asked.Base, asked.Quote, asked.On); ok || err != nil {
		t.Fatalf("prefetched but unanswered key: ok = %v, err = %v, want false and no error", ok, err)
	}
	_, ok, err := src.rateOn(ctx, unasked.Base, unasked.Quote, unasked.On)
	if ok {
		t.Fatal("un-prefetched key reported ok = true")
	}
	if err == nil || errors.Is(err, ErrNoRate) {
		t.Fatalf("un-prefetched key: err = %v, want a loud error that is not ErrNoRate", err)
	}
}

// The prefetch's enumeration covers each non-identity currency's whole
// resolution tree, and nothing for an identity one (#72).
func TestThePrefetchEnumeratesEveryCurrencyItIsGiven(t *testing.T) {
	ctx := context.Background()
	on := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)

	candidates := &recordingRows{}
	for _, currency := range []string{"USD", "EUR", "RUB"} {
		_, _, _ = rateVia(ctx, candidates, currency, "RUB", on)
	}

	// Into the hub, the from->RUB leg collapses onto the same keys and the RUB->to
	// leg asks for RUB/RUB; RUB->RUB as a whole short-circuits.
	want := []FxRateKey{
		{Base: "USD", Quote: "RUB", On: on},
		{Base: "RUB", Quote: "USD", On: on},
		{Base: "RUB", Quote: "RUB", On: on},
		{Base: "EUR", Quote: "RUB", On: on},
		{Base: "RUB", Quote: "EUR", On: on},
	}
	if len(candidates.keys) != len(want) {
		t.Fatalf("enumeration recorded %v, want exactly %v", candidates.keys, want)
	}
	for _, k := range want {
		if _, ok := candidates.seen[k]; !ok {
			t.Fatalf("enumeration recorded %v, missing %v", candidates.keys, k)
		}
	}
}

// A prefetched pair with no rate is that query's ErrNoRate; an unprefetched
// row voids the whole result.
func TestIncompletePrefetchVoidsTheWholeCall(t *testing.T) {
	ctx := context.Background()
	on := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	queries := []RateQuery{
		{From: "USD", To: "EUR", On: on},
		{From: "USD", To: "USD", On: on},
	}

	// Enumerated in full, answered by nothing: an honest gap.
	candidates := &recordingRows{}
	for _, q := range queries {
		_, _, _ = rateVia(ctx, candidates, q.From, q.To, q.On)
	}
	got, err := resolveQueries(ctx, prefetchedRows{asked: candidates.seen}, queries)
	if err != nil {
		t.Fatalf("resolveQueries over a complete prefetch: err = %v, want nil", err)
	}
	res, lookupErr := got.For(queries[0].From, queries[0].To, queries[0].On)
	if lookupErr != nil {
		t.Fatalf("USD->EUR: For returned %v, want the entry itself", lookupErr)
	}
	if !errors.Is(res.Err, ErrNoRate) {
		t.Fatalf("USD->EUR with nothing prefetched for it: Err = %v, want ErrNoRate in the result", res.Err)
	}
	res, lookupErr = got.For(queries[1].From, queries[1].To, queries[1].On)
	if lookupErr != nil {
		t.Fatalf("the identity query alongside it: For returned %v, want the entry itself", lookupErr)
	}
	if res.Err != nil {
		t.Fatalf("the identity query alongside it: Err = %v, want nil — one unresolvable pair must not spoil its neighbours", res.Err)
	}

	// Nothing enumerated at all: a bug, and it takes the call down with it.
	got, err = resolveQueries(ctx, prefetchedRows{}, queries)
	if err == nil || errors.Is(err, ErrNoRate) {
		t.Fatalf("resolveQueries over an empty prefetch: err = %v, want a loud error that is not ErrNoRate", err)
	}
	// The voided batch is the zero Rates, which refuses to answer.
	if got.Len() != 0 {
		t.Fatalf("resolveQueries over an empty prefetch returned %d entries, want none", got.Len())
	}
	if _, lookupErr := got.For(queries[0].From, queries[0].To, queries[0].On); !errors.Is(lookupErr, ErrNotRequested) {
		t.Fatalf("For on the voided batch: err = %v, want ErrNotRequested", lookupErr)
	}
}
