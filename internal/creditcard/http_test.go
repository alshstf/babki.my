package creditcard_test

import (
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"babki.my/babki/internal/account"
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
	creditcard.NewHandler(creditcard.NewService(pool, accStore, opStore), auth, sm).Mount(srv)
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

	var list []apitypes.CreditCardSummary
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/credit-cards", ""), &list)
	if len(list) != 1 || list[0].Name != "Кредитка" || list[0].Status.DebtMinor != 1_000_000 {
		t.Errorf("list = %+v", list)
	}

	if r := apitest.Do(t, c, "DELETE", path, ""); r.StatusCode != http.StatusNoContent {
		t.Errorf("delete = %d", r.StatusCode)
	}
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/credit-cards", ""), &list)
	if len(list) != 0 {
		t.Errorf("after delete, list = %+v", list)
	}
}
