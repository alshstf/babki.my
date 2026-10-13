package allocation_test

import (
	"fmt"
	"log/slog"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"babki.my/babki/internal/allocation"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
)

// A plan kept under a name comes back as typed; kept again under the same
// name it is replaced, not doubled; a second one is listed first; forgotten,
// it is gone. A plan without a name, without operations or with a purchase
// naming no paper is refused.
func TestAPlanIsSavedAndForgotten(t *testing.T) {
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	allocation.NewHandler(allocation.NewService(pool, fakeStructures{}), auth, sm).Mount(srv)
	url, c := apitest.Serve(t, srv.Handler())
	broker, savings, paper := uuid.NewString(), uuid.NewString(), sharePaper.ID.String()

	october := fmt.Sprintf(`{"name":"Октябрь","operations":[
		{"kind":"sell","account_id":%[1]q,"instrument_id":%[3]q,"currency":"RUB","amount_minor":10000000},
		{"kind":"transfer","account_id":%[2]q,"to_account_id":%[1]q,"currency":"RUB","amount_minor":5000000}]}`, broker, savings, paper)
	var got apitypes.AllocationSavedPlans
	apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/allocation/plans", october), &got)
	if len(got.Plans) != 1 || got.Plans[0].Name != "Октябрь" || len(got.Plans[0].Operations) != 2 {
		t.Fatalf("plans = %+v", got.Plans)
	}
	sell, move := got.Plans[0].Operations[0], got.Plans[0].Operations[1]
	if sell.Kind != "sell" || sell.InstrumentId.MustGet().String() != paper || !sell.ToAccountId.IsNull() || sell.AmountMinor != 100_000_00 {
		t.Errorf("sale = %+v", sell)
	}
	if move.Kind != "transfer" || move.ToAccountId.MustGet().String() != broker || !move.InstrumentId.IsNull() {
		t.Errorf("transfer = %+v", move)
	}

	again := fmt.Sprintf(`{"name":" Октябрь ","operations":[{"kind":"deposit","account_id":%q,"currency":"RUB","amount_minor":100}]}`, broker)
	apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/allocation/plans", again), &got)
	if len(got.Plans) != 1 || len(got.Plans[0].Operations) != 1 || got.Plans[0].Operations[0].Kind != "deposit" {
		t.Errorf("kept again = %+v", got.Plans)
	}
	november := fmt.Sprintf(`{"name":"Ноябрь","operations":[{"kind":"withdraw","account_id":%q,"currency":"USD","amount_minor":100}]}`, broker)
	apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/allocation/plans", november), &got)
	if len(got.Plans) != 2 || got.Plans[0].Name != "Ноябрь" {
		t.Errorf("two plans = %+v", got.Plans)
	}

	for _, bad := range []string{
		fmt.Sprintf(`{"name":" ","operations":[{"kind":"deposit","account_id":%q,"currency":"RUB","amount_minor":100}]}`, broker),
		`{"name":"Пусто","operations":[]}`,
		fmt.Sprintf(`{"name":"Без бумаги","operations":[{"kind":"buy","account_id":%q,"currency":"RUB","amount_minor":100}]}`, broker),
	} {
		if r := apitest.Do(t, c, "POST", url+"/api/v1/allocation/plans", bad); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, r.StatusCode)
		}
	}

	id := got.Plans[1].Id.String()
	apitest.Decode(t, apitest.Do(t, c, "DELETE", url+"/api/v1/allocation/plans/"+id, ""), &got)
	if len(got.Plans) != 1 || got.Plans[0].Name != "Ноябрь" {
		t.Errorf("after the delete = %+v", got.Plans)
	}
	if r := apitest.Do(t, c, "DELETE", url+"/api/v1/allocation/plans/"+id, ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("forgotten twice = %d, want 404", r.StatusCode)
	}
}
