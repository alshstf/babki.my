package cashflow_test

import (
	"log/slog"
	"net/http"
	"testing"

	"babki.my/babki/internal/account"
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

// The report answers by month, YYYY-MM, and names a bad period or member.
func TestTheReportOverHTTP(t *testing.T) {
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	cashflow.NewHandler(cashflow.NewService(operation.NewStore(pool), account.NewStore(pool), category.NewStore(pool),
		famStore, marketdata.NewConverter(marketdata.NewStore(pool))), auth, sm).Mount(srv)
	base, c := apitest.Serve(t, srv.Handler())

	resp := apitest.Do(t, c, http.MethodGet, base+"/api/v1/cashflow?from=2026-08-15&to=2026-10-02", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("report = %d", resp.StatusCode)
	}
	var r apitypes.CashflowReport
	apitest.Decode(t, resp, &r)
	if len(r.Months) != 3 || r.Months[0] != "2026-08" || r.Months[2] != "2026-10" || len(r.Income.ByMonth) != 3 ||
		r.Income.Lines == nil || r.MissingRates == nil {
		t.Errorf("an empty family's report = %+v", r)
	}

	for _, query := range []string{
		"from=2026-08-15", "from=15.08.2026&to=2026-10-02", "from=2026-10-02&to=2026-08-15",
		"from=2026-08-15&to=2026-10-02&member=nobody",
	} {
		if resp := apitest.Do(t, c, http.MethodGet, base+"/api/v1/cashflow?"+query, ""); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", query, resp.StatusCode)
		}
	}
}
