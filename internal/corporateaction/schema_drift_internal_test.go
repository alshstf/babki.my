package corporateaction

import (
	"slices"
	"testing"

	"babki.my/babki/internal/platform/testdb"
)

// TestTheSchemaNamesExactlyTheKindsAndSourcesTheCodeKnows: both lists live in a
// CHECK on instrument_events and in this package. A value added to one and not
// the other is a constraint violation on the first write that uses it.
func TestTheSchemaNamesExactlyTheKindsAndSourcesTheCodeKnows(t *testing.T) {
	pool := testdb.New(t)

	inCode := make([]string, 0, len(kinds))
	for _, k := range kinds {
		inCode = append(inCode, string(k))
	}
	slices.Sort(inCode)
	if inSchema := testdb.CheckLiterals(t, pool, "instrument_events_kind_check"); !slices.Equal(inSchema, inCode) {
		t.Errorf("kinds: the schema allows %v, the code knows %v", inSchema, inCode)
	}

	sources := []string{SourceManual, SourceMOEX, SourceYahoo, SourceKnown}
	slices.Sort(sources)
	if inSchema := testdb.CheckLiterals(t, pool, "instrument_events_source_check"); !slices.Equal(inSchema, sources) {
		t.Errorf("sources: the schema allows %v, the code knows %v", inSchema, sources)
	}
}
