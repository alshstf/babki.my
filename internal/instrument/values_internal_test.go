package instrument

import (
	"slices"
	"testing"

	"babki.my/babki/internal/platform/testdb"
)

// The instrument types the code accepts are the ones the column holds.
func TestInstrumentTypesAreTheColumnsOwn(t *testing.T) {
	var code []string
	for typ := range validTypes {
		code = append(code, string(typ))
	}
	slices.Sort(code)
	if db := testdb.CheckValues(t, testdb.New(t), "instruments", "type"); !slices.Equal(code, db) {
		t.Errorf("instrument types: code %v, database %v", code, db)
	}
}
