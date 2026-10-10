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
	"babki.my/babki/internal/platform/apitest"
)

// fakeJournals stands in for the portfolio engine: each account's journal worth
// today and on past days; no entry means no operations.
type fakeJournals struct {
	byAccount map[string]account.JournalValue
	onDay     map[string]account.JournalValue // account id + "@" + day
	periods   map[string]account.ReturnBasis
}

func (f *fakeJournals) ReturnBasis(_ context.Context, _, accountID uuid.UUID, _, _ time.Time) (account.ReturnBasis, error) {
	return f.periods[accountID.String()], nil
}

func (f *fakeJournals) ValueFromJournal(_ context.Context, _, accountID uuid.UUID) (account.JournalValue, error) {
	return f.byAccount[accountID.String()], nil
}

func (f *fakeJournals) ValueOn(_ context.Context, _, accountID uuid.UUID, day time.Time) (account.JournalValue, error) {
	return f.onDay[accountID.String()+"@"+day.Format(time.DateOnly)], nil
}

func (f *fakeJournals) ValuesOn(ctx context.Context, spaceID, accountID uuid.UUID, days []time.Time) ([]account.JournalValue, error) {
	out := make([]account.JournalValue, len(days))
	for i, day := range days {
		out[i], _ = f.ValueOn(ctx, spaceID, accountID, day)
	}
	return out, nil
}

