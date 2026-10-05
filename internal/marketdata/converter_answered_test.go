package marketdata_test

import (
	"errors"
	"fmt"
	"testing"

	"babki.my/babki/internal/marketdata"
)

// Rates.Answered is how handlers consume a prefetch; the handlers' own tests
// check the same properties end to end.

// answeredPairs renders what the walk yields as strings.
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

// A query the batch did not resolve is skipped, so a bug costs a round trip,
// not a number in the memo.
func TestAnsweredSkipsWhatTheBatchNeverResolved(t *testing.T) {
	asked := []marketdata.RateQuery{
		query("USD", "RUB", "2026-07-01"),
		query("EUR", "RUB", "2026-07-01"),
		query("GBP", "RUB", "2026-07-01"),
	}
	// The middle query is missing from the batch.
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

// A resolved ErrNoRate is yielded: it is an answer, and skipping it would cost
// a round trip per gap.
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

// Every query is visited once, in the caller's order, duplicates included.
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

// The zero Rates yields nothing, so a caller ignoring RatesOn's error loses only round trips.
func TestAnsweredOverTheZeroRatesYieldsNothing(t *testing.T) {
	got := answeredPairs(marketdata.Rates{}, []marketdata.RateQuery{
		query("USD", "RUB", "2026-07-01"),
		query("EUR", "RUB", "2026-07-01"),
	})
	if len(got) != 0 {
		t.Fatalf("Answered over the zero Rates yielded %q, want nothing — a batch that failed outright must fill no memo entry at all", got)
	}
}

// Breaking out of the range ends the walk.
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

// equalStrings treats nil and empty as equal.
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
