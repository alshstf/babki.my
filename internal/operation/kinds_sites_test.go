package operation

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"babki.my/babki/internal/instrument"
)

// The forms say a paper does not fit before Save, through fitsKind in
// web/src/lib/operation-kinds.ts; its table must be this one.
const kindsSite = "web/src/lib/operation-kinds.ts"

func TestTheFormsKnowWhichPaperEachEntryFits(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", kindsSite))
	if err != nil {
		t.Fatal(err)
	}
	line := regexp.MustCompile(`(?m)^  (\w+): \[([^\]]*)\],$`)
	kind := regexp.MustCompile(`"(\w+)"`)
	web := map[Type][]instrument.Type{}
	for _, m := range line.FindAllStringSubmatch(string(body), -1) {
		var kinds []instrument.Type
		for _, k := range kind.FindAllStringSubmatch(m[2], -1) {
			kinds = append(kinds, instrument.Type(k[1]))
		}
		web[Type(m[1])] = kinds
	}
	if len(web) != len(kindsFor) {
		t.Errorf("%s rules %d types, the server %d", kindsSite, len(web), len(kindsFor))
	}
	for typ, want := range kindsFor {
		if got := web[typ]; !slices.Equal(got, want) {
			t.Errorf("%s: %s fits %v, the server %v", kindsSite, typ, got, want)
		}
	}
}
