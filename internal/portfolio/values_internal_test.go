package portfolio

import (
	"slices"
	"testing"

	"babki.my/babki/internal/platform/testdb"
)

// The operation types the engine accepts are the ones the journal's column
// holds: a type added here without a migration would pass validation and fail
// the insert.
func TestOperationTypesAreTheColumnsOwn(t *testing.T) {
	var code []string
	for typ := range validTypes {
		code = append(code, string(typ))
	}
	slices.Sort(code)
	if db := testdb.CheckValues(t, testdb.New(t), "operations", "type"); !slices.Equal(code, db) {
		t.Errorf("operation types: code %v, database %v", code, db)
	}
}
