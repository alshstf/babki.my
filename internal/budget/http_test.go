package budget_test

import (
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/budget"
	"babki.my/babki/internal/cashflow"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
)

// Limits are stated for spending categories from a month on; the month's
// budget tells each one's spending from the journal, a копилка what the
// months before it left; a limit taken back is gone.
func TestTheBudgetOverHTTP(t *testing.T) {
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	accStore, opStore := account.NewStore(pool), operation.NewStore(pool)
	conv := marketdata.NewConverter(marketdata.NewStore(pool))
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	account.NewHandler(accStore, famStore, conv, nil, auth, sm).Mount(srv)
	operation.NewHandler(operation.NewService(opStore), opStore, famStore, conv, auth, sm).Mount(srv)
	category.NewHandler(category.NewStore(pool), auth, sm).Mount(srv)
	report := cashflow.NewService(opStore, accStore, category.NewStore(pool), famStore, conv)
	budget.NewHandler(budget.NewService(pool, report, category.NewStore(pool)), auth, sm).Mount(srv)
	url, c := apitest.Serve(t, srv.Handler())

	var wallet struct {
		ID string `json:"id"`
	}
	apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/accounts", `{"name":"Кошелёк","type":"cash","currency":"RUB"}`), &wallet)
	var cats []apitypes.Category
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/categories", ""), &cats)
	var trips, food, salary string
	for _, ct := range cats {
		switch {
		case ct.Kind == "expense" && ct.ParentId.IsNull() && trips == "":
			trips = ct.Id.String()
		case ct.Kind == "expense" && ct.ParentId.IsNull() && food == "":
			food = ct.Id.String()
		case ct.Kind == "income" && salary == "":
			salary = ct.Id.String()
		}
	}
	now := time.Now().UTC()
	this := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	m := func(back int) string { return this.AddDate(0, -back, 0).Format("2006-01") }
	set := func(category, from string, amount int64, rollover bool) *http.Response {
		return apitest.Do(t, c, "PUT", url+"/api/v1/budget/limits",
			fmt.Sprintf(`{"category_id":%q,"from_month":%q,"amount_minor":%d,"rollover":%t}`, category, from, amount, rollover))
	}
	// Trips: 5 000 a month into a копилка since two months back; food 3 000
	// from this month.
	for _, r := range []*http.Response{set(trips, m(2), 500_000, true), set(food, m(0), 300_000, false)} {
		if r.StatusCode != http.StatusNoContent {
			t.Fatalf("set limit = %d", r.StatusCode)
		}
	}
	for name, r := range map[string]*http.Response{
		"an income category": set(salary, m(0), 100_000, false),
		"a bad month":        set(trips, "2026-13", 100_000, false),
		"below zero":         set(trips, m(0), -1, false),
	} {
		if r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, r.StatusCode)
		}
	}
	if r := apitest.Do(t, c, "POST", url+"/api/v1/operations",
		fmt.Sprintf(`{"account_id":%q,"type":"withdrawal","occurred_on":%q,"amount_minor":-200000,"currency":"RUB","category_id":%q}`,
			wallet.ID, this.Format(time.DateOnly), trips)); r.StatusCode != http.StatusCreated {
		t.Fatalf("spending = %d", r.StatusCode)
	}

	var b apitypes.Budget
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/budget?month="+m(0), ""), &b)
	if b.Month != m(0) || b.BaseCurrency != "RUB" || len(b.Lines) != 2 {
		t.Fatalf("budget = %+v", b)
	}
	byCategory := map[string]apitypes.BudgetLine{}
	for _, l := range b.Lines {
		byCategory[l.CategoryId.String()] = l
	}
	// Two months of 5 000 carried, 2 000 spent of 15 000.
	if l := byCategory[trips]; l.CarriedMinor != 1_000_000 || l.SpentMinor != 200_000 || l.LeftMinor != 1_300_000 || !l.Rollover || l.Since != m(2) {
		t.Errorf("trips = %+v", l)
	}
	if l := byCategory[food]; l.LimitMinor != 300_000 || l.SpentMinor != 0 || l.LeftMinor != 300_000 {
		t.Errorf("food = %+v", l)
	}
	if b.PlannedMinor != 1_800_000 || b.SpentMinor != 200_000 || b.LeftMinor != 1_600_000 || b.UnlimitedMinor != 0 {
		t.Errorf("sums = %+v", b)
	}
	if r := apitest.Do(t, c, "GET", url+"/api/v1/budget?month=13.2026", ""); r.StatusCode != http.StatusBadRequest {
		t.Errorf("a bad month = %d, want 400", r.StatusCode)
	}

	// Food's limit taken back: no line; once more, nothing to take back.
	if r := apitest.Do(t, c, "DELETE", url+"/api/v1/budget/limits/"+food+"/"+m(0), ""); r.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d", r.StatusCode)
	}
	if r := apitest.Do(t, c, "DELETE", url+"/api/v1/budget/limits/"+food+"/"+m(0), ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("delete again = %d, want 404", r.StatusCode)
	}
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/budget?month="+m(0), ""), &b)
	if len(b.Lines) != 1 || b.Lines[0].CategoryId.String() != trips {
		t.Errorf("after delete = %+v", b.Lines)
	}
	var limits []apitypes.BudgetLimit
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/budget/limits", ""), &limits)
	if len(limits) != 1 || limits[0].FromMonth != m(2) || limits[0].AmountMinor != 500_000 {
		t.Errorf("limits = %+v", limits)
	}
}
