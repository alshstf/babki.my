package marketdata

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestPrefetchEnumeratesExactlyWhatTheAllAbsentWalkConsults guards the one way
// the batched path can silently lie. Prefetching is a promise that every row
// resolution will ask for is already in hand; if the promise is broken, the
// missing row must not read as "this pair has no rate" — that is a real
// answer here, shown to the user as an honest gap, and a bug wearing it would
// be indistinguishable.
//
// Both halves of the promise are checked: the full enumeration answers every
// question resolution asks (so the batch never fails loudly in production),
// and dropping ANY single enumerated candidate makes it fail loudly (so no
// candidate is enumerated "just in case" while resolution ignores it, and
// none is consulted without having been enumerated).
//
// "Exactly" is a claim about THIS walk only, which is why the name says so.
// The all-absent walk is the one where enumeration and resolution consult the
// identical set: prefetchedRows here finds nothing, so resolution takes every
// branch, just as recordingRows did. Once real rows exist, a direct hit prunes
// the branches below it and the prefetch is legitimately a superset of what
// resolution ends up asking for — which is the safe direction, and the one
// errNotPrefetched exists to keep it in.
//
// No database here on purpose: this is about the two in-memory halves
// agreeing with each other, and nothing else.
func TestPrefetchEnumeratesExactlyWhatTheAllAbsentWalkConsults(t *testing.T) {
	ctx := context.Background()
	on := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)

	// USD -> EUR: neither side is the RUB hub, so resolution walks the whole
	// tree — direct, inverse, then both bridge legs, each direct-or-inverse.
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

// TestPrefetchedRowsTellsAbsenceApartFromIgnorance is the same distinction one
// level down: a key that was prefetched and came back empty is an honest "no
// such rate" (ok=false, no error), while a key nobody prefetched is a bug in
// the caller (error).
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

// TestIncompletePrefetchVoidsTheWholeCall pins which of the two failures gets
// which treatment, the distinction the whole batched path rests on.
//
// A pair the prefetch asked about and found nothing for is that query's own
// ErrNoRate: the map still comes back, its other rows intact. A row the
// prefetch never asked about voids the map entirely — it is a bug here, and a
// bug that reported itself as ErrNoRate would be shown to the user as an
// honest missing rate, on a page where nothing else looks wrong.
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
	// The voided batch is the zero Rates, and the zero Rates refuses to answer
	// rather than handing back a zero-valued result: a caller that dropped err
	// on the floor still cannot read a fabricated rate out of it.
	if got.Len() != 0 {
		t.Fatalf("resolveQueries over an empty prefetch returned %d entries, want none", got.Len())
	}
	if _, lookupErr := got.For(queries[0].From, queries[0].To, queries[0].On); !errors.Is(lookupErr, ErrNotRequested) {
		t.Fatalf("For on the voided batch: err = %v, want ErrNotRequested", lookupErr)
	}
}
