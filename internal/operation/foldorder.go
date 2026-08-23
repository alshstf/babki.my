package operation

import (
	"fmt"
	"sort"
	"strings"
)

// The sources a journal row can carry. Only the two this package has rules
// about are named here; "csv" is in the column's CHECK constraint and nothing
// in Go mentions it yet.
const (
	// SourceManual is a row a person entered.
	SourceManual = "manual"

	// SourceRegistry is a row materialized from the corporate-actions registry
	// — a split that happened to the PAPER, carried into every account that
	// held it (see internal/corporateaction). Nobody types one and nobody
	// deletes one directly; the registry entry is edited and the rows follow.
	SourceRegistry = "registry"
)

// foldRank orders operations WITHIN one day, ahead of their created_at.
//
// # Why a day needs an order at all
//
// The engine folds a journal in sequence, so two operations of the same date
// are applied in whatever order the journal hands them over, and that order
// decides real figures: a sale folded before the purchase that covers it is an
// oversell the engine refuses, and among parcels bought on one day it is this
// order the FIFO queue breaks ties by. Until the registry arrived, created_at
// answered for all of it — the order things were written down in, which for a
// person's own entries is the only order there is.
//
// # Why the registry cannot use it
//
// A registry row is written LONG AFTER the trades it must precede. The owner
// buys Amazon in 2021, imports the history in 2026, and only then does the
// registry learn about the 2022 split; the split row is stamped in 2026 and
// dated 2022. By created_at it is the youngest row of its day and folds last —
// after any trade dated the split day itself, which is a trade already made in
// the NEW quantity and would be multiplied a second time.
//
// The event's own meaning settles it: EffectiveOn is the first day the paper
// trades in the new quantity, so the multiplication belongs at the START of
// that day, before anything else dated it. Rank 0 puts it there. Everything
// else keeps rank 1 and goes on being ordered by when it was written down.
//
// # One rule, two spellings, held together by a test
//
// The engine reads a journal two ways — through SQL (the ORDER BY of
// ListForEngine and its siblings) and in memory (sortJournal, used while a
// write is being checked). Both must fold a day the same way or a journal is
// accepted in one order and replayed in another, which is the exact fault this
// package spends most of its care on. So the rank is defined ONCE, in
// foldRanks below: this function reads it, and engineOrderSQL builds the SQL
// CASE expression from the same map rather than restating it.
// TestSQLAndMemoryFoldADayInTheSameOrder is what fails if they part.
func foldRank(source string) int {
	if rank, special := foldRanks[source]; special {
		return rank
	}
	return defaultFoldRank
}

// foldRanks names every source that does NOT fold in the ordinary place, and
// engineOrderSQL turns it into SQL. One entry today; the conversions and
// spin-offs the registry will materialize carry the same source and so need no
// entry of their own.
var foldRanks = map[string]int{SourceRegistry: 0}

// defaultFoldRank is where a row folds when nothing special is said about its
// source: after every ranked row of its day, in the order it was written down.
const defaultFoldRank = 1

// engineOrderSQL is the ORDER BY every query that feeds the engine uses,
// written from foldRanks so the database and sortJournal cannot come to
// disagree. The CASE arms are generated in a fixed order (sorted by source) so
// the string is stable across runs and shows up unchanged in a diff.
//
// It orders by the date first, the rank within the date, and created_at within
// the rank — the same three keys, in the same order, that sortJournal compares.
func engineOrderSQL() string {
	sources := make([]string, 0, len(foldRanks))
	for source := range foldRanks {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	// No ranked source: every row folds in the ordinary place and the CASE
	// would have no arms at all, which is not valid SQL. Written out rather
	// than left to fail, so that emptying the table produces a journal ordered
	// the old way — a wrong ANSWER a test can catch — instead of a syntax error
	// every query dies on, which is a test going red for a reason that has
	// nothing to do with what it checks.
	if len(sources) == 0 {
		return "ORDER BY occurred_on ASC, created_at ASC"
	}
	var b strings.Builder
	b.WriteString("ORDER BY occurred_on ASC, CASE source")
	for _, source := range sources {
		// The sources are this package's own constants, not anything a request
		// carries, so quoting them is a matter of writing valid SQL rather than
		// of defending against input. A source that somehow held a quote would
		// produce a query that fails to parse — loudly, at once, in every test.
		fmt.Fprintf(&b, " WHEN '%s' THEN %d", source, foldRanks[source])
	}
	fmt.Fprintf(&b, " ELSE %d END ASC, created_at ASC", defaultFoldRank)
	return b.String()
}

// engineOrder is engineOrderSQL computed once. The map it is built from is a
// package-level constant in all but name.
var engineOrder = engineOrderSQL()
