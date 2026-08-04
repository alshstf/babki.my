package marketdata_test

import (
	"errors"
	"fmt"
	"testing"

	"babki.my/babki/internal/marketdata"
)

// This file covers Rates.Answered — the one statement of how a prefetch is
// consumed, which until now was twenty lines copied verbatim into three HTTP
// handlers. None of it needs a database: what is under test is the walk over an
// already-resolved batch, and marketdata.NewRates builds one directly.
//
// The three handlers each keep their own memo under their own unexported key,
// so what they share is the walk and not the filing. Each of them still pins
// these same properties end to end in its own package (see
// TestAccountsFailedBatchCostsTripsNotNumbers and its journal and positions
// twins, and the gap tests beside them); the tests here are the same properties
// stated where the code now lives, not a replacement for those.

// answeredPairs walks rates over queries and renders what came back as
// comparable strings, so a test can state the whole expected walk in one
// literal instead of indexing into it.
func answeredPairs(rates marketdata.Rates, queries []marketdata.RateQuery) []string {
	var out []string
	for q, res := range rates.Answered(queries) {
		out = append(out, fmt.Sprintf("%s->%s@%s rate=%s err=%v",
			q.From, q.To, q.On.Format("2006-01-02"), res.Rate, res.Err))
	}
	return out
}

// query is one RateQuery on a fixed day, for the tables below.
func query(from, to, on string) marketdata.RateQuery {
	return marketdata.RateQuery{From: from, To: to, On: date(on)}
}

// TestAnsweredSkipsWhatTheBatchNeverResolved is the property that keeps a
// prefetch from ever costing a number.
//
// A query the batch did not answer comes back from For as ErrNotRequested,
// which says the enumeration and the batch disagree — a bug in the calling
// code, never a gap in the data. Handing it to a caller that files whatever it
// is given would put that bug in the memo, where it reads as an answer.
// Skipping it leaves the memo cold, and the caller's own per-pair fallback
// resolves the very same figure one round trip dearer.
func TestAnsweredSkipsWhatTheBatchNeverResolved(t *testing.T) {
	asked := []marketdata.RateQuery{
		query("USD", "RUB", "2026-07-01"),
		query("EUR", "RUB", "2026-07-01"),
		query("GBP", "RUB", "2026-07-01"),
	}
	// Only the first and last are in the batch: the middle one is what an
	// enumeration that has fallen behind the rules looks like from here.
	resolved := marketdata.NewRates(map[marketdata.RateQuery]marketdata.RateResult{
		asked[0]: {Rate: dec("90"), RateDate: date("2026-06-30")},
		asked[2]: {Rate: dec("115"), RateDate: date("2026-06-30")},
	})

	got := answeredPairs(resolved, asked)
	want := []string{
		"USD->RUB@2026-07-01 rate=90 err=<nil>",
		"GBP->RUB@2026-07-01 rate=115 err=<nil>",
	}
	if !equalStrings(got, want) {
		t.Fatalf("Answered walked\n got %q\nwant %q — a query the batch never resolved must be skipped, not handed back for filing",
			got, want)
	}
}

// TestAnsweredHandsBackAGenuineGapAsAnAnswer is the other side of the same
// line, and the one a reader is likeliest to get wrong: a RateResult whose Err
// is ErrNoRate is an ANSWER. Nothing connects this pair on this date, the batch
// went and found that out, and filing it is what makes the figure it belongs to
// come out as the gap it should be — without a second lookup that could only
// reach the same conclusion.
//
// Skipping it instead would change no number anywhere: the memo would be cold,
// the fallback would ask the store and be told the same thing. It would only
// cost a round trip per gap, invisibly. That is why the walk is pinned here by
// what it yields rather than only by what the handlers publish.
func TestAnsweredHandsBackAGenuineGapAsAnAnswer(t *testing.T) {
	noRate := fmt.Errorf("%w: XXX -> RUB on 2026-07-01", marketdata.ErrNoRate)
	asked := []marketdata.RateQuery{
		query("USD", "RUB", "2026-07-01"),
		query("XXX", "RUB", "2026-07-01"),
	}
	resolved := marketdata.NewRates(map[marketdata.RateQuery]marketdata.RateResult{
		asked[0]: {Rate: dec("90"), RateDate: date("2026-06-30")},
		asked[1]: {Err: noRate},
	})

	var gaps int
	for _, res := range resolved.Answered(asked) {
		if res.Err == nil {
			continue
		}
		gaps++
		if !errors.Is(res.Err, marketdata.ErrNoRate) {
			t.Fatalf("Answered yielded an entry carrying %v, want one carrying ErrNoRate", res.Err)
		}
	}
	if gaps != 1 {
		t.Fatalf("Answered yielded %d entries carrying an error, want exactly 1 — a pair the batch resolved to «no rate» is an honest answer and must be handed back as one",
			gaps)
	}
}

