package portfolio_test

import (
	"io"
	"net/http"
	"slices"
	"testing"

	"babki.my/babki/internal/platform/apitest"
)

// costBasisRules mirrors apitypes.CostBasisRules for decoding in tests.
type costBasisRules struct {
	Country   string   `json:"country"`
	Method    string   `json:"method"`
	Perimeter string   `json:"perimeter"`
	Supported bool     `json:"supported"`
	Notices   []string `json:"notices"`
}

// positionsWithRules decodes the positions payload together with the rules
// declaration that now travels with it.
type positionsWithRules struct {
	Positions      []positionResp `json:"positions"`
	CostBasisRules costBasisRules `json:"cost_basis_rules"`
}

func getPositions(t *testing.T, c *http.Client, url, accountID string) positionsWithRules {
	t.Helper()
	resp := apitest.Do(t, c, "GET", url+"/api/v1/accounts/"+accountID+"/positions", "")
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("positions = %d: %s", resp.StatusCode, b)
	}
	var out positionsWithRules
	apitest.Decode(t, resp, &out)
	return out
}

func patchSpace(t *testing.T, c *http.Client, url, body string) {
	t.Helper()
	resp := apitest.Do(t, c, "PATCH", url+"/api/v1/space", body)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("PATCH space %s = %d: %s", body, resp.StatusCode, b)
	}
}

// The positions response declares whose cost basis rules its figures follow:
// affirmed for RU, disclaimed for GB.
func TestPositionsDeclareWhoseCostBasisRulesTheyFollow(t *testing.T) {
	url, c := newAPI(t)

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	sber := createInstrument(t, c, url, `{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)
	createOperation(t, c, url, `{"account_id":"`+acc.ID+`","type":"deposit","occurred_on":"2024-01-01","amount_minor":100000000,"currency":"RUB"}`)
	createOperation(t, c, url, `{"account_id":"`+acc.ID+`","instrument_id":"`+sber.ID+
		`","type":"buy","occurred_on":"2024-02-01","quantity":"100","price":"270.50","amount_minor":-2705000,"currency":"RUB"}`)

	got := getPositions(t, c, url, acc.ID)
	if len(got.Positions) != 1 {
		t.Fatalf("positions = %d, want 1", len(got.Positions))
	}
	if got.CostBasisRules.Country != "RU" || got.CostBasisRules.Method != "fifo" ||
		got.CostBasisRules.Perimeter != "account" || !got.CostBasisRules.Supported ||
		len(got.CostBasisRules.Notices) != 0 {
		t.Fatalf("cost_basis_rules = %+v, want RU fifo/account supported with no notices", got.CostBasisRules)
	}

	// The owner moves to Britain. Nothing about the journal changed, so the
	// figures must not change either — but what they claim to be does.
	patchSpace(t, c, url, `{"tax_residency":"GB"}`)

	after := getPositions(t, c, url, acc.ID)
	if len(after.Positions) != 1 || after.Positions[0].CostMinor != got.Positions[0].CostMinor {
		t.Fatalf("positions changed with the residency: %+v vs %+v", after.Positions, got.Positions)
	}
	if after.CostBasisRules.Country != "GB" || after.CostBasisRules.Supported {
		t.Fatalf("cost_basis_rules = %+v, want GB and supported=false", after.CostBasisRules)
	}
	notices := slices.Clone(after.CostBasisRules.Notices)
	slices.Sort(notices)
	if !slices.Equal(notices, []string{"method_mismatch", "perimeter_mismatch"}) {
		t.Errorf("notices = %v, want [method_mismatch perimeter_mismatch]", notices)
	}
}

// An empty account still carries the declaration.
func TestAnEmptyAccountStillDeclaresTheRules(t *testing.T) {
	url, c := newAPI(t)
	acc := createAccount(t, c, url, `{"name":"Пустой","type":"brokerage","currency":"RUB"}`)

	patchSpace(t, c, url, `{"tax_residency":"NL"}`)

	got := getPositions(t, c, url, acc.ID)
	if len(got.Positions) != 0 {
		t.Fatalf("positions = %d, want 0", len(got.Positions))
	}
	// NL does not tax gains: not supported, with not_taxed only.
	if got.CostBasisRules.Supported {
		t.Error("supported = true for NL, want false")
	}
	if !slices.Equal(got.CostBasisRules.Notices, []string{"not_taxed"}) {
		t.Errorf("notices = %v, want exactly [not_taxed]", got.CostBasisRules.Notices)
	}
}
