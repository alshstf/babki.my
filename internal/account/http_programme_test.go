package account_test

import (
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
)

// programmeAPI serves the accounts with the key rate at 14 % since July.
func programmeAPI(t *testing.T) (string, *http.Client) {
	t.Helper()
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	md := marketdata.NewStore(pool)
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	account.NewHandler(account.NewStore(pool), famStore, marketdata.NewConverter(md), nil, auth, sm).WithKeyRates(md).Mount(srv)
	url, c := apitest.Serve(t, srv.Handler())
	if err := md.UpsertKeyRates(t.Context(), []marketdata.KeyRate{{On: time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC), Rate: decimal.NewFromInt(14), Source: "test"}}); err != nil {
		t.Fatal(err)
	}
	return url, c
}

func newAccount(t *testing.T, url string, c *http.Client, typ string, balance int64) string {
	t.Helper()
	var a struct {
		ID string `json:"id"`
	}
	apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/accounts", fmt.Sprintf(`{"name":"Счёт","type":%q,"currency":"RUB"}`, typ)), &a)
	if balance > 0 {
		r := apitest.Do(t, c, "PUT", url+"/api/v1/accounts/"+a.ID+"/balance", fmt.Sprintf(`{"amount_minor":%d,"as_of":"2026-10-10"}`, balance))
		if r.StatusCode != http.StatusOK {
			t.Fatalf("balance = %d", r.StatusCode)
		}
	}
	return a.ID
}

// Decision Р-53 Б: a deposit tied to Газпромбанк's «Ключевой момент» for 367
// days earns the key rate less 0,2 by the catalog; a rate written in by hand
// stands over it, and taken away gives the catalog back. A savings account
// for a deposit's programme, a term the bank has no rate for, an unknown
// programme, a card and a savings account opened before its variant's rates
// are refused.
func TestADepositTiedToItsProgramme(t *testing.T) {
	url, c := programmeAPI(t)
	dep := newAccount(t, url, c, "deposit", 500_000_00)
	rates := url + "/api/v1/accounts/" + dep

	bind := `{"programme_id":"gazprombank-klyuchevoy-moment","variant_id":"end","opened_on":"2026-10-05","term":367,"amount_minor":50000000,"conditions":"manual"}`
	var got apitypes.AccountRates
	apitest.Decode(t, apitest.Do(t, c, "PUT", rates+"/programme", bind), &got)
	p, q := got.Programme.MustGet(), got.Quote.MustGet()
	if got.Source != apitypes.AccountRatesSourceProgramme || q.Percent != "13.800" || p.BankName != "Газпромбанк" ||
		p.ProgrammeName != "Вклад «Ключевой момент»" || p.Term.MustGet() != 367 || p.TermUnit.MustGet() != "days" || q.EditionFrom != "2026-10-03" {
		t.Fatalf("bound = %+v, programme %+v, quote %+v", got, p, q)
	}

	apitest.Decode(t, apitest.Do(t, c, "PUT", rates+"/rates", `{"from_on":"2026-10-05","kind":"fixed","percent":"15"}`), &got)
	if got.Source != apitypes.AccountRatesSourceByHand || got.Current.MustGet().Percent != "15.000" || got.Quote.MustGet().Percent != "13.800" {
		t.Errorf("by hand over the programme = %+v", got)
	}
	apitest.Decode(t, apitest.Do(t, c, "DELETE", rates+"/rates/2026-10-05", ""), &got)
	if got.Source != apitypes.AccountRatesSourceProgramme {
		t.Errorf("the hand's rate taken away: source %s", got.Source)
	}

	savings := newAccount(t, url, c, "savings", 0)
	card := newAccount(t, url, c, "checking", 0)
	for _, bad := range []struct{ account, body string }{
		{savings, bind},
		{dep, `{"programme_id":"gazprombank-klyuchevoy-moment","variant_id":"end","opened_on":"2026-10-05","term":366,"amount_minor":50000000,"conditions":"manual"}`},
		{dep, `{"programme_id":"gazprombank-klyuchevoy-moment","variant_id":"end","opened_on":"2026-10-02","term":367,"amount_minor":50000000,"conditions":"manual"}`},
		{dep, `{"programme_id":"nope","variant_id":"end","opened_on":"2026-10-05","term":367,"amount_minor":50000000,"conditions":"manual"}`},
		{card, bind},
		{savings, `{"programme_id":"alfa-schet","variant_id":"minimum","opened_on":"2026-02-10","term":null,"amount_minor":null,"conditions":"manual"}`},
	} {
		if r := apitest.Do(t, c, "PUT", url+"/api/v1/accounts/"+bad.account+"/programme", bad.body); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad.body, r.StatusCode)
		}
	}

	apitest.Decode(t, apitest.Do(t, c, "DELETE", rates+"/programme", ""), &got)
	if !got.Programme.IsNull() || got.Source != apitypes.AccountRatesSourceNone {
		t.Errorf("untied = %+v", got)
	}
	if r := apitest.Do(t, c, "DELETE", rates+"/programme", ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("untied twice = %d, want 404", r.StatusCode)
	}
}

