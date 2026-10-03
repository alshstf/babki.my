package apitypes_test

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// Every request of the contract belongs to exactly one of the groups its head
// declares — the module that serves it — so the description reads by module
// rather than as one list of sixty.
func TestEveryRequestOfTheContractHasItsGroup(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read api/openapi.yaml: %v", err)
	}
	var doc struct {
		Tags []struct {
			Name string `yaml:"name"`
		} `yaml:"tags"`
		Paths map[string]map[string]struct {
			Tags []string `yaml:"tags"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse api/openapi.yaml: %v", err)
	}
	declared := map[string]bool{}
	for _, tag := range doc.Tags {
		declared[tag.Name] = true
	}
	used := map[string]bool{}
	for path, ops := range doc.Paths {
		for method, op := range ops {
			if method == "parameters" {
				continue
			}
			if len(op.Tags) != 1 || !declared[op.Tags[0]] {
				t.Errorf("%s %s: tags %v, want one of the groups the contract declares", method, path, op.Tags)
				continue
			}
			used[op.Tags[0]] = true
		}
	}
	for name := range declared {
		if !used[name] {
			t.Errorf("group %q is declared and holds no request", name)
		}
	}
}
