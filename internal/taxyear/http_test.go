package taxyear_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/portfolio"
	"babki.my/babki/internal/taxyear"
)

type noFacts struct{}

func (noFacts) YearFacts(context.Context, uuid.UUID, uuid.UUID, int, string, int) (portfolio.YearFacts, error) {
	return portfolio.YearFacts{}, nil
}

// Two wage payments of one employer: the program reckons both from what
// reached the card; the first's payslip written by hand stands for its
// reckoning — a bonus withheld at the same 13 % but 2 000 short — and
// dropped, the reckoning is back. A gift and a sum that does not fit are
// refused.
func TestAWagesPayslipOverHTTP(t *testing.T) {
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	accStore, opStore, md := account.NewStore(pool), operation.NewStore(pool), marketdata.NewStore(pool)
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	account.NewHandler(accStore, famStore, marketdata.NewConverter(md), nil, auth, sm).Mount(srv)
	operation.NewHandler(operation.NewService(opStore), opStore, famStore, marketdata.NewConverter(md), auth, sm).Mount(srv)
	category.NewHandler(category.NewStore(pool), auth, sm).Mount(srv)
	taxyear.NewHandler(taxyear.NewService(accStore, noFacts{}, famStore, md).WithEarnings(opStore, category.NewStore(pool)).
		WithEntries(pool, opStore), auth, sm).Mount(srv)
	url, c := apitest.Serve(t, srv.Handler())

	var acc struct {
		ID string `json:"id"`
	}
	apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/accounts", `{"name":"Карта","type":"checking","currency":"RUB"}`), &acc)
	var cats []struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
		Name string `json:"name"`
	}
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/categories", ""), &cats)
	byName := map[string]string{}
	for _, x := range cats {
		byName[x.Kind+"/"+x.Name] = x.ID
	}
	earn := func(on string, minor int64, cat string) string {
		var op struct {
			ID string `json:"id"`
		}
		apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/operations", fmt.Sprintf(
			`{"account_id":%q,"type":"deposit","occurred_on":%q,"amount_minor":%d,"currency":"RUB","category_id":%q,"counterparty":"ООО «Ромашка»"}`,
			acc.ID, on, minor, byName[cat])), &op)
		return op.ID
	}
	bonus := earn("2026-03-05", 87_000_00, "income/Зарплата")
	earn("2026-04-05", 87_000_00, "income/Зарплата")
	gift := earn("2026-04-06", 5_000_00, "income/Подарки")

	year := func() apitypes.TaxYearPerson {
		t.Helper()
		var y apitypes.TaxYear
		apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/tax-year?year=2026", ""), &y)
		if len(y.People) != 1 {
			t.Fatalf("people = %+v", y.People)
		}
		return y.People[0]
	}
	p := year()
	if len(p.WagePayments) != 2 || p.WagePayments[0].Entered || p.WagePayments[0].GrossMinor != 100_000_00 || p.WagesMinor != 200_000_00 || p.WagesWithheldMinor != 26_000_00 {
		t.Fatalf("reckoned: %+v", p)
	}

	put := func(id, body string) *http.Response {
		return apitest.Do(t, c, "PUT", url+"/api/v1/tax-year/wages/"+id, body)
	}
	if r := put(bonus, `{"gross_minor":11500000,"withheld_minor":1300000}`); r.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("written = %d %s", r.StatusCode, b)
	}
	p = year()
	w := p.WagePayments[0]
	if !w.Entered || w.GrossMinor != 115_000_00 || w.WithheldMinor != 13_000_00 || w.NetMinor != 87_000_00 || p.WagePayments[1].Entered {
		t.Errorf("the payslip: %+v", p.WagePayments)
	}
	// 115 000 + 100 000 at 13 % is 27 950; withheld 13 000 + 13 000.
	if p.WagesMinor != 215_000_00 || p.WagesTaxMinor != 27_950_00 || p.WagesWithheldMinor != 26_000_00 || p.WagesToPayMinor != 1_950_00 {
		t.Errorf("with the payslip: %+v", p)
	}

	for body, id := range map[string]string{
		`{"gross_minor":100,"withheld_minor":0}`:   gift,
		`{"gross_minor":0,"withheld_minor":0}`:     bonus,
		`{"gross_minor":100,"withheld_minor":101}`: bonus,
	} {
		if r := put(id, body); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s on %s = %d, want 400", body, id, r.StatusCode)
		}
	}
	if r := put(uuid.NewString(), `{"gross_minor":100,"withheld_minor":0}`); r.StatusCode != http.StatusNotFound {
		t.Errorf("no such payment = %d, want 404", r.StatusCode)
	}

	if r := apitest.Do(t, c, "DELETE", url+"/api/v1/tax-year/wages/"+bonus, ""); r.StatusCode != http.StatusNoContent {
		t.Fatalf("dropped = %d", r.StatusCode)
	}
	if p := year(); p.WagePayments[0].Entered || p.WagesMinor != 200_000_00 {
		t.Errorf("reckoned again: %+v", p)
	}
}
