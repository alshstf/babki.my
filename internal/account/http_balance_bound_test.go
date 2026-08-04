package account_test

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"testing"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/platform/money"
)

// PUT /accounts/{id}/balance bounded nothing at all (#89). Every other write of
// money in this program does — an operation's amount_minor, its fee, the
// product of its price and its quantity — and a balance is the same money on
// the same screen: it is summed with the balances of every other account in its
// currency and multiplied by an fx rate before anything is published.
//
// Caught at the READ, an over-large balance is a screen that answers 500 for as
// long as the row exists (money.ErrOverflow out of balanceInBase, see
// http_overflow_test.go). Caught at the WRITE it is a rejected field. Both
// stay: a rate is unbounded from above and arrives after the fact, so a balance
// that fitted when it was written can stop fitting later.

// The bound as a literal, spelled out rather than derived from
// money.MaxAmountMinor. A test that computed it the way the code does would
// agree with a wrong bound just as readily as with the right one — the same
// reason operation's bounds are spelled out in service_bounds_test.go.
const (
	balanceBound      = 1_000_000_000_000_000 // 10^15 minor units, 10^13 whole roubles
	balanceBoundDigit = "1000000000000000"
)

// wantBalanceRefusal asserts the WHOLE refusal, not a fragment of it. The
// number and the field name are the two things an importer acts on, and a
// message naming another field's bound sends them to fix the wrong column;
// since the digits of this bound are a substring of any longer number, a
// substring check could not tell such a message apart from the right one.
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

// newBoundedAccount creates one account and returns the API root, the client
// and the account id, so each test below starts from the same fixture.
func newBoundedAccount(t *testing.T) (string, *http.Client, string) {
	t.Helper()
	url, c := newAPI(t)
	return url, c, mkAccount(t, url, c, "Брокерский", "RUB")
}

// putBalance sends one balance mark and hands back the response. Unlike
// setBalance next door it does not insist on a 200: every test here is about
// which balances are taken and which are refused.
func putBalance(t *testing.T, url string, c *http.Client, id string, amountMinor int64) *http.Response {
	t.Helper()
	return do(t, c, "PUT", url+"/api/v1/accounts/"+id+"/balance",
		fmt.Sprintf(`{"as_of":"2026-07-20","amount_minor":%d}`, amountMinor))
}

// TestBalanceBeyondTheBoundIsRefusedAtTheWrite covers the shape this actually
// arrives in: a figure typed or imported in the wrong unit — kopecks where
// roubles were meant, or a column scaled by a hundred once too often.
func TestBalanceBeyondTheBoundIsRefusedAtTheWrite(t *testing.T) {
	url, c, id := newBoundedAccount(t)

	// One minor unit past the bound, which is where the bound actually is. A
	// wildly wrong figure would be refused by any cap loose enough to be no cap
	// at all; this fixes the edge, and together with the acceptance test below
	// it leaves exactly one balance that is the largest allowed.
	wantBalanceRefusal(t, putBalance(t, url, c, id, balanceBound+1), balanceBound+1)

	// And the two figures that break the arithmetic outright: math.MaxInt64
	// doubles past int64 at any rate above one, and math.MinInt64 cannot even be
	// negated. Both were accepted before this bound existed.
	wantBalanceRefusal(t, putBalance(t, url, c, id, math.MaxInt64), int64(math.MaxInt64))
	wantBalanceRefusal(t, putBalance(t, url, c, id, math.MinInt64), int64(math.MinInt64))
}

// TestBalanceBeyondTheBoundIsRefusedAsADebtToo: a credit card and a loan carry
// their debt as a NEGATIVE balance, so the negative side of the bound is a real
// door and not symmetry for its own sake.
func TestBalanceBeyondTheBoundIsRefusedAsADebtToo(t *testing.T) {
	url, c, id := newBoundedAccount(t)

	wantBalanceRefusal(t, putBalance(t, url, c, id, -balanceBound-1), -balanceBound-1)
}

// TestBalanceExactlyAtTheBoundIsAccepted is the other side of the same guard. A
// bound that refuses the value ON it withholds a figure that is perfectly
// representable and says nothing about why — the same class of harm as
// accepting one that wraps.
//
// The second half is what makes this number the right one rather than merely a
// number, and it is the same promise the quantity bound carries next door (see
// operation.TestQuantityExactlyAtTheBoundIsAccepted): what the write accepts,
// the accounts screen must still be able to publish. Both halves live in one
// test on purpose — as two, one could be moved and the other left to disagree
// with it.
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

	// And the screen can still convert it. This is money.Minor(balance × rate),
	// the arithmetic of balanceInBase, at the largest whole fx rate that fits:
	// math.MaxInt64 / 10^15 ≈ 9223, which is above any rate this program will
	// meet (the dearest currency against the cheapest is nowhere near it).
	const largestOrdinaryRate = 9223
	if _, err := money.Minor(decimal.NewFromInt(balanceBound).Mul(decimal.NewFromInt(largestOrdinaryRate))); err != nil {
		t.Errorf("the largest accepted balance at a rate of %d: %v — a bound that admits a balance the screen cannot convert is not a bound",
			largestOrdinaryRate, err)
	}
	// One unit of rate more does not fit, which is what makes 9223 the edge
	// rather than a comfortable round number.
	if _, err := money.Minor(decimal.NewFromInt(balanceBound).Mul(decimal.NewFromInt(largestOrdinaryRate + 1))); err == nil {
		t.Errorf("rate of %d at the bound converted cleanly, so the margin is wider than this test claims",
			largestOrdinaryRate+1)
	}
}

// TestOrdinaryBalancesAreUntouched: the bound is ten trillion whole roubles,
// and the point of choosing it there is that no real balance comes near it.
func TestOrdinaryBalancesAreUntouched(t *testing.T) {
	url, c, id := newBoundedAccount(t)

	for _, amount := range []int64{0, 150_000_00, -45_000_00, 1_000_000_000_00} {
		if resp := putBalance(t, url, c, id, amount); resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("balance of %d = %d, want 200: %s", amount, resp.StatusCode, b)
		}
	}
}
