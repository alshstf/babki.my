package account_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/marketdata"
)

// fakeJournals stands in for the portfolio engine: what each account is worth
// by its journal. An account it has no entry for has no operations.
type fakeJournals struct {
	byAccount map[string]account.JournalValue
}

func (f *fakeJournals) ValueFromJournal(_ context.Context, _, accountID uuid.UUID) (account.JournalValue, error) {
	return f.byAccount[accountID.String()], nil
}

type journalRow struct {
	ID              string `json:"id"`
	ValuedByBalance bool   `json:"valued_by_balance"`
	CountedBy       string `json:"counted_by"`
	Journal         *struct {
		AmountMinor       int64    `json:"amount_minor"`
		Currency          string   `json:"currency"`
		UnpricedPositions int      `json:"unpriced_positions"`
		MissingRates      []string `json:"missing_rates"`
		NegativeCash      []string `json:"negative_cash"`
		Reconciliation    *struct {
			Status             string `json:"status"`
			BalanceAsOf        string `json:"balance_as_of"`
			BalanceInBaseMinor int64  `json:"balance_in_base_minor"`
			DifferenceMinor    int64  `json:"difference_minor"`
		} `json:"reconciliation"`
	} `json:"journal"`
}

type journalSummary struct {
	summaryResponse
	Journal struct {
		Accounts                 int   `json:"accounts"`
		Differing                int   `json:"differing"`
		DifferingDifferenceMinor int64 `json:"differing_difference_minor"`
		PinnedToBalance          int   `json:"pinned_to_balance"`
		UnpricedPositions        int   `json:"unpriced_positions"`
	} `json:"journal"`
}

func getJSON(t *testing.T, c *http.Client, url string, out any) {
	t.Helper()
	resp := do(t, c, "GET", url, "")
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s = %d: %s", url, resp.StatusCode, b)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

func createAccount(t *testing.T, url string, c *http.Client, name, typ, currency string) string {
	t.Helper()
	resp := do(t, c, "POST", url+"/api/v1/accounts",
		fmt.Sprintf(`{"name":%q,"type":%q,"currency":%q}`, name, typ, currency))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create %s: %d", name, resp.StatusCode)
	}
	var a struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&a)
	return a.ID
}

func balanceOn(t *testing.T, url string, c *http.Client, id string, on time.Time, minor int64) {
	t.Helper()
	resp := do(t, c, "PUT", url+"/api/v1/accounts/"+id+"/balance",
		fmt.Sprintf(`{"as_of":%q,"amount_minor":%d}`, on.Format("2006-01-02"), minor))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set balance: %d", resp.StatusCode)
	}
}

func rowOf(t *testing.T, rows []journalRow, id string) journalRow {
	t.Helper()
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("account %s not in the list", id)
	return journalRow{}
}

