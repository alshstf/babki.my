package recurring_test

import (
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/recurring"
)

// A payment the family says is not regular stays in the list, marked hidden,
// until it is taken back; another spelling of the payee names the same one.
func TestAPaymentIsHiddenAndShownAgain(t *testing.T) {
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
	hidden := recurring.NewHidden(pool)
	recurring.NewHandler(recurring.NewService(opStore, accStore, hidden), hidden, auth, sm).Mount(srv)
	url, c := apitest.Serve(t, srv.Handler())

	var acc struct {
		ID string `json:"id"`
	}
	apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/accounts", `{"name":"Карта","type":"checking","currency":"RUB"}`), &acc)
	today := time.Now().UTC()
	for _, back := range []int{95, 65, 35, 5} {
		on := today.AddDate(0, 0, -back).Format(time.DateOnly)
		r := apitest.Do(t, c, "POST", url+"/api/v1/operations",
			fmt.Sprintf(`{"account_id":%q,"type":"withdrawal","occurred_on":%q,"amount_minor":-150000,"currency":"RUB","counterparty":"Пятёрочка"}`, acc.ID, on))
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("spending = %d", r.StatusCode)
		}
	}
	list := func() []apitypes.RecurringPayment {
		var out []apitypes.RecurringPayment
		apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/recurring", ""), &out)
		return out
	}
	if got := list(); len(got) != 1 || got[0].Name != "Пятёрочка" || got[0].Hidden {
		t.Fatalf("before = %+v", got)
	}
	if r := apitest.Do(t, c, "PUT", url+"/api/v1/recurring/hidden", `{"name":"ПЯТЕРОЧКА","incoming":false,"currency":"RUB"}`); r.StatusCode != http.StatusNoContent {
		t.Fatalf("hide = %d", r.StatusCode)
	}
	if got := list(); len(got) != 1 || !got[0].Hidden {
		t.Errorf("hidden = %+v", got)
	}
	if r := apitest.Do(t, c, "DELETE", url+"/api/v1/recurring/hidden", `{"name":"Пятёрочка","incoming":false,"currency":"RUB"}`); r.StatusCode != http.StatusNoContent {
		t.Errorf("show = %d", r.StatusCode)
	}
	if got := list(); len(got) != 1 || got[0].Hidden {
		t.Errorf("shown again = %+v", got)
	}
	if r := apitest.Do(t, c, "DELETE", url+"/api/v1/recurring/hidden", `{"name":"Пятёрочка","incoming":false,"currency":"RUB"}`); r.StatusCode != http.StatusNotFound {
		t.Errorf("showing what is not hidden = %d, want 404", r.StatusCode)
	}
}
