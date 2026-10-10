package archtest

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// owners gives every table of the schema to the module that keeps it. A
// module's SQL names its own tables; the core of the accounting — the catalog,
// the journal, the engine and the registry — shares its tables within itself
// (plan item 2.3), and every other module reaches them through the core's
// stores. A table missing here fails the test: a new one is given an owner on
// purpose.
var owners = map[string]string{
	"meta":                        "internal/platform/jobs",
	"job_outcomes":                "internal/platform/jobs",
	"job_progress":                "internal/platform/jobs",
	"users":                       "internal/family",
	"spaces":                      "internal/family",
	"memberships":                 "internal/family",
	"sessions":                    "internal/family",
	"accounts":                    "internal/account",
	"account_balances":            "internal/account",
	"categories":                  "internal/category",
	"category_defaults":           "internal/category",
	"category_rules":              "internal/category",
	"loans":                       "internal/loan",
	"credit_cards":                "internal/creditcard",
	"instruments":                 "internal/instrument",
	"quotes":                      "internal/marketdata",
	"fx_rates":                    "internal/marketdata",
	"reference_prices":            "internal/marketdata",
	"bond_days":                   "internal/marketdata",
	"bond_events":                 "internal/marketdata",
	"instrument_dividends":        "internal/marketdata",
	"operations":                  "internal/operation",
	"operation_transfer_lots":     "internal/operation",
	"operation_stated_purchases":  "internal/operation",
	"dividend_withheld_stated":    "internal/operation",
	"instrument_events":           "internal/corporateaction",
	"table_imports":               "internal/importer/table",
	"table_import_operations":     "internal/importer/table",
	"tinvest_connections":         "internal/importer/tinvest",
	"tinvest_account_links":       "internal/importer/tinvest",
	"tinvest_operations_mirror":   "internal/importer/tinvest",
	"tinvest_instrument_map":      "internal/importer/tinvest",
	"tinvest_sync_runs":           "internal/importer/tinvest",
	"tinvest_mirror_explanations": "internal/importer/tinvest",
	"tinvest_trade_settlements":   "internal/importer/tinvest",
	"tinvest_settlement_reports":  "internal/importer/tinvest",
}

// core is the accounting's core, whose modules share their tables.
var core = map[string]bool{
	"internal/instrument":      true,
	"internal/operation":       true,
	"internal/portfolio":       true,
	"internal/corporateaction": true,
}

// crossings are the files that still name another module's tables, each with
// why. The list may only shrink: a file that stops crossing must leave it, and
// a new crossing fails the test until it goes through the owner's store or is
// written here with its reason.
var crossings = map[string]map[string]string{
	// The journal locks the account row while it writes, so an archived account
	// takes no entries and two writers queue: the lock is the write's own.
	"internal/operation/store.go": {
		"accounts": "the account row locked for the write, and its status read under the lock",
	},
	// The demo data fills a sync run so the connection screen has history.
	"cmd/babki/seed.go": {
		"tinvest_sync_runs": "the demo connection's past runs",
	},
}

// tableRef finds a table named after FROM, JOIN, INTO or UPDATE.
var tableRef = regexp.MustCompile(`(?i)\b(?:from|join|into|update)\s+([a-z_][a-z0-9_]*)`)

// rawString is a Go raw string literal, where this code keeps its SQL.
var rawString = regexp.MustCompile("`[^`]*`")

// No module names another module's table in its SQL, outside the core and the
// crossings written down above.
func TestEveryTableIsReadOnlyByItsOwner(t *testing.T) {
	root := filepath.Join("..", "..")
	found := map[string]map[string]bool{}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			module := filepath.ToSlash(filepath.Dir(rel))
			for _, lit := range rawString.FindAllString(string(body), -1) {
				for _, m := range tableRef.FindAllStringSubmatch(lit, -1) {
					owner, known := owners[m[1]]
					if !known || module == owner || strings.HasPrefix(module, owner+"/") || (core[module] && core[owner]) {
						continue
					}
					if found[rel] == nil {
						found[rel] = map[string]bool{}
					}
					found[rel][m[1]] = true
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	var news, stale []string
	for file, tables := range found {
		for table := range tables {
			if _, allowed := crossings[file][table]; !allowed {
				news = append(news, file+" reads "+table+" ("+owners[table]+")")
			}
		}
	}
	for file, tables := range crossings {
		for table := range tables {
			if !found[file][table] {
				stale = append(stale, file+" no longer reads "+table)
			}
		}
	}
	sort.Strings(news)
	sort.Strings(stale)
	for _, v := range news {
		t.Errorf("%s: go through the owner's store, or write the crossing down with its reason", v)
	}
	for _, v := range stale {
		t.Errorf("%s: take it off the crossings", v)
	}
}

// createTable names the tables a migration creates.
var createTable = regexp.MustCompile(`(?i)CREATE TABLE (?:IF NOT EXISTS )?([a-z_][a-z0-9_]*)`)

// Every table of the schema has an owner.
func TestEveryTableHasAnOwner(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "platform", "db", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("migrations: %v (%d files)", err, len(files))
	}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range createTable.FindAllStringSubmatch(string(body), -1) {
			if _, ok := owners[m[1]]; !ok {
				t.Errorf("%s creates %s, which has no owner in archtest's owners", filepath.Base(f), m[1])
			}
		}
	}
}
