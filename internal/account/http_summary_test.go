package account_test

import (
	"encoding/json"
	"testing"
)

func TestSummaryEndpoint(t *testing.T) {
	url, c := newAPI(t)

	mk := func(body string) string {
		resp := do(t, c, "POST", url+"/api/v1/accounts", body)
		if resp.StatusCode != 201 {
			t.Fatalf("create: %d", resp.StatusCode)
		}
		var a struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&a)
		return a.ID
	}
	id1 := mk(`{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	id2 := mk(`{"name":"Кредитка","type":"credit_card","currency":"RUB"}`)
	do(t, c, "PUT", url+"/api/v1/accounts/"+id1+"/balance", `{"as_of":"2026-07-20","amount_minor":100000}`)
	do(t, c, "PUT", url+"/api/v1/accounts/"+id2+"/balance", `{"as_of":"2026-07-20","amount_minor":-25000}`)

	resp := do(t, c, "GET", url+"/api/v1/summary", "")
	if resp.StatusCode != 200 {
		t.Fatalf("summary = %d", resp.StatusCode)
	}
	var sum struct {
		Totals []struct {
			Currency         string `json:"currency"`
			AssetsMinor      int64  `json:"assets_minor"`
			LiabilitiesMinor int64  `json:"liabilities_minor"`
			NetMinor         int64  `json:"net_minor"`
		} `json:"totals"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&sum)
	if len(sum.Totals) != 1 || sum.Totals[0].NetMinor != 75000 ||
		sum.Totals[0].AssetsMinor != 100000 || sum.Totals[0].LiabilitiesMinor != -25000 {
		t.Fatalf("summary = %+v", sum)
	}
}
