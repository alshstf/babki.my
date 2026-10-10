package creditcard_test

import (
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/creditcard"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
)

// A card's terms are stated once; counted by its balance it tells the debt,
// the limit left and an estimated minimum; kept by its operations it tells
// what to pay by when to keep the purchases free of interest.
func TestACardTellsWhatIsDue(t *testing.T) {
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
	creditcard.NewHandler(creditcard.NewService(pool, accStore, opStore, category.NewStore(pool)), auth, sm).Mount(srv)
	url, c := apitest.Serve(t, srv.Handler())

	mk := func(name, typ string) string {
		var a struct {
			ID string `json:"id"`
		}
		apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/accounts", fmt.Sprintf(`{"name":%q,"type":%q,"currency":"RUB"}`, name, typ)), &a)
		return a.ID
	}
	card, current := mk("Кредитка", "credit_card"), mk("Текущий", "checking")
	path := url + "/api/v1/accounts/" + card + "/credit-card"

	if r := apitest.Do(t, c, "GET", path, ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("a card with no terms = %d, want 404", r.StatusCode)
	}
	terms := `{"limit_minor":15000000,"statement_day":1,"payment_days":20,"grace_kind":"statement","grace_days":0,
		"min_percent":"3","min_floor_minor":30000,"annual_rate":"39.9","own_rate":null}`
	if r := apitest.Do(t, c, "PUT", url+"/api/v1/accounts/"+current+"/credit-card", terms); r.StatusCode != http.StatusBadRequest {
		t.Errorf("terms on a current account = %d, want 400", r.StatusCode)
	}
	resp := apitest.Do(t, c, "PUT", path, terms)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("terms = %d", resp.StatusCode)
	}
	var got apitypes.CreditCard
	apitest.Decode(t, resp, &got)
	if got.ByJournal || got.Status.AvailableMinor != 15_000_000 || got.Terms.AnnualRate != "39.9" || got.Terms.OwnRate.IsSpecified() && !got.Terms.OwnRate.IsNull() {
		t.Errorf("card = %+v", got)
	}
	if !got.Benefit.IsNull() {
		t.Errorf("a card counted by its balance cannot be weighed: %+v", got.Benefit)
	}

	// Kept by its operations, a purchase yesterday is due by its statement's
	// payment date.
	if r := apitest.Do(t, c, "PATCH", url+"/api/v1/accounts/"+card, `{"kept_by_operations":true}`); r.StatusCode != http.StatusOK {
		t.Fatalf("keep by operations = %d", r.StatusCode)
	}
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format(time.DateOnly)
	if r := apitest.Do(t, c, "POST", url+"/api/v1/operations",
		fmt.Sprintf(`{"account_id":%q,"type":"withdrawal","occurred_on":%q,"amount_minor":-1000000,"currency":"RUB"}`, card, yesterday)); r.StatusCode != http.StatusCreated {
		t.Fatalf("purchase = %d", r.StatusCode)
	}
	apitest.Decode(t, apitest.Do(t, c, "GET", path, ""), &got)
	if !got.ByJournal || got.Status.DebtMinor != 1_000_000 || got.Status.AvailableMinor != 14_000_000 ||
		len(got.Status.Grace) != 1 || got.Status.Grace[0].AmountMinor != 1_000_000 {
		t.Errorf("status = %+v", got.Status)
	}
	if b, err := got.Benefit.Get(); err != nil || b.OwnRateKnown || b.TotalMinor != 0 {
		t.Errorf("benefit = %+v (%v), want weighed with no own rate and nothing earned or charged", b, err)
	}

	var list []apitypes.CreditCardSummary
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/credit-cards", ""), &list)
	if len(list) != 1 || list[0].Name != "Кредитка" || list[0].Status.DebtMinor != 1_000_000 {
		t.Errorf("list = %+v", list)
	}

	// Газпромбанк's windows: stated with the day of the contract, read back
	// as stated; without that day they are refused.
	windows := `{"limit_minor":30000000,"statement_day":1,"payment_days":0,"grace_kind":"windows","grace_days":0,
		"window_months":2,"grace_months":6,"opened_on":%s,"grace_all_lost":true,"pay_by_period_end":true,"charges_in_full":true,
		"min_percent":"3","min_floor_minor":50000,"annual_rate":"59.99","own_rate":null,
		"fees":{"monthly_minor":0,"cash_free_minor":10000000,"cash_percent":"5.9","cash_fixed_minor":59000,
			"transfer_percent":"4.9","transfer_fixed_minor":39000,"penalty_daily_percent":"0.1"}}`
	if r := apitest.Do(t, c, "PUT", path, fmt.Sprintf(windows, "null")); r.StatusCode != http.StatusBadRequest {
		t.Errorf("windows with no contract day = %d, want 400", r.StatusCode)
	}
	resp = apitest.Do(t, c, "PUT", path, fmt.Sprintf(windows, `"2026-07-10"`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("windows = %d", resp.StatusCode)
	}
	apitest.Decode(t, resp, &got)
	if opened, _ := got.Terms.OpenedOn.Get(); got.Terms.GraceKind != "windows" || got.Terms.WindowMonths != 2 || got.Terms.GraceMonths != 6 ||
		opened != "2026-07-10" || !got.Terms.GraceAllLost || !got.Terms.PayByPeriodEnd || !got.Terms.ChargesInFull ||
		len(got.Status.Grace) != 1 || !got.Status.GraceOffSince.IsNull() {
		t.Errorf("windows card = %+v", got)
	}
	if f := got.Terms.Fees; f.CashFreeMinor != 10_000_000 || f.CashPercent != "5.9" || f.TransferFixedMinor != 39_000 || f.PenaltyDailyPercent != "0.1" {
		t.Errorf("fees = %+v", f)
	}

	// Cash taken out today — money moved to a cash account — counts against
	// the free part of the period.
	cash := mk("Наличные", "cash")
	today := time.Now().UTC().Format(time.DateOnly)
	if r := apitest.Do(t, c, "POST", url+"/api/v1/operations/money-transfer",
		fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,"occurred_on":%q,"amount_minor":500000,"currency":"RUB","note":""}`, card, cash, today)); r.StatusCode != http.StatusCreated {
		t.Fatalf("cash out = %d", r.StatusCode)
	}
	apitest.Decode(t, apitest.Do(t, c, "GET", path, ""), &got)
	if got.Status.CashThisPeriodMinor != 500_000 {
		t.Errorf("cash this period = %d, want 5 000", got.Status.CashThisPeriodMinor)
	}

	// A transfer category is one of the family's spending categories.
	var cats []apitypes.Category
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/categories", ""), &cats)
	spending, earning := "", ""
	for _, ct := range cats {
		switch {
		case ct.Kind == "expense" && spending == "":
			spending = ct.Id.String()
		case ct.Kind == "income" && earning == "":
			earning = ct.Id.String()
		}
	}
	withCats := func(ids string) string {
		return fmt.Sprintf(`{"limit_minor":15000000,"statement_day":1,"payment_days":20,"grace_kind":"statement","grace_days":0,
			"min_percent":"3","min_floor_minor":30000,"annual_rate":"39.9","own_rate":null,"transfer_categories":[%s]}`, ids)
	}
	if r := apitest.Do(t, c, "PUT", path, withCats(fmt.Sprintf("%q", earning))); r.StatusCode != http.StatusBadRequest {
		t.Errorf("an income category as a transfer one = %d, want 400", r.StatusCode)
	}
	resp = apitest.Do(t, c, "PUT", path, withCats(fmt.Sprintf("%q,%q", spending, spending)))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("transfer categories = %d", resp.StatusCode)
	}
	apitest.Decode(t, resp, &got)
	if len(got.Terms.TransferCategories) != 1 || got.Terms.TransferCategories[0].String() != spending {
		t.Errorf("transfer categories = %v, want the spending one once", got.Terms.TransferCategories)
	}

	// Cashback rules: a spending category's percent; an income one refused.
	withCashback := func(category string) string {
		return fmt.Sprintf(`{"limit_minor":15000000,"statement_day":1,"payment_days":20,"grace_kind":"statement","grace_days":0,
			"min_percent":"3","min_floor_minor":30000,"annual_rate":"39.9","own_rate":null,
			"cashback":{"base_percent":"1","categories":[{"category_id":%q,"percent":"5"}],"monthly_cap_minor":500000,"points":true,"credit_days":3}}`, category)
	}
	if r := apitest.Do(t, c, "PUT", path, withCashback(earning)); r.StatusCode != http.StatusBadRequest {
		t.Errorf("an income category's cashback = %d, want 400", r.StatusCode)
	}
	resp = apitest.Do(t, c, "PUT", path, withCashback(spending))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cashback rules = %d", resp.StatusCode)
	}
	apitest.Decode(t, resp, &got)
	if cb := got.Terms.Cashback; cb.BasePercent != "1" || len(cb.Categories) != 1 || cb.Categories[0].Percent != "5" ||
		cb.Categories[0].CategoryId.String() != spending || cb.MonthlyCapMinor != 500_000 || !cb.Points || cb.CreditDays != 3 ||
		got.Status.CashbackOn.IsNull() {
		t.Errorf("cashback = %+v, status on %v", cb, got.Status.CashbackOn)
	}

	// A purchase in installments: three parts; not one of the card's rows
	// is a 404, and taken out it is a purchase again.
	var phone struct {
		ID string `json:"id"`
	}
	apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/operations",
		fmt.Sprintf(`{"account_id":%q,"type":"withdrawal","occurred_on":%q,"amount_minor":-1200000,"currency":"RUB"}`, card, yesterday)), &phone)
	plan := `{"months":3,"monthly_fee_percent":"4","fee_minor":0}`
	if r := apitest.Do(t, c, "PUT", path+"/installments/"+uuid.NewString(), plan); r.StatusCode != http.StatusNotFound {
		t.Errorf("installment of no row = %d, want 404", r.StatusCode)
	}
	if r := apitest.Do(t, c, "PUT", path+"/installments/"+phone.ID, `{"months":0,"monthly_fee_percent":"0","fee_minor":0}`); r.StatusCode != http.StatusBadRequest {
		t.Errorf("installment of 0 months = %d, want 400", r.StatusCode)
	}
	resp = apitest.Do(t, c, "PUT", path+"/installments/"+phone.ID, plan)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("installment = %d", resp.StatusCode)
	}
	apitest.Decode(t, resp, &got)
	if len(got.Status.Installments) != 1 || got.Status.Installments[0].AmountMinor != 1_200_000 || got.Status.Installments[0].Plan.Months != 3 {
		t.Errorf("installments = %+v", got.Status.Installments)
	}
	if r := apitest.Do(t, c, "DELETE", path+"/installments/"+phone.ID, ""); r.StatusCode != http.StatusNoContent {
		t.Errorf("take out of installments = %d", r.StatusCode)
	}

	// The catalog of tariffs; terms taken from it remember their version.
	var catalog []apitypes.CreditCardCatalogProduct
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/credit-cards/catalog", ""), &catalog)
	if len(catalog) < 4 || len(catalog[0].Versions) == 0 {
		t.Fatalf("catalog = %+v", catalog)
	}
	fromCatalog := `{"limit_minor":15000000,"statement_day":1,"payment_days":20,"grace_kind":"statement","grace_days":0,
		"min_percent":"3","min_floor_minor":30000,"annual_rate":"39.9","own_rate":null,
		"catalog":{"product":%q,"contracts_from":null,"revision":"2026-09-30"}}`
	if r := apitest.Do(t, c, "PUT", path, fmt.Sprintf(fromCatalog, "no-such-card")); r.StatusCode != http.StatusBadRequest {
		t.Errorf("a card not in the catalog = %d, want 400", r.StatusCode)
	}
	resp = apitest.Do(t, c, "PUT", path, fmt.Sprintf(fromCatalog, "tbank-platinum"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("terms from the catalog = %d", resp.StatusCode)
	}
	apitest.Decode(t, resp, &got)
	if ref, err := got.Terms.Catalog.Get(); err != nil || ref.Product != "tbank-platinum" || ref.Revision != "2026-09-30" || !got.CatalogUpdate.IsNull() {
		t.Errorf("catalog ref = %+v (%v), update %+v", ref, err, got.CatalogUpdate)
	}

	// What the bank says: stated, shown against the reckoning, forgotten.
	ahead := time.Now().UTC().AddDate(0, 0, 10).Format(time.DateOnly)
	if r := apitest.Do(t, c, "PUT", path+"/bank", fmt.Sprintf(`{"stated_on":%q,"grace":null,"minimum":null}`, today)); r.StatusCode != http.StatusBadRequest {
		t.Errorf("bank figures with nothing said = %d, want 400", r.StatusCode)
	}
	resp = apitest.Do(t, c, "PUT", path+"/bank", fmt.Sprintf(`{"stated_on":%q,"grace":null,"minimum":{"on":%q,"amount_minor":150000}}`, today, ahead))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bank figures = %d", resp.StatusCode)
	}
	apitest.Decode(t, resp, &got)
	if b, err := got.Status.Bank.Get(); err != nil || !b.Grace.IsNull() || b.Minimum.MustGet().AmountMinor != 150_000 || b.Minimum.MustGet().On != ahead {
		t.Errorf("bank = %+v (%v)", b, err)
	}
	if r := apitest.Do(t, c, "DELETE", path+"/bank", ""); r.StatusCode != http.StatusNoContent {
		t.Errorf("forget bank figures = %d", r.StatusCode)
	}

	if r := apitest.Do(t, c, "DELETE", path, ""); r.StatusCode != http.StatusNoContent {
		t.Errorf("delete = %d", r.StatusCode)
	}
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/credit-cards", ""), &list)
	if len(list) != 0 {
		t.Errorf("after delete, list = %+v", list)
	}
}
