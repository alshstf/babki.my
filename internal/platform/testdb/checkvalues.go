package testdb

import (
	"context"
	"regexp"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// literal is one value of a constraint's list as PostgreSQL prints it back.
var literal = regexp.MustCompile(`'([^']*)'::text`)

// CheckValues returns, sorted, the values the CHECK (column IN (...))
// constraint on table allows, read from the migrated schema itself. It is what
// a test compares a list in the code against: a value the code accepts and the
// column refuses is a 500 on the first write, and nothing else catches it.
//
// The constraint is found by its printed form, CHECK ((column = ANY
// (ARRAY[...]))), so a constraint that only mentions the column among other
// conditions is not mistaken for its list. Exactly one must match.
func CheckValues(t *testing.T, pool *pgxpool.Pool, table, column string) []string {
	t.Helper()
	whole := regexp.MustCompile(`^CHECK \(\(` + regexp.QuoteMeta(column) + ` = ANY \(ARRAY\[(.+)\]\)\)\)$`)
	rows, err := pool.Query(context.Background(), `
		SELECT pg_get_constraintdef(c.oid) FROM pg_constraint c
		JOIN pg_class r ON r.oid = c.conrelid
		WHERE r.relname = $1 AND c.contype = 'c'`, table)
	if err != nil {
		t.Fatalf("read the constraints of %s: %v", table, err)
	}
	defer rows.Close()
	var lists []string
	for rows.Next() {
		var def string
		if err := rows.Scan(&def); err != nil {
			t.Fatalf("read the constraints of %s: %v", table, err)
		}
		if m := whole.FindStringSubmatch(def); m != nil {
			lists = append(lists, m[1])
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the constraints of %s: %v", table, err)
	}
	if len(lists) != 1 {
		t.Fatalf("%s.%s: %d constraints list its values, want exactly one", table, column, len(lists))
	}
	var out []string
	for _, m := range literal.FindAllStringSubmatch(lists[0], -1) {
		out = append(out, m[1])
	}
	slices.Sort(out)
	return out
}
