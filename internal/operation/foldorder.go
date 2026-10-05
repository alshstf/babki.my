package operation

import (
	"fmt"
	"sort"
	"strings"
)

// SourceManual is a row a person entered. SourceRegistry is declared in
// operation.go.
const SourceManual = "manual"

// foldRank orders operations within one day, ahead of their created_at.
//
// Same-day order decides real figures: a sale before its covering purchase is an
// oversell, and ties in the FIFO queue break by it. A registry row is written long
// after the trades it must precede (a 2022 split learned in 2026), so by
// created_at it would fold last on its day, after trades already in the new
// quantity, and multiply them again. EffectiveOn is the first day in the new
// quantity, so the registry row folds at the start of it: rank 0. Everything else
// is rank 1.
//
// The SQL order (engineOrderSQL) and the in-memory one (sortJournal) are both
// built from foldRanks; TestSQLAndMemoryFoldADayInTheSameOrder fails if they
// part.
func foldRank(source string) int {
	if rank, special := foldRanks[source]; special {
		return rank
	}
	return defaultFoldRank
}

// foldRanks names every source that does not fold in the ordinary place.
// Conversions and spin-offs share SourceRegistry and need no entry.
var foldRanks = map[string]int{SourceRegistry: 0}

// defaultFoldRank: after the ranked rows of the day, in the order written.
const defaultFoldRank = 1

// engineOrderSQL is the ORDER BY of every query that feeds the engine, built
// from foldRanks: date, rank, instant (rows without one last), created_at, the
// keys foldsBefore compares. CASE arms are sorted by source so the string is
// stable.
func engineOrderSQL() string { return foldOrderSQL(false) }

// listingOrderSQL is the journal screen's order: the engine's, newest first.
func listingOrderSQL() string { return foldOrderSQL(true) }

// foldOrderSQL writes the engine's order, or its exact reverse.
func foldOrderSQL(reverse bool) string {
	asc, nulls := "ASC", "NULLS LAST"
	if reverse {
		asc, nulls = "DESC", "NULLS FIRST"
	}
	sources := make([]string, 0, len(foldRanks))
	for source := range foldRanks {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	// With no ranked source the CASE would have no arms, which is not valid
	// SQL; this keeps an empty map a wrong order a test can catch rather than
	// a syntax error.
	if len(sources) == 0 {
		return fmt.Sprintf("ORDER BY occurred_on %[1]s, occurred_at %[1]s %[2]s, created_at %[1]s", asc, nulls)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "ORDER BY occurred_on %s, CASE source", asc)
	for _, source := range sources {
		// Sources are package constants, not request data.
		fmt.Fprintf(&b, " WHEN '%s' THEN %d", source, foldRanks[source])
	}
	fmt.Fprintf(&b, " ELSE %[2]d END %[1]s, occurred_at %[1]s %[3]s, created_at %[1]s", asc, defaultFoldRank, nulls)
	return b.String()
}

// foldsBefore is the engine's fold order for two rows: day, rank within the
// day (foldRank), the source's instant with timed rows first, then created_at.
// The broker's instant, not when a row arrived, decides which same-day parcel a
// sale consumes (#198); a hand row has no instant and folds after the timed rows
// of its day.
func foldsBefore(a, b Operation) bool {
	if !a.OccurredOn.Equal(b.OccurredOn) {
		return a.OccurredOn.Before(b.OccurredOn)
	}
	if ra, rb := foldRank(a.Source), foldRank(b.Source); ra != rb {
		return ra < rb
	}
	if before, decided := byInstant(a, b); decided {
		return before
	}
	return a.CreatedAt.Before(b.CreatedAt)
}

// byInstant is foldsBefore's third key: timed rows first, then by instant.
// decided is false when it cannot tell the two apart.
func byInstant(a, b Operation) (before, decided bool) {
	switch {
	case a.OccurredAt != nil && b.OccurredAt != nil:
		if a.OccurredAt.Equal(*b.OccurredAt) {
			return false, false
		}
		return a.OccurredAt.Before(*b.OccurredAt), true
	case a.OccurredAt != nil:
		return true, true
	case b.OccurredAt != nil:
		return false, true
	}
	return false, false
}

// engineOrder is engineOrderSQL computed once.
var engineOrder = engineOrderSQL()

// listingOrder is listingOrderSQL computed once.
var listingOrder = listingOrderSQL()
