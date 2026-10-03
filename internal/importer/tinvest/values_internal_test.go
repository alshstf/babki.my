package tinvest

import (
	"slices"
	"testing"

	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/testdb"
)

// Every writer of the journal names itself with a source the column holds, and
// the column holds no source nobody writes: this importer, a person, a table,
// the corporate-actions registry.
func TestJournalSourcesAreTheColumnsOwn(t *testing.T) {
	code := []string{Source, operation.SourceManual, operation.SourceTable, operation.SourceRegistry}
	slices.Sort(code)
	if db := testdb.CheckValues(t, testdb.New(t), "operations", "source"); !slices.Equal(code, db) {
		t.Errorf("journal sources: code %v, database %v", code, db)
	}
}
