package depositcheck_test

import (
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/depositcheck"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
)

// Decision Р-53 «Б, но»: the ВТБ-Счёт in the automatic mode. Last month the
// family paid 55 000 ₽ by the ВТБ card and got the wages on it; a purchase
// by another bank's card does not count. This month is not over, so it is
// told by the last: 6 + 2 for the wages + 4 for purchases from 50 000 ₽.
// The family's mark that the purchases no longer count stands over the
// data; the manual mode tells nothing without marks.
func TestTheConditionsToldByTheJournals(t *testing.T) {
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	accStore, opStore, catStore := account.NewStore(pool), operation.NewStore(pool), category.NewStore(pool)
	conv := marketdata.NewConverter(marketdata.NewStore(pool))
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	account.NewHandler(accStore, famStore, conv, nil, auth, sm).WithMeasures(depositcheck.New(accStore, opStore, catStore)).Mount(srv)
	operation.NewHandler(operation.NewService(opStore), opStore, famStore, conv, auth, sm).Mount(srv)
	category.NewHandler(catStore, auth, sm).Mount(srv)
	url, c := apitest.Serve(t, srv.Handler())

	mk := func(name, typ, bank string) string {
		var a struct {
			ID string `json:"id"`
		}
		apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/accounts", fmt.Sprintf(`{"name":%q,"type":%q,"currency":"RUB","institution":%q}`, name, typ, bank)), &a)
		return a.ID
	}
	card, other, savings := mk("Карта ВТБ", "checking", "ВТБ"), mk("Карта Сбера", "checking", "Сбербанк"), mk("ВТБ-Счёт", "savings", "ВТБ")
	if r := apitest.Do(t, c, "PUT", url+"/api/v1/accounts/"+savings+"/balance", `{"amount_minor":30000000,"as_of":"2026-10-10"}`); r.StatusCode != http.StatusOK {
		t.Fatalf("balance = %d", r.StatusCode)
	}
	var cats []apitypes.Category
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/categories", ""), &cats)
	var food, wages string
	for _, ct := range cats {
		if ct.Kind == "expense" && food == "" {
			food = ct.Id.String()
		}
		if ct.Name == "Зарплата" {
			wages = ct.Id.String()
		}
	}
	now := time.Now().UTC()
	this := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	last := this.AddDate(0, -1, 10).Format(time.DateOnly)
	for _, op := range []struct {
		account, typ, category string
		amount                 int64
	}{
		{card, "withdrawal", food, -30_000_00},
		{card, "withdrawal", food, -25_000_00},
		{other, "withdrawal", food, -90_000_00},
		{card, "deposit", wages, 100_000_00},
	} {
		body := fmt.Sprintf(`{"account_id":%q,"type":%q,"occurred_on":%q,"amount_minor":%d,"currency":"RUB","category_id":%q}`, op.account, op.typ, last, op.amount, op.category)
		if r := apitest.Do(t, c, "POST", url+"/api/v1/operations", body); r.StatusCode != http.StatusCreated {
			t.Fatalf("%s = %d", body, r.StatusCode)
		}
	}

	base := url + "/api/v1/accounts/" + savings
	opened := this.AddDate(0, -6, 0).Format(time.DateOnly)
	var got apitypes.AccountRates
	apitest.Decode(t, apitest.Do(t, c, "PUT", base+"/programme", fmt.Sprintf(`{"programme_id":"vtb-schet","variant_id":"minimum","opened_on":%q,"term":null,"amount_minor":null,"conditions":"auto"}`, opened)), &got)
	q := got.Quote.MustGet()
	told := map[string]apitypes.ProgrammeCondition{}
	for _, cond := range got.Conditions {
		told[cond.Id] = cond
	}
	spend, salary := told["spend"], told["salary"]
	if q.Percent != "12.000" || spend.Told != "data" || spend.MeasuredMinor.MustGet() != 55_000_00 || spend.Level != 1 ||
		spend.MeasuredMonth.MustGet() != this.AddDate(0, -1, 0).Format("2006-01") || !salary.Met || salary.Told != "data" || told["subscription"].Told != "none" {
		t.Fatalf("automatic = %+v, conditions %+v", q, got.Conditions)
	}

	apitest.Decode(t, apitest.Do(t, c, "PUT", base+"/marks", fmt.Sprintf(`{"condition":"spend","from_on":%q,"level":0,"met":false}`, this.Format(time.DateOnly))), &got)
	if q := got.Quote.MustGet(); q.Percent != "8.000" {
		t.Errorf("the mark over the data = %+v", q)
	}

	apitest.Decode(t, apitest.Do(t, c, "PUT", base+"/programme", fmt.Sprintf(`{"programme_id":"vtb-schet","variant_id":"minimum","opened_on":%q,"term":null,"amount_minor":null,"conditions":"manual"}`, opened)), &got)
	if q := got.Quote.MustGet(); q.Percent != "6.000" {
		t.Errorf("manual, the data aside = %+v", q)
	}
}