type journalRow struct {
	ID              string `json:"id"`
	ValuedByBalance bool   `json:"valued_by_balance"`
	CountedBy       string `json:"counted_by"`
	Journal         *struct {
		AmountMinor        int64    `json:"amount_minor"`
		FullAmountMinor    int64    `json:"full_amount_minor"`
		Currency           string   `json:"currency"`
		UnpricedPositions  int      `json:"unpriced_positions"`
		NotTradedPositions int      `json:"not_traded_positions"`
		MissingRates       []string `json:"missing_rates"`
		NegativeCash       []string `json:"negative_cash"`
		Reconciliation     *struct {
			Status             string `json:"status"`
			BalanceAsOf        string `json:"balance_as_of"`
			ComparedOn         string `json:"compared_on"`
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
		NotTradedPositions       int   `json:"not_traded_positions"`
		FullDifferenceMinor      int64 `json:"full_difference_minor"`
	} `json:"journal"`
}

func getJSON(t *testing.T, c *http.Client, url string, out any) {
	t.Helper()
	resp := apitest.Do(t, c, "GET", url, "")
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
	resp := apitest.Do(t, c, "POST", url+"/api/v1/accounts",
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
	resp := apitest.Do(t, c, "PUT", url+"/api/v1/accounts/"+id+"/balance",
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

// The Р-2 family: a broker whose journal agrees, one missing operations, a
// deposit and a card. Brokerage accounts count by journal unless pinned, and
// the total says what it owes to journals.
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
	// A frozen fund the liquid worth counts as nothing and the full one at
	// 50 000 ₽ (decision Р-11).
	journals.byAccount[tinv] = account.JournalValue{
		Currency: "RUB", Minor: 86_000_000, ByCurrency: map[string]int64{"RUB": 86_000_000},
		Operations: 40, Unpriced: 1, NotTraded: 1, MissingRates: []string{}, NegativeCash: []string{},
		FullMinor: 91_000_000,
	}
	journals.byAccount[alfa] = account.JournalValue{
		Currency: "RUB", Minor: 19_500_000, ByCurrency: map[string]int64{"RUB": 19_500_000},
		Operations: 12, NegativeCash: []string{"RUB"}, FullMinor: 19_500_000,
	}
	journals.byAccount[gone] = account.JournalValue{
		Currency: "RUB", Minor: 1_000_000, ByCurrency: map[string]int64{"RUB": 1_000_000}, Operations: 3,
	}
	if resp := apitest.Do(t, c, "DELETE", url+"/api/v1/accounts/"+gone, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("archive: %d", resp.StatusCode)
	}

	var sum journalSummary
	getJSON(t, c, url+"/api/v1/summary", &sum)
	// 860 000 + 195 000 by journals, 100 + 500 000 − 30 000 by balances.
	if sum.TotalInBaseMinor == nil || *sum.TotalInBaseMinor != 152_510_000 {
		t.Errorf("total = %v, want 152510000", sum.TotalInBaseMinor)
	}
	if j := sum.Journal; j.Accounts != 2 || j.Differing != 1 || j.DifferingDifferenceMinor != -34_500_000 ||
		j.PinnedToBalance != 0 || j.UnpricedPositions != 1 || j.NotTradedPositions != 1 || j.FullDifferenceMinor != 5_000_000 {
		t.Errorf("journal = %+v, want 2 accounts, Альфа off by −345 000, one unpriced and not traded, 50 000 more in full", j)
	}
	if len(sum.Totals) != 1 || sum.Totals[0].AssetsMinor != 155_510_000 || sum.Totals[0].LiabilitiesMinor != -3_000_000 {
		t.Errorf("totals = %+v, want assets 1 555 100, debts −30 000", sum.Totals)
	}

	var rows []journalRow
	getJSON(t, c, url+"/api/v1/accounts", &rows)
	if r := rowOf(t, rows, tinv); r.CountedBy != "journal" || r.Journal == nil || r.Journal.AmountMinor != 86_000_000 ||
		r.Journal.FullAmountMinor != 91_000_000 || r.Journal.NotTradedPositions != 1 ||
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

	// Pinned, Альфа counts by its balance and still reports its journal.
	resp := apitest.Do(t, c, "PATCH", url+"/api/v1/accounts/"+alfa, `{"valued_by_balance":true}`)
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
	resp = apitest.Do(t, c, "PATCH", url+"/api/v1/accounts/"+alfa, `{"name":"Альфа-Инвестиции"}`)
	var renamed journalRow
	_ = json.NewDecoder(resp.Body).Decode(&renamed)
	if !renamed.ValuedByBalance {
		t.Errorf("renamed = %+v, want still valued by its balance", renamed)
	}
}

// A multi-currency journal is totalled under each currency, a shortfall as
// debt; a foreign balance is reconciled at today's rate.
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

// A mark older than three days is compared with the journal on its own day;
// no verdict when the journal cannot be valued whole then.
func TestAnOldBalanceIsComparedOnItsOwnDay(t *testing.T) {
	journals := &fakeJournals{byAccount: map[string]account.JournalValue{}, onDay: map[string]account.JournalValue{}}
	url, c, md := newAPIWithJournals(t, journals)
	march := time.Date(2026, 3, 12, 0, 0, 0, 0, time.UTC)
	if err := md.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: march, Rate: decimal.RequireFromString("80"), Source: "test"},
		{Base: "USD", Quote: "RUB", On: time.Now().UTC(), Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatal(err)
	}
	// A dollar balance is put into rubles at its own day's rate.
	dollars := createAccount(t, url, c, "В долларах", "brokerage", "USD")
	balanceOn(t, url, c, dollars, march, 100_000)
	journals.byAccount[dollars] = account.JournalValue{Currency: "RUB", Minor: 1, Operations: 9}
	journals.onDay[dollars+"@2026-03-12"] = account.JournalValue{Currency: "RUB", Minor: 8_000_000, Operations: 5}

	agrees := createAccount(t, url, c, "Сходится", "brokerage", "RUB")
	differs := createAccount(t, url, c, "Не сходится", "brokerage", "RUB")
	unpriced := createAccount(t, url, c, "Без цены", "brokerage", "RUB")
	before := createAccount(t, url, c, "Раньше журнала", "brokerage", "RUB")
	for _, id := range []string{agrees, differs, unpriced, before} {
		balanceOn(t, url, c, id, march, 54_000_000)
		// Today's worth is far from March's balance: the market moved.
		journals.byAccount[id] = account.JournalValue{
			Currency: "RUB", Minor: 70_000_000, ByCurrency: map[string]int64{"RUB": 70_000_000}, Operations: 9,
		}
	}
	key := func(id string) string { return id + "@2026-03-12" }
	journals.onDay[key(agrees)] = account.JournalValue{Currency: "RUB", Minor: 53_800_000, Operations: 5}
	journals.onDay[key(differs)] = account.JournalValue{Currency: "RUB", Minor: 19_500_000, Operations: 5}
	journals.onDay[key(unpriced)] = account.JournalValue{Currency: "RUB", Minor: 53_900_000, Operations: 5, Unpriced: 1}
	journals.onDay[key(before)] = account.JournalValue{Currency: "RUB", Minor: 0, Operations: 0}

	var rows []journalRow
	getJSON(t, c, url+"/api/v1/accounts", &rows)
	for id, want := range map[string][2]string{
		agrees: {"agrees", "-200000"}, differs: {"differs", "-34500000"}, dollars: {"agrees", "0"},
		unpriced: {"stale", "-100000"}, before: {"stale", "-54000000"},
	} {
		r := rowOf(t, rows, id)
		if r.Journal == nil || r.Journal.Reconciliation == nil {
			t.Fatalf("account %s has no reconciliation: %+v", id, r)
		}
		rec := r.Journal.Reconciliation
		if rec.Status != want[0] || fmt.Sprint(rec.DifferenceMinor) != want[1] || rec.ComparedOn != "2026-03-12" {
			t.Errorf("account %s = %+v, want %s, %s apart, compared on 2026-03-12", id, rec, want[0], want[1])
		}
	}
}

type capitalSeries struct {
	Currency string `json:"currency"`
	Points   []struct {
		Day        string `json:"day"`
		TotalMinor int64  `json:"total_minor"`
		Complete   bool   `json:"complete"`
		Accounts   []struct {
			AccountID   string `json:"account_id"`
			AmountMinor int64  `json:"amount_minor"`
			CountedBy   string `json:"counted_by"`
			Complete    bool   `json:"complete"`
		} `json:"accounts"`
	} `json:"points"`
}

// The family's worth at month ends: a broker by its journal then, a deposit by
// its latest mark by then, a dollar card at that day's rate; an incomplete
// month is marked.
func TestTheFamilysWorthIsSeriesOfMonthEnds(t *testing.T) {
	journals := &fakeJournals{byAccount: map[string]account.JournalValue{}, onDay: map[string]account.JournalValue{}}
	url, c, md := newAPIWithJournals(t, journals)
	day := func(s string) time.Time { d, _ := time.Parse(time.DateOnly, s); return d }
	if err := md.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: day("2026-01-01"), Rate: decimal.RequireFromString("80"), Source: "test"},
		{Base: "USD", Quote: "RUB", On: day("2026-03-01"), Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatal(err)
	}
	broker := createAccount(t, url, c, "Брокер", "brokerage", "RUB")
	deposit := createAccount(t, url, c, "Вклад", "deposit", "RUB")
	card := createAccount(t, url, c, "Карта", "credit_card", "USD")
	journals.byAccount[broker] = account.JournalValue{Currency: "RUB", Minor: 1, Operations: 3}
	// The chart reads the full worth (decision Р-11); the liquid one beside it
	// is left at nothing to show it is not read.
	journals.onDay[broker+"@2026-01-31"] = account.JournalValue{Currency: "RUB", FullMinor: 1_000_000, Operations: 1}
	journals.onDay[broker+"@2026-02-28"] = account.JournalValue{Currency: "RUB", FullMinor: 1_200_000, Operations: 2, FullUnpriced: 1}
	journals.onDay[broker+"@2026-03-31"] = account.JournalValue{Currency: "RUB", FullMinor: 1_500_000, Operations: 3}
	balanceOn(t, url, c, deposit, day("2026-02-10"), 5_000_000)
	balanceOn(t, url, c, card, day("2026-01-15"), -10_000)

	var got capitalSeries
	getJSON(t, c, url+"/api/v1/capital?from=2026-01-05&step=month", &got)
	if got.Currency != "RUB" || len(got.Points) < 4 {
		t.Fatalf("series = %+v, want January to March and today", got)
	}
	// January: broker 10 000, no deposit yet, card −100 $ × 80.
	// February: broker 12 000 (incomplete), deposit 50 000, card −100 $ × 80.
	// March: broker 15 000, deposit 50 000, card −100 $ × 90.
	want := []struct {
		day      string
		total    int64
		complete bool
	}{
		{"2026-01-31", 1_000_000 - 800_000, true},
		{"2026-02-28", 1_200_000 + 5_000_000 - 800_000, false},
		{"2026-03-31", 1_500_000 + 5_000_000 - 900_000, true},
	}
	for i, w := range want {
		p := got.Points[i]
		if p.Day != w.day || p.TotalMinor != w.total || p.Complete != w.complete {
			t.Errorf("point %d = %s %d complete=%v, want %s %d complete=%v", i, p.Day, p.TotalMinor, p.Complete, w.day, w.total, w.complete)
		}
	}
	for _, a := range got.Points[0].Accounts {
		if a.AccountID == broker && a.CountedBy != "journal" {
			t.Errorf("the broker is counted by %s, want its journal", a.CountedBy)
		}
	}
	if last := got.Points[len(got.Points)-1]; last.Day != time.Now().UTC().Format(time.DateOnly) {
		t.Errorf("last point is %s, want today", last.Day)
	}

	resp := apitest.Do(t, c, "GET", url+"/api/v1/capital?from=2016-01-01&step=week", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("ten years by week = %d, want 400", resp.StatusCode)
	}
}

// The family's return is over its journal-valued accounts; moves between them
// cancel, and a deposit is not included.
func TestTheFamilysReturnIsReckonedOverItsJournals(t *testing.T) {
	journals := &fakeJournals{byAccount: map[string]account.JournalValue{}, periods: map[string]account.ReturnBasis{}}
	url, c, _ := newAPIWithJournals(t, journals)
	day := func(s string) time.Time { d, _ := time.Parse(time.DateOnly, s); return d }

	a := createAccount(t, url, c, "Брокер А", "brokerage", "RUB")
	b := createAccount(t, url, c, "Брокер Б", "brokerage", "RUB")
	deposit := createAccount(t, url, c, "Вклад", "deposit", "RUB")
	balanceOn(t, url, c, deposit, day("2026-01-10"), 99_000_000)
	for _, id := range []string{a, b} {
		journals.byAccount[id] = account.JournalValue{Currency: "RUB", Minor: 1, Operations: 3}
	}
	journals.periods[a] = account.ReturnBasis{
		Start: account.JournalValue{Minor: 10_000_000}, End: account.JournalValue{Minor: 6_000_000}, Complete: true,
		Flows: []account.ReturnFlow{
			{Day: day("2026-03-01"), Minor: -2_000_000}, // a deposit
			{Day: day("2026-04-01"), Minor: 5_000_000},  // shares out to Б
		},
	}
	journals.periods[b] = account.ReturnBasis{
		Start: account.JournalValue{Minor: 0}, End: account.JournalValue{Minor: 7_300_000}, Complete: true,
		Flows: []account.ReturnFlow{
			{Day: day("2026-04-01"), Minor: -5_000_000}, // the same shares in
		},
	}

	var got struct {
		StartMinor         int64   `json:"start_minor"`
		EndMinor           int64   `json:"end_minor"`
		ContributionsMinor int64   `json:"contributions_minor"`
		ProfitMinor        int64   `json:"profit_minor"`
		AnnualRate         *string `json:"annual_rate"`
		Complete           bool    `json:"complete"`
		Accounts           int     `json:"accounts"`
	}
	getJSON(t, c, url+"/api/v1/return?from=2025-12-31&to=2026-09-30", &got)
	// 100 000 + 20 000 put in; 60 000 + 73 000 at the end: 13 000 earned.
	if got.StartMinor != 10_000_000 || got.EndMinor != 13_300_000 || got.ContributionsMinor != 2_000_000 ||
		got.ProfitMinor != 1_300_000 || !got.Complete || got.Accounts != 2 || got.AnnualRate == nil {
		t.Errorf("family return = %+v, want 100 000 → 133 000 with 20 000 put in, 13 000 earned over 2 accounts", got)
	}

	inc := journals.periods[b]
	inc.Complete = false
	journals.periods[b] = inc
	getJSON(t, c, url+"/api/v1/return?from=2025-12-31&to=2026-09-30", &got)
	if got.Complete {
		t.Error("an account valued only in part left the family's period complete")
	}
}

// An everyday account is counted by its balance until the family keeps it by
// its operations; then its journal counts, a credit card's as a debt, it is
// reconciled with the bank's balance, and it stays out of the family's return.
func TestAnEverydayAccountIsKeptByItsOperationsWhenAsked(t *testing.T) {
	journals := &fakeJournals{byAccount: map[string]account.JournalValue{}, periods: map[string]account.ReturnBasis{}}
	url, c, _ := newAPIWithJournals(t, journals)
	today := time.Now().UTC()

	card := createAccount(t, url, c, "Кредитка", "credit_card", "RUB")
	balanceOn(t, url, c, card, today, -3_000_000)
	journals.byAccount[card] = account.JournalValue{
		Currency: "RUB", Minor: -2_990_000, ByCurrency: map[string]int64{"RUB": -2_990_000},
		Operations: 17, NegativeCash: []string{"RUB"}, FullMinor: -2_990_000,
	}
	journals.periods[card] = account.ReturnBasis{
		Start: account.JournalValue{Minor: 0}, End: account.JournalValue{Minor: -2_990_000}, Complete: true,
	}

	var rows []journalRow
	getJSON(t, c, url+"/api/v1/accounts", &rows)
	if r := rowOf(t, rows, card); r.CountedBy != "balance" || r.Journal == nil || r.Journal.AmountMinor != -2_990_000 {
		t.Errorf("before = %+v, want counted by its balance with its journal beside it", r)
	}
	var sum journalSummary
	getJSON(t, c, url+"/api/v1/summary", &sum)
	if sum.Journal.PinnedToBalance != 0 || sum.Journal.Accounts != 0 {
		t.Errorf("summary before = %+v, want nothing pinned: kept by balance is an everyday account's way", sum.Journal)
	}

	resp := apitest.Do(t, c, "PATCH", url+"/api/v1/accounts/"+card, `{"kept_by_operations":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("keep by operations = %d", resp.StatusCode)
	}
	getJSON(t, c, url+"/api/v1/accounts", &rows)
	if r := rowOf(t, rows, card); r.CountedBy != "journal" || r.Journal.Reconciliation == nil ||
		r.Journal.Reconciliation.Status != "agrees" || r.Journal.Reconciliation.DifferenceMinor != 10_000 {
		t.Errorf("after = %+v, want counted by a journal agreeing with the bank, 100 ₽ apart", r)
	}
	getJSON(t, c, url+"/api/v1/summary", &sum)
	if sum.TotalInBaseMinor == nil || *sum.TotalInBaseMinor != -2_990_000 ||
		len(sum.Totals) != 1 || sum.Totals[0].LiabilitiesMinor != -2_990_000 || sum.Journal.Accounts != 1 {
		t.Errorf("summary after = %+v, want the journal's debt", sum)
	}
	var ret struct {
		Accounts int `json:"accounts"`
	}
	getJSON(t, c, url+"/api/v1/return?from=2025-12-31&to=2026-09-30", &ret)
	if ret.Accounts != 0 {
		t.Errorf("the family's return counted %d accounts, want none: a card is not an investment", ret.Accounts)
	}
}
