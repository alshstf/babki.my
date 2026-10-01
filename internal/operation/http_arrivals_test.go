package operation_test

import (
	"fmt"
	"io"
	"net/http"
	"testing"
)

type arrivalsResp struct {
	Arrivals []struct {
		OperationID       string  `json:"operation_id"`
		OccurredOn        string  `json:"occurred_on"`
		Quantity          string  `json:"quantity"`
		CostMinor         int64   `json:"cost_minor"`
		Source            string  `json:"source"`
		FromAnotherBroker bool    `json:"from_another_broker"`
		FromAccountID     *string `json:"from_account_id"`
		Purchases         []struct {
			Quantity   string  `json:"quantity"`
			CostMinor  int64   `json:"cost_minor"`
			AcquiredOn *string `json:"acquired_on"`
		} `json:"purchases"`
	} `json:"arrivals"`
}

func listArrivals(t *testing.T, c *http.Client, url, accountID, instrumentID string) arrivalsResp {
	t.Helper()
	resp := do(t, c, "GET", url+"/api/v1/accounts/"+accountID+"/instruments/"+instrumentID+"/arrivals", "")
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET arrivals = %d: %s", resp.StatusCode, b)
	}
	var out arrivalsResp
	decodeJSON(t, resp, &out)
	return out
}

// Shares that came from another broker can now be written down by hand on an
// account no importer feeds — with their purchases when the owner has them,
// and without, as bought for nothing, to be priced later.
func TestSharesFromAnotherBrokerCanBeRecordedByHand(t *testing.T) {
	url, c := newAPI(t)
	acc := createID(t, c, url+"/api/v1/accounts", `{"name":"Freedom","type":"brokerage","currency":"USD"}`)
	ko := createID(t, c, url+"/api/v1/instruments", `{"type":"share","name":"Coca-Cola","ticker":"KO","currency":"USD"}`)

	// Without purchases: in at nought.
	resp := do(t, c, "POST", url+"/api/v1/operations/arrivals", fmt.Sprintf(
		`{"account_id":%q,"instrument_id":%q,"occurred_on":"2026-06-15","quantity":"10","currency":"USD","note":"из Т-Банка"}`, acc, ko))
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST arrival = %d: %s", resp.StatusCode, b)
	}
	var bare opResp
	decodeJSON(t, resp, &bare)
	if bare.Type != "transfer_in" || bare.AmountMinor != 0 {
		t.Errorf("arrival = %+v, want a transfer_in with a basis of nought", bare)
	}

	// With purchases: priced from the start, the cost struck by the server.
	resp = do(t, c, "POST", url+"/api/v1/operations/arrivals", fmt.Sprintf(
		`{"account_id":%q,"instrument_id":%q,"occurred_on":"2026-06-20","quantity":"5","currency":"USD",
		  "purchases":[{"quantity":"5","price":"60.10","fee_minor":100,"acquired_on":"2025-02-03"}]}`, acc, ko))
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST arrival with purchases = %d: %s", resp.StatusCode, b)
	}
	var priced opResp
	decodeJSON(t, resp, &priced)
	if priced.AmountMinor != 30_050+100 {
		t.Errorf("amount_minor = %d, want 30150 (5 × 60,10 + 1,00 fee)", priced.AmountMinor)
	}

	// The bare one is given its purchases afterwards, by hand, like an imported one.
	resp = do(t, c, "PUT", url+"/api/v1/operations/"+bare.ID+"/purchases",
		`{"purchases":[{"quantity":"10","price":"55","acquired_on":"2024-08-01"}]}`)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT purchases on a hand-entered arrival = %d: %s", resp.StatusCode, b)
	}

	// Another paper arriving on the same account is that paper's, not this one's.
	pep := createID(t, c, url+"/api/v1/instruments", `{"type":"share","name":"PepsiCo","ticker":"PEP","currency":"USD"}`)
	if resp := do(t, c, "POST", url+"/api/v1/operations/arrivals", fmt.Sprintf(
		`{"account_id":%q,"instrument_id":%q,"occurred_on":"2026-06-15","quantity":"3","currency":"USD"}`, acc, pep)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST another paper's arrival = %d", resp.StatusCode)
	}

	got := listArrivals(t, c, url, acc, ko)
	if len(got.Arrivals) != 2 {
		t.Fatalf("arrivals = %+v, want the two of this paper", got.Arrivals)
	}
	first := got.Arrivals[0]
	if first.OperationID != bare.ID || first.CostMinor != 55_000 || !first.FromAnotherBroker || first.FromAccountID != nil ||
		first.Source != "manual" || len(first.Purchases) != 1 || first.Purchases[0].AcquiredOn == nil || *first.Purchases[0].AcquiredOn != "2024-08-01" {
		t.Errorf("first arrival = %+v, want the bare one, now 10 bought on 2024-08-01 for 55000", first)
	}

	// Deleting it is an ordinary deletion of a hand-entered row.
	if resp := do(t, c, "DELETE", url+"/api/v1/operations/"+priced.ID, ""); resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("DELETE arrival = %d: %s", resp.StatusCode, b)
	}
}

