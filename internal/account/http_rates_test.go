package account_test

import (
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
)

// Decision Р-51 А: a deposit's rate written in by hand — 16 % from January,
// the key rate less 1.5 from September — read back with the one in force
// today; the September row taken away leaves January's. A card, a rate of
// 120 %, ten-thousandths, an unknown kind and a stranger's account are
// refused.
func TestAccountRatesOverHTTP(t *testing.T) {
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	md := marketdata.NewStore(pool)
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	account.NewHandler(account.NewStore(pool), famStore, marketdata.NewConverter(md), nil, auth, sm).WithKeyRates(md).Mount(srv)
	url, c := apitest.Serve(t, srv.Handler())
	if err := md.UpsertKeyRates(t.Context(), []marketdata.KeyRate{{On: time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC), Rate: decimal.NewFromInt(17), Source: "test"}}); err != nil {
		t.Fatal(err)
	}

	mk := func(typ string) string {
		var a struct {
			ID string `json:"id"`
		}
		apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/accounts", fmt.Sprintf(`{"name":"Счёт","type":%q,"currency":"RUB"}`, typ)), &a)
		return a.ID
	}
	deposit, card := mk("deposit"), mk("checking")
	rates := url + "/api/v1/accounts/" + deposit + "/rates"

	var got apitypes.AccountRates
	apitest.Decode(t, apitest.Do(t, c, "GET", rates, ""), &got)
	if len(got.Rates) != 0 || !got.Current.IsNull() || got.KeyRate.MustGet() != "17" {
		t.Fatalf("before = %+v", got)
	}
	apitest.Decode(t, apitest.Do(t, c, "PUT", rates, `{"from_on":"2026-01-01","kind":"fixed","percent":"16"}`), &got)
	apitest.Decode(t, apitest.Do(t, c, "PUT", rates, `{"from_on":"2026-09-01","kind":"key","percent":"-1.5"}`), &got)
	now := got.Current.MustGet()
	if len(got.Rates) != 2 || got.Rates[0].Percent != "16.000" || !got.Rates[1].ByHand ||
		now.Percent != "15.500" || now.Kind != "key" || now.FromOn != "2026-09-01" || !now.ByHand {
		t.Fatalf("set = %+v, now %+v", got, now)
	}

	apitest.Decode(t, apitest.Do(t, c, "DELETE", rates+"/2026-09-01", ""), &got)
	if now := got.Current.MustGet(); len(got.Rates) != 1 || now.Percent != "16.000" || now.Kind != "fixed" {
		t.Errorf("after the delete = %+v", got)
	}
	if r := apitest.Do(t, c, "DELETE", rates+"/2026-09-01", ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("deleted twice = %d, want 404", r.StatusCode)
	}

	for _, bad := range []struct{ account, body string }{
		{card, `{"from_on":"2026-01-01","kind":"fixed","percent":"16"}`},
		{deposit, `{"from_on":"2026-01-01","kind":"fixed","percent":"120"}`},
		{deposit, `{"from_on":"2026-01-01","kind":"fixed","percent":"16.0005"}`},
		{deposit, `{"from_on":"2026-01-01","kind":"floating","percent":"16"}`},
		{deposit, `{"from_on":"2026-01-01","kind":"key","percent":"60"}`},
	} {
		if r := apitest.Do(t, c, "PUT", url+"/api/v1/accounts/"+bad.account+"/rates", bad.body); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad.body, r.StatusCode)
		}
	}
	if r := apitest.Do(t, c, "PUT", url+"/api/v1/accounts/"+uuid.NewString()+"/rates", `{"from_on":"2026-01-01","kind":"fixed","percent":"16"}`); r.StatusCode != http.StatusNotFound {
		t.Errorf("a stranger's account = %d, want 404", r.StatusCode)
	}
}