// TestAnsweredWalksEveryQueryInTheOrderGiven pins the shape of the walk itself:
// one visit per query as the caller listed them, duplicates included. The
// handlers file into a map keyed by the triple, so a repeat is harmless there —
// but it is the caller's list that decides what gets filed, and a walk that
// deduplicated or reordered on its own would be a second opinion about which
// queries matter.
func TestAnsweredWalksEveryQueryInTheOrderGiven(t *testing.T) {
	usd := query("USD", "RUB", "2026-07-01")
	eur := query("EUR", "RUB", "2026-07-02")
	resolved := marketdata.NewRates(map[marketdata.RateQuery]marketdata.RateResult{
		usd: {Rate: dec("90"), RateDate: date("2026-06-30")},
		eur: {Rate: dec("100"), RateDate: date("2026-07-02")},
	})

	got := answeredPairs(resolved, []marketdata.RateQuery{eur, usd, eur})
	want := []string{
		"EUR->RUB@2026-07-02 rate=100 err=<nil>",
		"USD->RUB@2026-07-01 rate=90 err=<nil>",
		"EUR->RUB@2026-07-02 rate=100 err=<nil>",
	}
	if !equalStrings(got, want) {
		t.Fatalf("Answered walked\n got %q\nwant %q", got, want)
	}
}

// TestAnsweredOverTheZeroRatesYieldsNothing is the safety net under a caller
// that ignores the error RatesOn returned beside its Rates. RatesOn hands back
// the zero Rates on failure, and the zero Rates answers every For with
// ErrNotRequested — so a walk over it files nothing at all and every figure
// falls back to a per-pair lookup, which is exactly what a dead batch should
// cost. Nothing here depends on the caller having checked.
func TestAnsweredOverTheZeroRatesYieldsNothing(t *testing.T) {
	got := answeredPairs(marketdata.Rates{}, []marketdata.RateQuery{
		query("USD", "RUB", "2026-07-01"),
		query("EUR", "RUB", "2026-07-01"),
	})
	if len(got) != 0 {
		t.Fatalf("Answered over the zero Rates yielded %q, want nothing — a batch that failed outright must fill no memo entry at all", got)
	}
}

// TestAnsweredStopsWhenTheCallerStops pins the half of the iterator contract
// that no handler exercises today: a caller that breaks out of the range must
// end the walk, not have it run on over the rest of the queries. Today's three
// callers all walk to the end, so nothing else in this codebase would notice an
// iterator that ignored a false from yield — and a range-over-func that does
// that is a panic in the caller's next break, not a slow loop.
func TestAnsweredStopsWhenTheCallerStops(t *testing.T) {
	asked := []marketdata.RateQuery{
		query("USD", "RUB", "2026-07-01"),
		query("EUR", "RUB", "2026-07-01"),
		query("GBP", "RUB", "2026-07-01"),
	}
	results := make(map[marketdata.RateQuery]marketdata.RateResult, len(asked))
	for _, q := range asked {
		results[q] = marketdata.RateResult{Rate: dec("90"), RateDate: date("2026-06-30")}
	}
	resolved := marketdata.NewRates(results)

	visited := 0
	for range resolved.Answered(asked) {
		visited++
		break
	}
	if visited != 1 {
		t.Fatalf("a caller that broke after one entry saw %d, want 1", visited)
	}
}

// equalStrings compares two slices element by element, treating nil and empty
// as the same thing — a walk that yields nothing returns a nil slice from
// answeredPairs, and a test naming an empty expectation means the same.
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
