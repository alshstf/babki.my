package allocation_test

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/allocation"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/portfolio"
	"babki.my/babki/internal/structure"
)

// fakeAccounts is one broker's account kept by its journal.
type fakeAccounts struct{}

func (fakeAccounts) ListWithBalance(context.Context, uuid.UUID) ([]account.WithBalance, error) {
	return []account.WithBalance{{Account: account.Account{ID: uuid.New(), Type: account.TypeBrokerage, Status: account.StatusActive}}}, nil
}

// fakeSales is the shares of fakeStructures: 100 of one paper worth 600 000
// now, bought in January 2025 for 500 000.
type fakeSales struct{}

func (fakeSales) ForSale(context.Context, uuid.UUID, uuid.UUID) ([]portfolio.SaleLots, error) {
	worth, on := int64(600_000_00), time.Date(2025, 1, 10, 0, 0, 0, 0, time.UTC)
	return []portfolio.SaleLots{{
		InstrumentID: uuid.New(), Type: instrument.TypeShare, Quantity: decimal.NewFromInt(100), LiquidInBase: &worth,
		Lots: []portfolio.HeldLot{{Quantity: decimal.NewFromInt(100), AcquiredOn: &on, CostInBase: 500_000_00, CostKnown: true}},
	}}, nil
}

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
	allocation.NewHandler(allocation.NewService(pool, fakeStructures{}).WithSales(fakeAccounts{}, fakeSales{}, famStore), auth, sm).Mount(srv)
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
	if topUp.WorthAfterMinor != 1_100_000_00 || len(topUp.Kinds) != 3 || topUp.Kinds[0].ChangeMinor != 0 || topUp.Kinds[1].ChangeMinor+topUp.Kinds[2].ChangeMinor != 100_000_00 {
		t.Errorf("top-up = %+v", topUp)
	}
	if r := apitest.Do(t, c, "GET", url+"/api/v1/allocation/topup?amount_minor=0", ""); r.StatusCode != http.StatusBadRequest {
		t.Errorf("a top-up of nothing = %d, want 400", r.StatusCode)
	}

	// Bought and sold to 50/40/10: a sixth of the shares goes, 100 000, which
	// bought for 83 333,33 gains 16 666,67 — 13 % of it in tax, the lot being
	// under three years old.
	var re apitypes.AllocationRebalance
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/allocation/rebalance", ""), &re)
	if re.WorthAfterMinor != 1_000_000_00 || len(re.Kinds) != 3 || re.Kinds[0].ChangeMinor != -100_000_00 || re.Kinds[1].ChangeMinor != 0 || re.Kinds[2].ChangeMinor != 100_000_00 {
		t.Fatalf("rebalance = %+v", re)
	}
	sale := re.Kinds[0].Sale.MustGet()
	if !sale.Known || sale.GainMinor != 1_666_667 || sale.FreeMinor != 0 || re.TaxMinor.MustGet() != 216_667 || re.TaxRate.MustGet() != "13" || re.LongHoldingYears != 3 {
		t.Errorf("the estimate = %+v, tax %v at %v", sale, re.TaxMinor, re.TaxRate)
	}
	if !re.Kinds[2].Sale.IsNull() {
		t.Errorf("a kind bought has no sale: %+v", re.Kinds[2])
	}
	if r := apitest.Do(t, c, "GET", url+"/api/v1/allocation/rebalance?amount_minor=-1", ""); r.StatusCode != http.StatusBadRequest {
		t.Errorf("new money below zero = %d, want 400", r.StatusCode)
	}

	// Taken back: no targets, the rows as held.
	apitest.Decode(t, apitest.Do(t, c, "PUT", url+"/api/v1/allocation/targets", `{"dimension":"kind","targets":[]}`), &got)
	if len(got.Kinds) != 2 || !got.Kinds[0].Target.IsNull() || got.Mode != "rebalance" {
		t.Errorf("targets taken back: %+v", got)
	}
}