// The ВТБ-Счёт on the minimal balance, opened today: the base 6 % without a
// mark; marked a new client — the welcome 14,2 %. Opened half a year ago,
// marked for the wages and purchases from 50 000 ₽ — 6 + 2 + 4; the purchases'
// mark taken back from this month — 8. A condition the programme has not, a
// level past its steps are refused.
func TestASavingsAccountsConditionsByMarks(t *testing.T) {
	url, c := programmeAPI(t)
	acc := newAccount(t, url, c, "savings", 300_000_00)
	base := url + "/api/v1/accounts/" + acc
	today := time.Now().UTC()
	month := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)

	var got apitypes.AccountRates
	bind := func(opened string) {
		apitest.Decode(t, apitest.Do(t, c, "PUT", base+"/programme", fmt.Sprintf(`{"programme_id":"vtb-schet","variant_id":"minimum","opened_on":%q,"term":null,"amount_minor":null,"conditions":"manual"}`, opened)), &got)
	}
	bind(today.Format(time.DateOnly))
	if q := got.Quote.MustGet(); q.Percent != "6.000" || !q.Period.IsNull() || len(got.Conditions) != 6 || got.Conditions[0].Id != "new_client" {
		t.Fatalf("no marks = %+v, conditions %+v", q, got.Conditions)
	}
	apitest.Decode(t, apitest.Do(t, c, "PUT", base+"/marks", fmt.Sprintf(`{"condition":"new_client","from_on":%q,"level":0,"met":true}`, month)), &got)
	if q := got.Quote.MustGet(); q.Percent != "14.200" || q.Period.MustGet() != "Приветственная" || !got.Conditions[0].Met {
		t.Errorf("a new client = %+v", q)
	}

	bind(today.AddDate(0, -6, 0).Format(time.DateOnly))
	apitest.Decode(t, apitest.Do(t, c, "PUT", base+"/marks", `{"condition":"salary","from_on":"2026-01-01","level":0,"met":true}`), &got)
	apitest.Decode(t, apitest.Do(t, c, "PUT", base+"/marks", `{"condition":"spend","from_on":"2026-01-01","level":1,"met":true}`), &got)
	if q := got.Quote.MustGet(); q.Percent != "12.000" || len(q.Bonuses) != 2 {
		t.Errorf("wages and purchases = %+v", q)
	}
	apitest.Decode(t, apitest.Do(t, c, "PUT", base+"/marks", fmt.Sprintf(`{"condition":"spend","from_on":%q,"level":0,"met":false}`, month)), &got)
	if q := got.Quote.MustGet(); q.Percent != "8.000" || len(got.Marks) != 4 {
		t.Errorf("purchases no more = %+v, marks %+v", q, got.Marks)
	}
	apitest.Decode(t, apitest.Do(t, c, "DELETE", base+"/marks/spend/"+month, ""), &got)
	if q := got.Quote.MustGet(); q.Percent != "12.000" {
		t.Errorf("the mark taken back = %+v", q)
	}
	for _, bad := range []string{
		`{"condition":"guess","from_on":"2026-01-01","level":0,"met":true}`,
		`{"condition":"spend","from_on":"2026-01-01","level":2,"met":true}`,
		`{"condition":"salary","from_on":"2026-01-01","level":1,"met":true}`,
	} {
		if r := apitest.Do(t, c, "PUT", base+"/marks", bad); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, r.StatusCode)
		}
	}
}

// The catalog's listing: each programme with its bank and variants, a
// deposit's terms and least sum, the conditions.
func TestTheCatalogOfProgrammes(t *testing.T) {
	url, c := programmeAPI(t)
	var got apitypes.DepositProgrammes
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/deposit-programmes", ""), &got)
	byID := map[string]apitypes.DepositProgramme{}
	for _, p := range got.Programmes {
		byID[p.Id] = p
	}
	gpb, vtb := byID["gazprombank-klyuchevoy-moment"], byID["vtb-schet"]
	if len(gpb.Variants) != 1 || len(gpb.Variants[0].Terms) != 7 || gpb.Variants[0].AmountFromMinor != 100_000_00 || gpb.BankName != "Газпромбанк" {
		t.Errorf("Газпромбанк = %+v", gpb)
	}
	if len(vtb.Variants) != 2 || vtb.Variants[0].Welcome.MustGet() != "Приветственная" || len(vtb.Variants[0].Conditions) != 6 || vtb.Kind != "savings" {
		t.Errorf("ВТБ = %+v", vtb)
	}
}