// What cannot be an arrival is refused before anything is written.
func TestAnArrivalThatCannotBeIsRefused(t *testing.T) {
	url, c := newAPI(t)
	acc := createID(t, c, url+"/api/v1/accounts", `{"name":"Freedom","type":"brokerage","currency":"USD"}`)
	ko := createID(t, c, url+"/api/v1/instruments", `{"type":"share","name":"Coca-Cola","ticker":"KO","currency":"USD"}`)
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"no shares":                 {`"quantity":"0","currency":"USD"`, http.StatusBadRequest},
		"a currency that is not":    {`"quantity":"5","currency":"usd"`, http.StatusBadRequest},
		"purchases that do not add": {`"quantity":"5","currency":"USD","purchases":[{"quantity":"4","price":"1"}]}`, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			body := fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"occurred_on":"2026-06-15",%s}`, acc, ko, tc.body)
			if resp := do(t, c, "POST", url+"/api/v1/operations/arrivals", body); resp.StatusCode != tc.want {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d: %s", resp.StatusCode, tc.want, b)
			}
		})
	}
	if got := listArrivals(t, c, url, acc, ko); len(got.Arrivals) != 0 {
		t.Errorf("arrivals after refusals = %+v, want none", got.Arrivals)
	}

	// A paper already held in another currency: the journal will not take it.
	createOperation := do(t, c, "POST", url+"/api/v1/operations", fmt.Sprintf(
		`{"account_id":%q,"instrument_id":%q,"type":"buy","occurred_on":"2026-01-10","quantity":"1","price":"60","currency":"USD"}`, acc, ko))
	if createOperation.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(createOperation.Body)
		t.Fatalf("buy = %d: %s", createOperation.StatusCode, b)
	}
	resp := do(t, c, "POST", url+"/api/v1/operations/arrivals", fmt.Sprintf(
		`{"account_id":%q,"instrument_id":%q,"occurred_on":"2026-06-15","quantity":"5","currency":"EUR",
		  "purchases":[{"quantity":"5","price":"50"}]}`, acc, ko))
	if resp.StatusCode != http.StatusConflict {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("an arrival in EUR of a paper held in USD = %d, want 409: %s", resp.StatusCode, b)
	}
}

// A move between two of the owner's accounts is listed too, naming the account
// it left — where those shares' purchases are to be stated.
func TestArrivalsNameTheAccountAMoveLeft(t *testing.T) {
	url, c := newAPI(t)
	from := createID(t, c, url+"/api/v1/accounts", `{"name":"Т-Банк","type":"brokerage","currency":"RUB"}`)
	to := createID(t, c, url+"/api/v1/accounts", `{"name":"Freedom","type":"brokerage","currency":"RUB"}`)
	sber := createID(t, c, url+"/api/v1/instruments", `{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)
	if resp := do(t, c, "POST", url+"/api/v1/operations", fmt.Sprintf(
		`{"account_id":%q,"instrument_id":%q,"type":"buy","occurred_on":"2026-01-10","quantity":"10","price":"300","currency":"RUB"}`,
		from, sber)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("buy = %d", resp.StatusCode)
	}
	if resp := do(t, c, "POST", url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"10","occurred_on":"2026-06-01"}`,
		from, to, sber)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("transfer = %d", resp.StatusCode)
	}

	got := listArrivals(t, c, url, to, sber)
	if len(got.Arrivals) != 1 {
		t.Fatalf("arrivals = %+v, want one", got.Arrivals)
	}
	a := got.Arrivals[0]
	if a.FromAnotherBroker || a.FromAccountID == nil || *a.FromAccountID != from {
		t.Errorf("arrival = %+v, want a move from %s", a, from)
	}
	if a.CostMinor != 300_000 || len(a.Purchases) != 1 || a.Purchases[0].AcquiredOn == nil || *a.Purchases[0].AcquiredOn != "2026-01-10" {
		t.Errorf("arrival = %+v, want the purchase it carried from the source", a)
	}
	// And nothing of this paper arrived on the source account.
	if got := listArrivals(t, c, url, from, sber); len(got.Arrivals) != 0 {
		t.Errorf("arrivals on the source = %+v, want none", got.Arrivals)
	}
}
