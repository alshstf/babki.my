package allocation_test

import (
	"context"
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
	"babki.my/babki/internal/structure"
)

// fakeStructures is a worth of 1 000 000: 600 000 in shares, 400 000 in
// bonds, all in roubles, and 100 000 to live on; at the full valuation a
// frozen fund adds 500 000.
type fakeStructures struct{}

func (fakeStructures) Of(_ context.Context, _ uuid.UUID, valuation structure.Valuation) (structure.Structure, error) {
	s := structure.Structure{
		BaseCurrency: "RUB", Targets: 1_000_000_00, Everyday: 100_000_00, MissingRates: []string{},
		ByKind:           []structure.Slice{{Key: "shares", Minor: 600_000_00}, {Key: "bonds", Minor: 400_000_00}},
		ByTargetCurrency: []structure.Slice{{Key: "RUB", Minor: 1_000_000_00}},
	}
	if valuation == structure.Full {
		s.Targets += 500_000_00
		s.ByKind = append(s.ByKind, structure.Slice{Key: "funds", Minor: 500_000_00})
		s.ByTargetCurrency[0].Minor += 500_000_00
	}
	return s, nil
}

// Targets are stated by kind and by currency and set against the worth;
// targets that do not add up to 100 are refused; the mode is the family's
// pick, top-ups until then.
func TestTheTargetsOverHTTP(t *testing.T) {
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	allocation.NewHandler(allocation.NewService(pool, fakeStructures{}), auth, sm).Mount(srv)
	url, c := apitest.Serve(t, srv.Handler())

	var got apitypes.Allocation
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/allocation", ""), &got)
	if got.Mode != "topup" || got.WorthMinor != 1_000_000_00 || got.EverydayMinor != 100_000_00 || len(got.Kinds) != 2 || !got.Kinds[0].Target.IsNull() {
		t.Fatalf("before targets: %+v", got)
	}

	if r := apitest.Do(t, c, "PUT", url+"/api/v1/allocation/targets",
		`{"dimension":"kind","targets":[{"key":"shares","percent":"50"},{"key":"bonds","percent":"40"}]}`); r.StatusCode != http.StatusBadRequest {
		t.Errorf("targets of 90%% = %d, want 400", r.StatusCode)
	}
	resp := apitest.Do(t, c, "PUT", url+"/api/v1/allocation/targets",
		`{"dimension":"kind","targets":[{"key":"shares","percent":"50"},{"key":"bonds","percent":"40"},{"key":"metals","percent":"10"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("targets = %d", resp.StatusCode)
	}
	apitest.Decode(t, resp, &got)
	byKey := map[string]apitypes.AllocationRow{}
	for _, r := range got.Kinds {
		byKey[r.Key] = r
	}
	if r := byKey["shares"]; r.Share != "60.00" || r.Target.MustGet() != "50.00" || r.Gap.MustGet() != "10.00" || r.OffMinor.MustGet() != 100_000_00 {
		t.Errorf("shares = %+v", r)
	}
	if r := byKey["metals"]; r.Minor != 0 || r.OffMinor.MustGet() != -100_000_00 {
		t.Errorf("metals = %+v", r)
	}

	resp = apitest.Do(t, c, "PUT", url+"/api/v1/allocation/targets", `{"dimension":"currency","targets":[{"key":"RUB","percent":"70"},{"key":"CNY","percent":"30"}]}`)
	apitest.Decode(t, resp, &got)
	if len(got.Currencies) != 2 || got.Currencies[0].Key != "RUB" || got.Currencies[0].Gap.MustGet() != "30.00" || got.Currencies[1].Key != "CNY" {
		t.Errorf("currencies = %+v", got.Currencies)
	}

	if r := apitest.Do(t, c, "PUT", url+"/api/v1/allocation/mode", `{"mode":"sometimes"}`); r.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown mode = %d, want 400", r.StatusCode)
	}
	apitest.Decode(t, apitest.Do(t, c, "PUT", url+"/api/v1/allocation/mode", `{"mode":"rebalance"}`), &got)
	if got.Mode != "rebalance" {
		t.Errorf("mode = %s", got.Mode)
	}

	// At the full valuation, as «Структура» shows it: the frozen fund in.
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/allocation?valuation=full", ""), &got)
	if got.WorthMinor != 1_500_000_00 || len(got.Kinds) != 4 || got.Kinds[2].Key != "funds" {
		t.Errorf("full valuation: worth %d, kinds %+v", got.WorthMinor, got.Kinds)
	}

	// A top-up of 100 000 by the targets 50/40/10: shares are above theirs,
	// bonds and metals below.
	var topUp apitypes.AllocationTopUp
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/allocation/topup?amount_minor=10000000", ""), &topUp)
	if topUp.WorthAfterMinor != 1_100_000_00 || len(topUp.Kinds) != 3 || topUp.Kinds[0].BuyMinor != 0 || topUp.Kinds[1].BuyMinor+topUp.Kinds[2].BuyMinor != 100_000_00 {
		t.Errorf("top-up = %+v", topUp)
	}
	if r := apitest.Do(t, c, "GET", url+"/api/v1/allocation/topup?amount_minor=0", ""); r.StatusCode != http.StatusBadRequest {
		t.Errorf("a top-up of nothing = %d, want 400", r.StatusCode)
	}

	// Taken back: no targets, the rows as held.
	apitest.Decode(t, apitest.Do(t, c, "PUT", url+"/api/v1/allocation/targets", `{"dimension":"kind","targets":[]}`), &got)
	if len(got.Kinds) != 2 || !got.Kinds[0].Target.IsNull() || got.Mode != "rebalance" {
		t.Errorf("targets taken back: %+v", got)
	}
}
