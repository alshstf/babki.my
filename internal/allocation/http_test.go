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
		Papers:           1_000_000_00,
		ByCountry:        []structure.Slice{{Key: "RU", Minor: 750_000_00}, {Key: "US", Minor: 250_000_00}, {Key: "", Minor: 100_000_00}},
		BySector:         []structure.Slice{{Key: "it", Minor: 350_000_00}, {Key: "government", Minor: 400_000_00}, {Key: "financials", Minor: 250_000_00}},
		AtBrokers:        1_000_000_00,
		ByBroker:         []structure.Slice{{Key: "tinvest", Minor: 700_000_00}, {Key: "freedom", Minor: 300_000_00}},
		AtBanks:          2_000_000_00,
		ByBank:           []structure.Slice{{Key: "tbank", Minor: 1_900_000_00}, {Key: "sber", Minor: 100_000_00}},
	}
	if valuation == structure.Full {
		s.Targets += 500_000_00
		s.ByKind = append(s.ByKind, structure.Slice{Key: "funds", Minor: 500_000_00})
		s.ByTargetCurrency[0].Minor += 500_000_00
	}
	return s, nil
}

// OfWith lays the changes on the worth the plain way: a paper by its kind,
// its ISIN's country and an unknown sector, money as the brokers' roubles.
func (f fakeStructures) OfWith(ctx context.Context, spaceID uuid.UUID, valuation structure.Valuation, changes []structure.Change) (structure.Structure, error) {
	s, _ := f.Of(ctx, spaceID, valuation)
	add := func(list []structure.Slice, key string, minor int64) []structure.Slice {
		for i := range list {
			if list[i].Key == key {
				list[i].Minor += minor
				return list
			}
		}
		return append(list, structure.Slice{Key: key, Minor: minor})
	}
	for _, c := range changes {
		s.Targets += c.Minor
		s.ByTargetCurrency = add(s.ByTargetCurrency, c.Currency, c.Minor)
		if c.Paper == nil {
			s.ByKind = add(s.ByKind, string(structure.KindBrokerCash), c.Minor)
			continue
		}
		s.ByKind = add(s.ByKind, string(structure.PaperKind(c.Type)), c.Minor)
		s.Papers += c.Minor
		s.ByCountry = add(s.ByCountry, c.ISIN[:2], c.Minor)
		s.BySector = add(s.BySector, structure.UnknownSector, c.Minor)
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
	if re.SoldMinor != 100_000_00 || re.FromBrokersMinor != 0 || re.BoughtMinor != 100_000_00 {
		t.Errorf("sold %d, from brokers %d, bought %d", re.SoldMinor, re.FromBrokersMinor, re.BoughtMinor)
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

// Ceilings by country and sector, each on its own (decision Р-46): the papers
// against them, what is above; a ceiling of a country not two letters or of
// no sector is refused, ceilings not adding up to 100 are not.
func TestCeilingsOverHTTP(t *testing.T) {
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	allocation.NewHandler(allocation.NewService(pool, fakeStructures{}), auth, sm).Mount(srv)
	url, c := apitest.Serve(t, srv.Handler())

	put := func(body string) *http.Response {
		return apitest.Do(t, c, "PUT", url+"/api/v1/allocation/targets", body)
	}
	if r := put(`{"dimension":"country","targets":[{"key":"US","percent":"20"}]}`); r.StatusCode != http.StatusNoContent && r.StatusCode != http.StatusOK {
		t.Fatalf("country ceiling = %d", r.StatusCode)
	}
	if r := put(`{"dimension":"sector","targets":[{"key":"it","percent":"30"},{"key":"financials","percent":"30"}]}`); r.StatusCode != http.StatusNoContent && r.StatusCode != http.StatusOK {
		t.Fatalf("sector ceilings = %d", r.StatusCode)
	}
	var got apitypes.Allocation
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/allocation", ""), &got)
	row := func(rows []apitypes.AllocationRow, key string) apitypes.AllocationRow {
		for _, r := range rows {
			if r.Key == key {
				return r
			}
		}
		t.Fatalf("no %s in %+v", key, rows)
		return apitypes.AllocationRow{}
	}
	if got.PapersMinor != 1_000_000_00 || len(got.Countries) != 2 {
		t.Fatalf("countries = %+v", got.Countries)
	}
	if us := row(got.Countries, "US"); us.Share != "25.00" || us.Target.MustGet() != "20.00" || us.OffMinor.MustGet() != 50_000_00 {
		t.Errorf("US = %+v", us)
	}
	if ru := row(got.Countries, "RU"); !ru.Target.IsNull() {
		t.Errorf("RU has no ceiling: %+v", ru)
	}
	if it := row(got.Sectors, "it"); it.Share != "35.00" || it.OffMinor.MustGet() != 50_000_00 {
		t.Errorf("IT = %+v", it)
	}
	if fin := row(got.Sectors, "financials"); fin.OffMinor.MustGet() != -50_000_00 {
		t.Errorf("financials = %+v", fin)
	}
	for _, bad := range []string{
		`{"dimension":"country","targets":[{"key":"USA","percent":"20"}]}`,
		`{"dimension":"sector","targets":[{"key":"banks","percent":"20"}]}`,
		`{"dimension":"sector","targets":[{"key":"it","percent":"120"}]}`,
	} {
		if r := put(bad); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, r.StatusCode)
		}
	}
}

// Ceilings by broker and bank (decision Р-47): what each holds against its
// ceiling, of what the brokers or the banks hold; a bank's id as a broker's,
// or one not in the catalog, is refused.
func TestBrokerAndBankCeilingsOverHTTP(t *testing.T) {
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	allocation.NewHandler(allocation.NewService(pool, fakeStructures{}), auth, sm).Mount(srv)
	url, c := apitest.Serve(t, srv.Handler())
	put := func(body string) *http.Response {
		return apitest.Do(t, c, "PUT", url+"/api/v1/allocation/targets", body)
	}
	for _, body := range []string{
		`{"dimension":"broker","targets":[{"key":"tinvest","percent":"50"}]}`,
		`{"dimension":"bank","targets":[{"key":"tbank","percent":"70"}]}`,
	} {
		if r := put(body); r.StatusCode >= 300 {
			t.Fatalf("%s = %d", body, r.StatusCode)
		}
	}
	var got apitypes.Allocation
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/allocation", ""), &got)
	if got.AtBrokersMinor != 1_000_000_00 || got.AtBanksMinor != 2_000_000_00 || len(got.Brokers) != 2 || len(got.Banks) != 2 {
		t.Fatalf("brokers %+v, banks %+v", got.Brokers, got.Banks)
	}
	if b := got.Brokers[0]; b.Key != "tinvest" || b.Share != "70.00" || b.OffMinor.MustGet() != 200_000_00 {
		t.Errorf("tinvest = %+v", b)
	}
	if b := got.Banks[0]; b.Key != "tbank" || b.Share != "95.00" || b.OffMinor.MustGet() != 500_000_00 {
		t.Errorf("tbank = %+v", b)
	}
	for _, bad := range []string{
		`{"dimension":"broker","targets":[{"key":"tbank","percent":"50"}]}`,
		`{"dimension":"bank","targets":[{"key":"nobank","percent":"50"}]}`,
	} {
		if r := put(bad); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, r.StatusCode)
		}
	}
}
