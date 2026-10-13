package goals_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/goals"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/structure"
)

type noPositions struct{}

func (noPositions) Positions(context.Context, uuid.UUID, uuid.UUID) (apitypes.PositionsResponse, int, error) {
	return apitypes.PositionsResponse{}, 0, nil
}

// The note's family (Р-44): the cushion on a whole savings account, the
// holiday on 70 000 out of the card, set, read back with their progress,
// changed and taken away; a debt as a fund, a stranger's account and a name
// left empty are refused.
func TestGoalsOverHTTP(t *testing.T) {
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	accStore, md := account.NewStore(pool), marketdata.NewStore(pool)
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	account.NewHandler(accStore, famStore, marketdata.NewConverter(md), nil, auth, sm).Mount(srv)
	structures := structure.NewService(accStore, noPositions{}, famStore, marketdata.NewConverter(md))
	goals.NewHandler(goals.NewService(pool, structures, accStore), auth, sm).Mount(srv)
	url, c := apitest.Serve(t, srv.Handler())

	mk := func(name, typ string, balance int64) string {
		var a struct {
			ID string `json:"id"`
		}
		apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/accounts", fmt.Sprintf(`{"name":%q,"type":%q,"currency":"RUB"}`, name, typ)), &a)
		if balance != 0 {
			if r := apitest.Do(t, c, "PUT", url+"/api/v1/accounts/"+a.ID+"/balance", fmt.Sprintf(`{"amount_minor":%d,"as_of":"2026-10-01"}`, balance)); r.StatusCode >= 300 {
				t.Fatalf("balance of %s = %d", name, r.StatusCode)
			}
		}
		return a.ID
	}
	savings, card, credit := mk("Накопительный", "savings", 450_000_00), mk("Карта", "checking", 180_000_00), mk("Кредитка", "credit_card", -10_000_00)

	var got apitypes.Goals
	cushion := fmt.Sprintf(`{"name":"Подушка","target_minor":57000000,"target_on":null,"monthly_minor":1500000,"yield_percent":"14","funds":[{"account_id":%q,"amount_minor":null}]}`, savings)
	apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/goals", cushion), &got)
	holiday := fmt.Sprintf(`{"name":"Отпуск","target_minor":25000000,"target_on":"2099-07-01","monthly_minor":2000000,"yield_percent":"0","funds":[{"account_id":%q,"amount_minor":7000000}]}`, card)
	apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/goals", holiday), &got)
	if len(got.Goals) != 2 || got.BaseCurrency != "RUB" {
		t.Fatalf("goals = %+v", got)
	}
	c0, h := got.Goals[0], got.Goals[1]
	if c0.Name != "Подушка" || c0.SavedMinor != 450_000_00 || c0.Status != "none" || c0.ReachOn.IsNull() || c0.Funds[0].ValueMinor != 450_000_00 {
		t.Errorf("cushion = %+v", c0)
	}
	if h.SavedMinor != 70_000_00 || h.Status != "ahead" || h.TargetOn.MustGet() != "2099-07-01" || h.Funds[0].AmountMinor.MustGet() != 70_000_00 {
		t.Errorf("holiday = %+v", h)
	}

	// The holiday moved up to a date it cannot reach; far off, 20 000 a month
	// had it well ahead.
	changed := fmt.Sprintf(`{"name":"Отпуск","target_minor":25000000,"target_on":"2026-11-01","monthly_minor":2000000,"yield_percent":"0","funds":[{"account_id":%q,"amount_minor":7000000}]}`, card)
	apitest.Decode(t, apitest.Do(t, c, "PUT", url+"/api/v1/goals/"+h.Id.String(), changed), &got)
	if h := got.Goals[1]; h.Status != "at_risk" || h.NeededMinor.IsNull() {
		t.Errorf("holiday moved up = %+v", h)
	}

	for _, bad := range []string{
		fmt.Sprintf(`{"name":"Долг","target_minor":100,"target_on":null,"monthly_minor":null,"yield_percent":"0","funds":[{"account_id":%q,"amount_minor":null}]}`, credit),
		`{"name":" ","target_minor":100,"target_on":null,"monthly_minor":null,"yield_percent":"0","funds":[]}`,
		`{"name":"Х","target_minor":100,"target_on":null,"monthly_minor":null,"yield_percent":"120","funds":[]}`,
	} {
		if r := apitest.Do(t, c, "POST", url+"/api/v1/goals", bad); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, r.StatusCode)
		}
	}
	stranger := fmt.Sprintf(`{"name":"Чужое","target_minor":100,"target_on":null,"monthly_minor":null,"yield_percent":"0","funds":[{"account_id":%q,"amount_minor":null}]}`, uuid.NewString())
	if r := apitest.Do(t, c, "POST", url+"/api/v1/goals", stranger); r.StatusCode != http.StatusNotFound {
		t.Errorf("a stranger's account = %d, want 404", r.StatusCode)
	}

	apitest.Decode(t, apitest.Do(t, c, "DELETE", url+"/api/v1/goals/"+h.Id.String(), ""), &got)
	if len(got.Goals) != 1 {
		t.Errorf("after the delete = %+v", got.Goals)
	}
	if r := apitest.Do(t, c, "DELETE", url+"/api/v1/goals/"+h.Id.String(), ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("deleted twice = %d, want 404", r.StatusCode)
	}
}
