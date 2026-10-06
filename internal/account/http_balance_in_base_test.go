package account_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitest"
)

// moneyInBase mirrors apitypes.MoneyInBase for decoding in tests.
type moneyInBase struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	RateOn      string `json:"rate_on"`
}

// accountListItem is the part of an account these tests read; balance_in_base
// is always set, to a value or null.
type accountListItem struct {
	ID       string `json:"id"`
	Currency string `json:"currency"`
	Balance  *struct {
		AmountMinor int64  `json:"amount_minor"`
		AsOf        string `json:"as_of"`
	} `json:"balance"`
	BalanceInBase *moneyInBase `json:"balance_in_base"`
}

// listAccounts fetches and decodes GET /api/v1/accounts.
func listAccounts(t *testing.T, url string, c *http.Client) []accountListItem {
	t.Helper()
	resp := apitest.Do(t, c, "GET", url+"/api/v1/accounts", "")
	if resp.StatusCode != 200 {
		t.Fatalf("list accounts = %d", resp.StatusCode)
	}
	var out []accountListItem
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode account list: %v", err)
	}
	return out
}

// mkAccount creates a cash account in currency and returns its id.
func mkAccount(t *testing.T, url string, c *http.Client, name, currency string) string {
	t.Helper()
	resp := apitest.Do(t, c, "POST", url+"/api/v1/accounts",
		`{"name":"`+name+`","type":"cash","currency":"`+currency+`"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create %s account: %d", currency, resp.StatusCode)
	}
	var a struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&a)
	return a.ID
}

// setBalance sets an account's balance via PUT .../balance.
func setBalance(t *testing.T, url string, c *http.Client, accountID string, amountMinor int64) {
	t.Helper()
	resp := apitest.Do(t, c, "PUT", url+"/api/v1/accounts/"+accountID+"/balance",
		`{"as_of":"2026-07-20","amount_minor":`+decimal.NewFromInt(amountMinor).String()+`}`)
	if resp.StatusCode != 200 {
		t.Fatalf("set balance: %d", resp.StatusCode)
	}
}

func findAccount(t *testing.T, list []accountListItem, id string) accountListItem {
	t.Helper()
	for _, a := range list {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("account %s not found in list", id)
	return accountListItem{}
}

// A non-base account with a rate gets balance_in_base at today's rate, in the
// base currency, dated by the rate. Two USD accounts check the memo stores the
// rate, not one account's result:
//
//	12345 × 90 = 1111050
//	 5000 × 90 =  450000
func TestListBalanceInBaseConvertsNonBaseCurrency(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	on := pastOn()

	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rates: %v", err)
	}

	id1 := mkAccount(t, url, c, "US cash 1", "USD")
	setBalance(t, url, c, id1, 12345)
	id2 := mkAccount(t, url, c, "US cash 2", "USD")
	setBalance(t, url, c, id2, 5000)

	list := listAccounts(t, url, c)

	a1 := findAccount(t, list, id1)
	if a1.BalanceInBase == nil {
		t.Fatalf("account 1 balance_in_base = nil, want a converted value")
	}
	if a1.BalanceInBase.AmountMinor != 1111050 {
		t.Errorf("account 1 balance_in_base.amount_minor = %d, want 1111050", a1.BalanceInBase.AmountMinor)
	}
	if a1.BalanceInBase.Currency != "RUB" {
		t.Errorf("account 1 balance_in_base.currency = %q, want RUB", a1.BalanceInBase.Currency)
	}
	wantRateOn := on.Format("2006-01-02")
	if a1.BalanceInBase.RateOn != wantRateOn {
		t.Errorf("account 1 balance_in_base.rate_on = %q, want %q", a1.BalanceInBase.RateOn, wantRateOn)
	}

	a2 := findAccount(t, list, id2)
	if a2.BalanceInBase == nil {
		t.Fatalf("account 2 balance_in_base = nil, want a converted value")
	}
	if a2.BalanceInBase.AmountMinor != 450000 {
		t.Errorf("account 2 balance_in_base.amount_minor = %d, want 450000 (memoized rate must still apply per-account amounts correctly)", a2.BalanceInBase.AmountMinor)
	}
	if a2.BalanceInBase.Currency != "RUB" {
		t.Errorf("account 2 balance_in_base.currency = %q, want RUB", a2.BalanceInBase.Currency)
	}
	if a2.BalanceInBase.RateOn != wantRateOn {
		t.Errorf("account 2 balance_in_base.rate_on = %q, want %q", a2.BalanceInBase.RateOn, wantRateOn)
	}
}

// An account already in the base currency has a null balance_in_base.
func TestListBalanceInBaseNullWhenAlreadyBaseCurrency(t *testing.T) {
	url, c := newAPI(t)

	id := mkAccount(t, url, c, "RUB cash", "RUB")
	setBalance(t, url, c, id, 100000)

	list := listAccounts(t, url, c)
	a := findAccount(t, list, id)
	if a.BalanceInBase != nil {
		t.Fatalf("balance_in_base = %+v, want null (account already in base currency)", a.BalanceInBase)
	}
}

// An account with no rate has a null balance_in_base, and the request still
// succeeds.
func TestListBalanceInBaseNullWhenNoRate(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	on := pastOn()

	// Only USD has a rate; GBP has none.
	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rates: %v", err)
	}

	id := mkAccount(t, url, c, "GBP cash", "GBP")
	setBalance(t, url, c, id, 10000)

	resp := apitest.Do(t, c, "GET", url+"/api/v1/accounts", "")
	if resp.StatusCode != 200 {
		t.Fatalf("list accounts = %d, want 200 (missing rate must not fail the request)", resp.StatusCode)
	}
	var list []accountListItem
	_ = json.NewDecoder(resp.Body).Decode(&list)

	a := findAccount(t, list, id)
	if a.BalanceInBase != nil {
		t.Fatalf("balance_in_base = %+v, want null (no fx rate for GBP->RUB)", a.BalanceInBase)
	}
}

// An account with no balance has a null balance_in_base.
func TestListBalanceInBaseNullWhenNoBalance(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	on := pastOn()

	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rates: %v", err)
	}

	id := mkAccount(t, url, c, "US cash, no balance", "USD")

	list := listAccounts(t, url, c)
	a := findAccount(t, list, id)
	if a.Balance != nil {
		t.Fatalf("balance = %+v, want nil (never set)", a.Balance)
	}
	if a.BalanceInBase != nil {
		t.Fatalf("balance_in_base = %+v, want null (no balance to convert)", a.BalanceInBase)
	}
}
