// Package archtest holds the tests that keep the module structure what
// docs/architecture.md says it is. It has no code of its own.
package archtest

import (
	"os/exec"
	"sort"
	"strings"
	"testing"
)

const module = "babki.my/babki/"

// layers puts every package of the program on a layer. A package may import
// packages of its own layer or a lower one, never a higher one: the platform
// knows nothing of the domain, the core of the accounting knows nothing of
// importers, exports or the binary that wires them. A package missing from
// this table fails the test — a new one is placed here on purpose.
var layers = map[string]int{
	"internal/platform/":       0,
	"internal/family":          1,
	"internal/instrument":      2,
	"internal/marketdata":      2,
	"internal/account":         2,
	"internal/category":        2,
	"internal/portfolio":       3,
	"internal/operation":       3,
	"internal/corporateaction": 3,
	"internal/importer/":       4,
	"internal/export":          4,
	"internal/cashflow":        4,
	"internal/payouts":         4,
	"cmd/":                     5,
	// The background jobs register the workers of every module, so they sit
	// with the binary that wires the modules.
	"internal/background": 5,
	"web":                 5,
	"internal/archtest":   5,
}

// layerOf is the layer of a package: the longest entry of layers that is the
// package itself or a prefix of it ending at a path boundary.
func layerOf(pkg string) (int, bool) {
	best, layer, found := -1, 0, false
	for prefix, l := range layers {
		match := pkg == prefix || (strings.HasSuffix(prefix, "/") && strings.HasPrefix(pkg, prefix)) ||
			strings.HasPrefix(pkg, prefix+"/")
		if match && len(prefix) > best {
			best, layer, found = len(prefix), l, true
		}
	}
	return layer, found
}

// No package imports one of a higher layer (non-test code only: a test may
// reach wherever it has to).
func TestNoPackageImportsAHigherLayer(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", `{{.ImportPath}}{{range .Imports}} {{.}}{{end}}`, module+"...").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	var violations, unplaced []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		pkg := strings.TrimPrefix(fields[0], module)
		from, ok := layerOf(pkg)
		if !ok {
			unplaced = append(unplaced, pkg)
			continue
		}
		for _, imp := range fields[1:] {
			if !strings.HasPrefix(imp, module) {
				continue
			}
			dep := strings.TrimPrefix(imp, module)
			to, ok := layerOf(dep)
			if ok && to > from {
				violations = append(violations, pkg+" (layer "+itoa(from)+") imports "+dep+" (layer "+itoa(to)+")")
			}
		}
	}
	sort.Strings(unplaced)
	for _, pkg := range unplaced {
		t.Errorf("%s is on no layer: place it in layers", pkg)
	}
	for _, v := range violations {
		t.Error(v)
	}
}

func itoa(n int) string { return string(rune('0' + n)) }
