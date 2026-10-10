package loan_test

import (
	"fmt"
	"log/slog"
	"net/http"
	"testing"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/loan"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
)

// A loan's terms give its schedule; a payment from a card is the interest as a
// spending under «Проценты по кредитам» and the rest a transfer that brings the
// debt down.
func TestALoanIsPaidByItsSchedule(t *testing.T) {
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	accStore, opStore := account.NewStore(pool), operation.NewStore(pool)
	opSvc := operation.NewService(opStore)
	conv := marketdata.NewConverter(marketdata.NewStore(pool))
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	account.NewHandler(accStore, famStore, conv, nil, auth, sm).Mount(srv)
	operation.NewHandler(opSvc, opStore, famStore, conv, auth, sm).Mount(srv)
	category.NewHandler(category.NewStore(pool), auth, sm).Mount(srv)
	loan.NewHandler(loan.NewService(pool, accStore, opSvc, category.NewStore(pool)), auth, sm).Mount(srv)
	url, c := apitest.Serve(t, srv.Handler())

	mk := func(name, typ, currency string) string {
		var a struct {
			ID string `json:"id"`
		}
		apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/accounts", fmt.Sprintf(`{"name":%q,"type":%q,"currency":%q}`, name, typ, currency)), &a)
		return a.ID
	}
	mortgage, card, dollars := mk("Ипотека", "loan", "RUB"), mk("Карта", "checking", "RUB"), mk("Доллары", "checking", "USD")

	if r := apitest.Do(t, c, "GET", url+"/api/v1/accounts/"+mortgage+"/loan", ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("a loan with no terms = %d, want 404", r.StatusCode)
	}
	terms := `{"principal_minor":100000000,"annual_rate":"12","term_months":12,"issued_on":"2026-01-15","kind":"annuity"}`
	if r := apitest.Do(t, c, "PUT", url+"/api/v1/accounts/"+card+"/loan", terms); r.StatusCode != http.StatusBadRequest {
		t.Errorf("terms on a card = %d, want 400", r.StatusCode)
	}
	resp := apitest.Do(t, c, "PUT", url+"/api/v1/accounts/"+mortgage+"/loan", terms)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("terms = %d", resp.StatusCode)
	}
	var got apitypes.Loan
	apitest.Decode(t, resp, &got)
	if len(got.Schedule) != 12 || got.Schedule[0].PaymentMinor != 8_884_879 || got.TotalInterestMinor <= 0 {
		t.Errorf("schedule = %d rows, first %+v, interest %d", len(got.Schedule), got.Schedule[0], got.TotalInterestMinor)
	}

	pay := func(from string, principal, interest int64) *http.Response {
		return apitest.Do(t, c, "POST", url+"/api/v1/accounts/"+mortgage+"/loan/payments",
			fmt.Sprintf(`{"from_account_id":%q,"occurred_on":"2026-02-15","principal_minor":%d,"interest_minor":%d}`, from, principal, interest))
	}
	if r := pay(dollars, 7_884_879, 1_000_000); r.StatusCode != http.StatusBadRequest {
		t.Errorf("paying from a dollar account = %d, want 400", r.StatusCode)
	}
	if r := pay(card, 7_884_879, 1_000_000); r.StatusCode != http.StatusCreated {
		t.Fatalf("the first payment = %d", r.StatusCode)
	}
	var cats []apitypes.Category
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/categories", ""), &cats)
	interestCat := ""
	for _, ct := range cats {
		if ct.Name == loan.InterestCategory {
			interestCat = ct.Id.String()
		}
	}
	var page struct {
		Operations []struct {
			Type        string  `json:"type"`
			AmountMinor int64   `json:"amount_minor"`
			CategoryID  *string `json:"category_id"`
		} `json:"operations"`
	}
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/accounts/"+card+"/operations", ""), &page)
	var interest, principal bool
	for _, o := range page.Operations {
		if o.Type == "withdrawal" && o.AmountMinor == -1_000_000 && o.CategoryID != nil && *o.CategoryID == interestCat {
			interest = true
		}
		if o.Type == "withdrawal" && o.AmountMinor == -7_884_879 && o.CategoryID == nil {
			principal = true
		}
	}
	if !interest || !principal {
		t.Errorf("the card's journal = %+v, want the interest filed and the transfer apart", page.Operations)
	}
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/accounts/"+mortgage+"/operations", ""), &page)
	if len(page.Operations) != 1 || page.Operations[0].Type != "deposit" || page.Operations[0].AmountMinor != 7_884_879 {
		t.Errorf("the loan's journal = %+v, want the transfer in", page.Operations)
	}
}
