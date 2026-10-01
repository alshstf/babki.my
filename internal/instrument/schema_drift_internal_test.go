package instrument

import (
	"slices"
	"testing"

	"babki.my/babki/internal/platform/testdb"
)

// TestTheSchemaNamesExactlyTheInstrumentTypesTheCodeKnows: the list lives in the
// table's CHECK and in validTypes. A type added to one and not the other is a
// constraint violation on the first write that uses it, answered as a 500.
func TestTheSchemaNamesExactlyTheInstrumentTypesTheCodeKnows(t *testing.T) {
	inCode := make([]string, 0, len(validTypes))
	for typ := range validTypes {
		inCode = append(inCode, string(typ))
	}
	slices.Sort(inCode)
	if inSchema := testdb.CheckLiterals(t, testdb.New(t), "instruments_type_check"); !slices.Equal(inSchema, inCode) {
		t.Errorf("the schema allows %v, the code knows %v", inSchema, inCode)
	}
}
