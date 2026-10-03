package account

import (
	"slices"
	"testing"

	"babki.my/babki/internal/platform/testdb"
)

// The account types the code accepts are the ones the column holds: a type
// added here without a migration would pass validation and fail the insert.
func TestAccountTypesAreTheColumnsOwn(t *testing.T) {
	var code []string
	for typ := range validTypes {
		code = append(code, string(typ))
	}
	slices.Sort(code)
	if db := testdb.CheckValues(t, testdb.New(t), "accounts", "type"); !slices.Equal(code, db) {
		t.Errorf("account types: code %v, database %v", code, db)
	}
}
