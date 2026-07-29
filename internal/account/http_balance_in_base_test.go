package account_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
)

// moneyInBase mirrors apitypes.MoneyInBase for decoding in tests.
type moneyInBase struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	RateOn      string `json:"rate_on"`
}

// accountListItem is the subset of apitypes.AccountWithBalance these tests
// care about: the account's own currency/balance plus balance_in_base. A nil
// *moneyInBase covers both an omitted key and an explicit JSON null — which
// is exactly what balance_in_base always is here (see the handler's
// balanceInBase: it is always explicitly set to either a value or null,
// never left unset).
type accountListItem struct {
	ID       string `json:"id"`
	Currency string `json:"currency"`
	Balance  *struct {
		AmountMinor int64  `json:"amount_minor"`
		AsOf        string `json:"as_of"`
	} `json:"balance"`
	BalanceInBase *moneyInBase `json:"balance_in_base"`
}

// listAccounts fetches GET /api/v1/accounts and decodes it into
// accountListItem, failing the test on a non-200 or a decode error.
func listAccounts(t *testing.T, url string, c *http.Client) []accountListItem {
	t.Helper()
	resp := do(t, c, "GET", url+"/api/v1/accounts", "")
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
	resp := do(t, c, "POST", url+"/api/v1/accounts",
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
	resp := do(t, c, "PUT", url+"/api/v1/accounts/"+accountID+"/balance",
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

// TestListBalanceInBaseConvertsNonBaseCurrency covers the brief's main case:
// an account in a non-base currency with a resolvable fx rate must get
// balance_in_base filled in, converted using today's rate, with currency
// equal to the space's base currency (RUB, the default) and rate_on equal
// to the seeded rate's own date.
//
// A second USD account (different balance) is included to exercise the
// per-request rate memoization path (see the handler's balanceInBase/
// rateLookup doc): both accounts share the same currency, so the handler
// must resolve the USD->RUB rate once and apply it to each account's own
// balance — not reuse one account's converted amount for the other's. If the
// cache mixed up accounts (e.g. cached the converted minor amount instead of
// the rate, or applied the wrong entry), one of these two conversions would
// come out wrong.
//
// Manual arithmetic (rate matches converter_test.go's fixtures):
//
//	account 1: 123.45 USD (amount_minor 12345) * 90 RUB/USD = 11110.50 RUB (1111050 minor)
//	account 2:  50.00 USD (amount_minor  5000) * 90 RUB/USD =  4500.00 RUB ( 450000 minor)
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

// TestListBalanceInBaseNullWhenAlreadyBaseCurrency covers: an account
// already denominated in the space's base currency (RUB, the default) has
// nothing to convert, so balance_in_base must be null even though the
// account has a balance.
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

// TestListBalanceInBaseNullWhenNoRate covers: an account in a non-base
// currency with NO resolvable fx rate must still come back with
// balance_in_base = null — and, crucially, the request as a whole must
// still succeed (200), not fail just because one account's currency lacks a
// rate. This mirrors ConvertMany's "missing" handling in handleSummary and
// toAPI's marketdata.ErrNoRate handling in portfolio/http.go.
func TestListBalanceInBaseNullWhenNoRate(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	on := pastOn()

	// Only USD has a rate; the account below is in GBP, which has none at
	// all (no direct, inverse, or RUB-bridge leg).
	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rates: %v", err)
	}

	id := mkAccount(t, url, c, "GBP cash", "GBP")
	setBalance(t, url, c, id, 10000)

	resp := do(t, c, "GET", url+"/api/v1/accounts", "")
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

// TestListBalanceInBaseNullWhenNoBalance covers: an account with no balance
// recorded at all (never PUT .../balance) must have balance_in_base = null
// — there's nothing to convert — even in a non-base currency with a
// resolvable rate.
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