// The family from the Р-2 memo: one broker whose journal agrees with the
// broker's own figure, one whose journal is missing operations, a deposit and a
// credit card on their balances. A brokerage account counts by its journal
// unless the family pins it to its balance; the total says what it owes to
// journals and how far the disagreeing one is off.
func TestTheTotalCountsABrokerageAccountByItsJournal(t *testing.T) {
	journals := &fakeJournals{byAccount: map[string]account.JournalValue{}}
	url, c, _ := newAPIWithJournals(t, journals)
	today := time.Now().UTC()

	tinv := createAccount(t, url, c, "Т-Инвестиции", "brokerage", "RUB")
	alfa := createAccount(t, url, c, "Альфа", "brokerage", "RUB")
	empty := createAccount(t, url, c, "Новый брокер", "brokerage", "RUB")
	deposit := createAccount(t, url, c, "Вклад", "deposit", "RUB")
	card := createAccount(t, url, c, "Кредитка", "credit_card", "RUB")
	gone := createAccount(t, url, c, "Закрытый брокер", "brokerage", "RUB")

	balanceOn(t, url, c, tinv, today, 86_021_000)
	balanceOn(t, url, c, alfa, today, 54_000_000)
	balanceOn(t, url, c, empty, today, 10_000)
	balanceOn(t, url, c, deposit, today, 50_000_000)
	balanceOn(t, url, c, card, today, -3_000_000)
	journals.byAccount[tinv] = account.JournalValue{
		Currency: "RUB", Minor: 86_000_000, ByCurrency: map[string]int64{"RUB": 86_000_000},
		Operations: 40, Unpriced: 1, MissingRates: []string{}, NegativeCash: []string{},
	}
	journals.byAccount[alfa] = account.JournalValue{
		Currency: "RUB", Minor: 19_500_000, ByCurrency: map[string]int64{"RUB": 19_500_000},
		Operations: 12, NegativeCash: []string{"RUB"},
	}
	journals.byAccount[gone] = account.JournalValue{
		Currency: "RUB", Minor: 1_000_000, ByCurrency: map[string]int64{"RUB": 1_000_000}, Operations: 3,
	}
	if resp := do(t, c, "DELETE", url+"/api/v1/accounts/"+gone, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("archive: %d", resp.StatusCode)
	}

	var sum journalSummary
	getJSON(t, c, url+"/api/v1/summary", &sum)
	// 860 000 + 195 000 by journals, 100 + 500 000 − 30 000 by balances.
	if sum.TotalInBaseMinor == nil || *sum.TotalInBaseMinor != 152_510_000 {
		t.Errorf("total = %v, want 152510000", sum.TotalInBaseMinor)
	}
	if j := sum.Journal; j.Accounts != 2 || j.Differing != 1 || j.DifferingDifferenceMinor != -34_500_000 ||
		j.PinnedToBalance != 0 || j.UnpricedPositions != 1 {
		t.Errorf("journal = %+v, want 2 accounts, Альфа off by −345 000, one unpriced", j)
	}
	if len(sum.Totals) != 1 || sum.Totals[0].AssetsMinor != 155_510_000 || sum.Totals[0].LiabilitiesMinor != -3_000_000 {
		t.Errorf("totals = %+v, want assets 1 555 100, debts −30 000", sum.Totals)
	}

	var rows []journalRow
	getJSON(t, c, url+"/api/v1/accounts", &rows)
	if r := rowOf(t, rows, tinv); r.CountedBy != "journal" || r.Journal == nil || r.Journal.AmountMinor != 86_000_000 ||
		r.Journal.Reconciliation == nil || r.Journal.Reconciliation.Status != "agrees" ||
		r.Journal.Reconciliation.DifferenceMinor != -21_000 || r.Journal.Reconciliation.BalanceInBaseMinor != 86_021_000 {
		t.Errorf("Т-Инвестиции = %+v, want counted by a journal agreeing with the balance, 210 ₽ apart", r)
	}
	if r := rowOf(t, rows, alfa); r.CountedBy != "journal" || r.Journal == nil ||
		r.Journal.Reconciliation == nil || r.Journal.Reconciliation.Status != "differs" ||
		len(r.Journal.NegativeCash) != 1 || r.Journal.MissingRates == nil {
		t.Errorf("Альфа = %+v, want counted by a journal that differs, cash below zero named", r)
	}
	for _, id := range []string{empty, deposit, card, gone} {
		if r := rowOf(t, rows, id); r.CountedBy != "balance" || r.Journal != nil {
			t.Errorf("account %s = %+v, want counted by its balance with no journal", id, r)
		}
	}

	// Pinned, Альфа is counted by its balance and still says what its journal
	// comes to.
	resp := do(t, c, "PATCH", url+"/api/v1/accounts/"+alfa, `{"valued_by_balance":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pin = %d", resp.StatusCode)
	}
	var pinned journalRow
	_ = json.NewDecoder(resp.Body).Decode(&pinned)
	if !pinned.ValuedByBalance || pinned.CountedBy != "balance" || pinned.Journal == nil || pinned.Journal.AmountMinor != 19_500_000 {
		t.Errorf("pinned = %+v, want counted by its balance with the journal beside it", pinned)
	}
	getJSON(t, c, url+"/api/v1/summary", &sum)
	if sum.TotalInBaseMinor == nil || *sum.TotalInBaseMinor != 187_010_000 {
		t.Errorf("total once pinned = %v, want 187010000", sum.TotalInBaseMinor)
	}
	if j := sum.Journal; j.Accounts != 1 || j.Differing != 0 || j.DifferingDifferenceMinor != 0 || j.PinnedToBalance != 1 {
		t.Errorf("journal once pinned = %+v, want 1 account by journal, 1 pinned", j)
	}

	// A PATCH that does not name the flag leaves it alone.
	resp = do(t, c, "PATCH", url+"/api/v1/accounts/"+alfa, `{"name":"Альфа-Инвестиции"}`)
	var renamed journalRow
	_ = json.NewDecoder(resp.Body).Decode(&renamed)
	if !renamed.ValuedByBalance {
		t.Errorf("renamed = %+v, want still valued by its balance", renamed)
	}
}

// A journal holding several currencies is totalled under each, a currency the
// account is short of among the debts, and converted with the rest; a balance
// in a foreign currency is reconciled at today's rate.
func TestAJournalInSeveralCurrenciesIsTotalledUnderEach(t *testing.T) {
	journals := &fakeJournals{byAccount: map[string]account.JournalValue{}}
	url, c, md := newAPIWithJournals(t, journals)
	today := time.Now().UTC()
	if err := md.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: today, Rate: decimal.RequireFromString("90"), Source: "test"},
		{Base: "EUR", Quote: "RUB", On: today, Rate: decimal.RequireFromString("100"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed rates: %v", err)
	}

	broker := createAccount(t, url, c, "Freedom", "brokerage", "USD")
	balanceOn(t, url, c, broker, today, 94_000)
	// 1 000 $ × 90 − 50 € × 100 + 200 ₽ = 85 200 ₽.
	journals.byAccount[broker] = account.JournalValue{
		Currency: "RUB", Minor: 8_520_000,
		ByCurrency: map[string]int64{"USD": 100_000, "EUR": -5_000, "RUB": 20_000},
		Operations: 4, NegativeCash: []string{"EUR"},
	}

	var sum journalSummary
	getJSON(t, c, url+"/api/v1/summary", &sum)
	if sum.TotalInBaseMinor == nil || *sum.TotalInBaseMinor != 8_520_000 {
		t.Errorf("total = %v, want 8520000", sum.TotalInBaseMinor)
	}
	want := map[string][2]int64{"EUR": {0, -5_000}, "RUB": {20_000, 0}, "USD": {100_000, 0}}
	if len(sum.Totals) != len(want) {
		t.Fatalf("totals = %+v, want EUR, RUB and USD", sum.Totals)
	}
	for _, tot := range sum.Totals {
		if w := want[tot.Currency]; tot.AssetsMinor != w[0] || tot.LiabilitiesMinor != w[1] {
			t.Errorf("%s = %+v, want assets %d, debts %d", tot.Currency, tot, w[0], w[1])
		}
	}

	var rows []journalRow
	getJSON(t, c, url+"/api/v1/accounts", &rows)
	// 940 $ × 90 = 84 600 ₽ against 85 200 ₽: 0.7% apart.
	r := rowOf(t, rows, broker)
	if r.Journal == nil || r.Journal.Reconciliation == nil || r.Journal.Reconciliation.Status != "agrees" ||
		r.Journal.Reconciliation.BalanceInBaseMinor != 8_460_000 || r.Journal.Reconciliation.DifferenceMinor != 60_000 {
		t.Errorf("Freedom = %+v, want the dollar balance put into rubles and agreeing", r)
	}
}
