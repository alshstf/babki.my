package archtest

import (
	"sort"
	"testing"
)

// exported says, for every table, whether the family's export carries what
// is in it (internal/export): "" — it does; otherwise why not. The export
// promises everything the family entered; a table that holds such things and
// is left out loses them (#545: goals, saved plans and classes by hand were).
// A new table is decided here on purpose, as it is given an owner.
var exported = map[string]string{
	"meta":                        "the program's own state",
	"job_outcomes":                "background jobs' outcomes, the program's",
	"job_progress":                "background jobs' progress, the program's",
	"users":                       "",
	"spaces":                      "",
	"memberships":                 "",
	"sessions":                    "logins, nothing entered",
	"accounts":                    "",
	"account_balances":            "",
	"account_rates":               "",
	"account_programmes":          "",
	"account_condition_marks":     "",
	"categories":                  "",
	"category_defaults":           "a mark that the default categories were laid, not data",
	"category_rules":              "",
	"budget_limits":               "",
	"allocation_targets":          "",
	"allocation_settings":         "",
	"allocation_plans":            "",
	"valuables_storages":          "",
	"valuables_items":             "",
	"valuables_bank_prices":       "",
	"wage_entries":                "",
	"instrument_classes":          "the broker's and Yahoo's classes, read again from them",
	"instrument_class_overrides":  "",
	"savings_goals":               "",
	"savings_goal_funds":          "",
	"receipts":                    "",
	"mailboxes":                   "the mailbox's address and encrypted password: a secret, set up again by hand",
	"loans":                       "",
	"loan_prepayments":            "",
	"credit_cards":                "",
	"card_installments":           "",
	"card_bank_figures":           "",
	"push_subscriptions":          "devices' push keys, registered again by each device",
	"push_sent":                   "which reminders went out, the program's",
	"push_prefs":                  "each member's choice of reminders and quiet hours — a setting of the device's person, not the family's data",
	"recurring_hidden":            "",
	"instruments":                 "",
	"quotes":                      "",
	"fx_rates":                    "the Central Bank's rates, loaded again",
	"metal_prices":                "the Central Bank's metal prices, loaded again",
	"key_rates":                   "the Central Bank's key rates, loaded again",
	"reference_prices":            "funds' and foreign closes, loaded again",
	"bond_days":                   "the exchange's bond schedules, loaded again",
	"bond_events":                 "the exchange's bond events, loaded again",
	"index_values":                "benchmark indices, loaded again",
	"instrument_dividends":        "declared dividends, loaded again",
	"operations":                  "",
	"operation_transfer_lots":     "",
	"operation_parts":             "",
	"operation_stated_purchases":  "the statement behind a transfer's lots, which the export carries",
	"dividend_withheld_stated":    "",
	"instrument_events":           "",
	"table_imports":               "the log of tables loaded; the operations they brought are exported",
	"table_import_operations":     "the same log's rows",
	"tinvest_connections":         "the broker's encrypted token: a secret, connected again by hand",
	"tinvest_account_links":       "the connection's links, made again when connecting",
	"tinvest_operations_mirror":   "the broker's own copy, read again from it",
	"tinvest_instrument_map":      "the broker's paper names, read again from it",
	"tinvest_sync_runs":           "the log of synchronizations",
	"tinvest_mirror_explanations": "the program's reasons for the broker's rows, worked out again",
	"tinvest_trade_settlements":   "settlement days from the broker's reports, read again",
	"tinvest_settlement_reports":  "the broker's reports asked for, the program's",
}

// Every table is decided for the export, and only tables there are.
func TestEveryTableIsDecidedForTheExport(t *testing.T) {
	var missing, stale []string
	for table := range owners {
		if _, ok := exported[table]; !ok {
			missing = append(missing, table)
		}
	}
	for table := range exported {
		if _, ok := owners[table]; !ok {
			stale = append(stale, table)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	for _, table := range missing {
		t.Errorf("%s: say in archtest's exported whether the family's export carries it — and if it holds what the family entered, make it carry it", table)
	}
	for _, table := range stale {
		t.Errorf("%s: no such table any more; take it off exported", table)
	}
}
