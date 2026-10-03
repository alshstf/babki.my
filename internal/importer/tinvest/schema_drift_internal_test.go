package tinvest

import (
	"slices"
	"testing"

	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/testdb"
)

// Every writer of the journal names itself with a source the column holds, and
// the column holds no source nobody writes: a person, a table, the
// corporate-actions registry, this importer. A source added to one side only is
// a constraint violation on the first write, answered as a 500.
func TestTheSchemaNamesExactlyTheJournalSourcesTheCodeWrites(t *testing.T) {
	inCode := []string{operation.SourceManual, operation.SourceTable, operation.SourceRegistry, Source}
	slices.Sort(inCode)
	if inSchema := testdb.CheckLiterals(t, testdb.New(t), "operations_source_check"); !slices.Equal(inSchema, inCode) {
		t.Errorf("the schema allows %v, the code writes %v", inSchema, inCode)
	}
}
