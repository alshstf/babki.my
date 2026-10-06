package account_test

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"testing"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/money"
)

// PUT /accounts/{id}/balance is bounded like every other money write (#89):
// unbounded, a balance made the accounts screen fail. The read-side guard stays
// too, since rates can grow a figure later.

// The bound as a literal, so a wrong derivation would not agree with it.
const (
	balanceBound      = 1_000_000_000_000_000 // 10^15 minor units, 10^13 whole roubles
	balanceBoundDigit = "1000000000000000"
)

// wantBalanceRefusal asserts the whole message: the digits of the bound are a
// substring of longer numbers.
func wantBalanceRefusal(t *testing.T, resp *http.Response, amountMinor any) {
	t.Helper()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PUT balance of %v = %d, want 400: %s", amountMinor, resp.StatusCode, body)
	}
	var got struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode refusal %q: %v", body, err)
	}
	const want = "amount_minor must be within ±" + balanceBoundDigit
	if got.Error != want {
		t.Errorf("refusal = %q, want exactly %q", got.Error, want)
	}
}

// newBoundedAccount creates one account and returns the URL, client and id.
func newBoundedAccount(t *testing.T) (string, *http.Client, string) {
	t.Helper()
	url, c := newAPI(t)
	return url, c, mkAccount(t, url, c, "Брокерский", "RUB")
}

// putBalance sends one balance mark and returns the response, whatever it is.
func putBalance(t *testing.T, url string, c *http.Client, id string, amountMinor int64) *http.Response {
	t.Helper()
	return apitest.Do(t, c, "PUT", url+"/api/v1/accounts/"+id+"/balance",
		fmt.Sprintf(`{"as_of":"2026-07-20","amount_minor":%d}`, amountMinor))
}

// A balance in the wrong unit (kopecks for roubles) is refused at the write.
func TestBalanceBeyondTheBoundIsRefusedAtTheWrite(t *testing.T) {
	url, c, id := newBoundedAccount(t)

	// One unit past the bound, where the bound is.
	wantBalanceRefusal(t, putBalance(t, url, c, id, balanceBound+1), balanceBound+1)

	// math.MaxInt64 and math.MinInt64, which break the arithmetic outright.
	wantBalanceRefusal(t, putBalance(t, url, c, id, math.MaxInt64), int64(math.MaxInt64))
	wantBalanceRefusal(t, putBalance(t, url, c, id, math.MinInt64), int64(math.MinInt64))
}

// A debt is a negative balance, so the negative side is a real door.
func TestBalanceBeyondTheBoundIsRefusedAsADebtToo(t *testing.T) {
	url, c, id := newBoundedAccount(t)

	wantBalanceRefusal(t, putBalance(t, url, c, id, -balanceBound-1), -balanceBound-1)
}

// The bound itself is accepted, and the screen can still publish it.
func TestBalanceExactlyAtTheBoundIsAccepted(t *testing.T) {
	url, c, id := newBoundedAccount(t)

	for _, amount := range []int64{balanceBound, -balanceBound} {
		resp := putBalance(t, url, c, id, amount)
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("balance of exactly %d = %d, want 200 — the value ON the bound is inside it: %s",
				amount, resp.StatusCode, body)
		}
		var acc struct {
			Balance *struct {
				AmountMinor int64 `json:"amount_minor"`
			} `json:"balance"`
		}
		if err := json.Unmarshal(body, &acc); err != nil {
			t.Fatalf("decode account: %v", err)
		}
		if acc.Balance == nil || acc.Balance.AmountMinor != amount {
			t.Errorf("stored balance = %+v, want %d", acc.Balance, amount)
		}
	}

	// balanceInBase's arithmetic at the largest whole rate that fits, 9223
	// (MaxInt64 / 10^15) — above any real rate.
	const largestOrdinaryRate = 9223
	if _, err := money.Minor(decimal.NewFromInt(balanceBound).Mul(decimal.NewFromInt(largestOrdinaryRate))); err != nil {
		t.Errorf("the largest accepted balance at a rate of %d: %v — a bound that admits a balance the screen cannot convert is not a bound",
			largestOrdinaryRate, err)
	}
	// One more unit of rate does not fit.
	if _, err := money.Minor(decimal.NewFromInt(balanceBound).Mul(decimal.NewFromInt(largestOrdinaryRate + 1))); err == nil {
		t.Errorf("rate of %d at the bound converted cleanly, so the margin is wider than this test claims",
			largestOrdinaryRate+1)
	}
}

// Ordinary balances are untouched.
func TestOrdinaryBalancesAreUntouched(t *testing.T) {
	url, c, id := newBoundedAccount(t)

	for _, amount := range []int64{0, 150_000_00, -45_000_00, 1_000_000_000_00} {
		if resp := putBalance(t, url, c, id, amount); resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("balance of %d = %d, want 200: %s", amount, resp.StatusCode, b)
		}
	}
}
