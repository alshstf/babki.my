package marketdata

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
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

// recordedRows is an fxRateRows that answers with one fixed row and remembers
// every key it was asked for. It stands in for the store under warmRows, so the
// fallback can be observed without a database: what matters is WHETHER the
// question was passed on, not what came back.
type recordedRows struct {
	row  FxRate
	ok   bool
	seen []FxRateKey
}

func (r *recordedRows) rateOn(_ context.Context, base, quote string, on time.Time) (FxRate, bool, error) {
	r.seen = append(r.seen, FxRateKey{Base: base, Quote: quote, On: on})
	return r.row, r.ok, nil
}

// TestWarmRowsAsksTheStoreOnlyForWhatWasNotPrefetched pins the property that
// makes ConvertMany's prewarm safe to have at all: it is a cache, so a row it
// does not hold costs a query and never a wrong answer.
//
// The two halves are opposites and both matter:
//
//   - a key that WAS prefetched is answered from the batch, absent or present,
//     and the store is never asked. Retrying an absent-but-requested key would
//     undo the whole saving, one query per pair that legitimately has no rate;
//   - a key that was NOT prefetched is passed to the store. Answering it as
//     absent — which is what prefetchedRows' shape would do here — would turn a
//     hole in the enumeration into a currency reported as having no rate, and
//     that is a figure quietly missing from a total rather than an error.
//
// No database: this is about which source gets the question.
func TestWarmRowsAsksTheStoreOnlyForWhatWasNotPrefetched(t *testing.T) {
	ctx := context.Background()
	on := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	present := FxRateKey{Base: "USD", Quote: "RUB", On: on}
	absent := FxRateKey{Base: "EUR", Quote: "RUB", On: on}
	unasked := FxRateKey{Base: "CHF", Quote: "RUB", On: on}

	prefetched := FxRate{Base: "USD", Quote: "RUB", On: on, Rate: decimal.NewFromInt(90)}
	fromStore := FxRate{Base: "CHF", Quote: "RUB", On: on, Rate: decimal.NewFromInt(95)}
	fallback := &recordedRows{row: fromStore, ok: true}
	w := warmRows{
		asked:    map[FxRateKey]struct{}{present: {}, absent: {}},
		rows:     map[FxRateKey]FxRate{present: prefetched},
		fallback: fallback,
	}

	got, ok, err := w.rateOn(ctx, present.Base, present.Quote, present.On)
	if err != nil || !ok || !got.Rate.Equal(prefetched.Rate) {
		t.Fatalf("prefetched key: (%v, %v, %v), want the batch's own row %s", got.Rate, ok, err, prefetched.Rate)
	}
	if _, ok, err = w.rateOn(ctx, absent.Base, absent.Quote, absent.On); ok || err != nil {
		t.Fatalf("prefetched-but-empty key: ok = %v, err = %v, want an honest absence and no error", ok, err)
	}
	if len(fallback.seen) != 0 {
		t.Fatalf("the store was asked for %v, but both keys were prefetched — a batched pair must never cost a query", fallback.seen)
	}

	got, ok, err = w.rateOn(ctx, unasked.Base, unasked.Quote, unasked.On)
	if err != nil || !ok || !got.Rate.Equal(fromStore.Rate) {
		t.Fatalf("un-prefetched key: (%v, %v, %v), want the store's row %s — a hole in the enumeration must cost a query, not an answer",
			got.Rate, ok, err, fromStore.Rate)
	}
	if len(fallback.seen) != 1 || fallback.seen[0] != unasked {
		t.Fatalf("the store was asked for %v, want exactly [%v]", fallback.seen, unasked)
	}
}

// TestPrewarmEnumeratesEveryCurrencyItIsGiven checks the half of ConvertMany's
// prewarm that decides what the one round trip is for: every non-identity
// currency in the map must put its whole resolution tree into the batch, and an
// identity conversion must put nothing there at all.
//
// It matters because the enumeration is what the round-trip count rests on: a
// currency left out of it is not a wrong number (warmRows asks the store) but
// it is a query per screen, which is exactly what #72 is about, and nothing
// else in this package would notice.
func TestPrewarmEnumeratesEveryCurrencyItIsGiven(t *testing.T) {
	ctx := context.Background()
	on := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)

	candidates := &recordingRows{}
	for _, currency := range []string{"USD", "EUR", "RUB"} {
		_, _, _ = rateVia(ctx, candidates, currency, "RUB", on)
	}

	// USD->RUB and EUR->RUB each walk direct then inverse against the hub; the
	// from->RUB bridge leg then collapses onto those same two keys because the
	// target IS the hub, and the RUB->to leg becomes RUB/RUB, which
	// resolveRate asks for in earnest (only rateVia short-circuits an identity,
	// and the bridge does not go through it). RUB->RUB as a whole conversion
	// does short-circuit, and records nothing of its own.
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
