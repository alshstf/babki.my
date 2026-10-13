package allocation_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/allocation"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/portfolio"
)

var (
	sharePaper = instrument.Instrument{ID: uuid.New(), Type: instrument.TypeShare, Name: "Сбербанк", ISIN: "RU0009029540", Currency: "RUB"}
	bondPaper  = instrument.Instrument{ID: uuid.New(), Type: instrument.TypeBond, Name: "ОФЗ 26238", ISIN: "RU000A1038V6", Currency: "RUB"}
	usPaper    = instrument.Instrument{ID: uuid.New(), Type: instrument.TypeETF, Name: "Invesco NASDAQ 100", ISIN: "US46138G6492", Currency: "RUB"}
)

type fakeCatalog map[uuid.UUID]instrument.Instrument

func (f fakeCatalog) ByID(_ context.Context, id uuid.UUID) (instrument.Instrument, error) {
	i, ok := f[id]
	if !ok {
		return instrument.Instrument{}, fmt.Errorf("%w: no paper", family.ErrValidation)
	}
	return i, nil
}

type noRates struct{}

func (noRates) Rate(context.Context, string, string, time.Time) (decimal.Decimal, time.Time, error) {
	return decimal.Zero, time.Time{}, marketdata.ErrNoRate
}

func (noRates) RatesOn(context.Context, []marketdata.RateQuery) (marketdata.Rates, error) {
	return marketdata.Rates{}, nil
}

// whatIfSales is the account's Сбербанк: 100 shares worth 600 000, bought
// in January 2025 for 500 000.
type whatIfSales struct{}

func (whatIfSales) ForSale(context.Context, uuid.UUID, uuid.UUID) ([]portfolio.SaleLots, error) {
	worth, on := int64(600_000_00), time.Date(2025, 1, 10, 0, 0, 0, 0, time.UTC)
	return []portfolio.SaleLots{{
		InstrumentID: sharePaper.ID, Type: instrument.TypeShare, Quantity: decimal.NewFromInt(100), LiquidInBase: &worth,
		Lots: []portfolio.HeldLot{{Quantity: decimal.NewFromInt(100), AcquiredOn: &on, CostInBase: 500_000_00, CostKnown: true}},
	}}, nil
}

// A plan against targets of half shares, half bonds and a ceiling of 20 %
// on the United States: selling 100 000 of shares and buying the bond brings
// both kinds closer; putting in 50 000 for an American fund takes the
// country further above its ceiling. The sale gains a sixth of 100 000 over
// cost — 16 667 — taxed at 13 %. A plan that is no plan is refused.
func TestAPlanIsSetAgainstTheTargets(t *testing.T) {
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	svc := allocation.NewService(pool, fakeStructures{}).WithSales(fakeAccounts{}, whatIfSales{}, famStore).
		WithCatalog(fakeCatalog{sharePaper.ID: sharePaper, bondPaper.ID: bondPaper, usPaper.ID: usPaper}, noRates{})
	allocation.NewHandler(svc, auth, sm).Mount(srv)
	url, c := apitest.Serve(t, srv.Handler())
	for _, body := range []string{
		`{"dimension":"kind","targets":[{"key":"shares","percent":"50"},{"key":"bonds","percent":"50"}]}`,
		`{"dimension":"country","targets":[{"key":"US","percent":"20"}]}`,
	} {
		if r := apitest.Do(t, c, "PUT", url+"/api/v1/allocation/targets", body); r.StatusCode >= 300 {
			t.Fatalf("%s = %d", body, r.StatusCode)
		}
	}
	acc := uuid.NewString()
	plan := fmt.Sprintf(`{"operations":[
		{"kind":"sell","account_id":%[1]q,"instrument_id":%[2]q,"currency":"RUB","amount_minor":10000000},
		{"kind":"buy","account_id":%[1]q,"instrument_id":%[3]q,"currency":"RUB","amount_minor":10000000},
		{"kind":"deposit","account_id":%[1]q,"currency":"RUB","amount_minor":5000000},
		{"kind":"buy","account_id":%[1]q,"instrument_id":%[4]q,"currency":"RUB","amount_minor":5000000}]}`,
		acc, sharePaper.ID, bondPaper.ID, usPaper.ID)
	var got apitypes.AllocationWhatIf
	apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/allocation/what-if", plan), &got)
	row := func(dim, key string) apitypes.AllocationCutRow {
		for _, cut := range got.Cuts {
			if string(cut.Dimension) != dim {
				continue
			}
			for _, r := range cut.Rows {
				if r.Key == key {
					return r
				}
			}
		}
		t.Fatalf("no %s %s in %+v", dim, key, got.Cuts)
		return apitypes.AllocationCutRow{}
	}
	if r := row("kind", "shares"); r.BeforeMinor != 600_000_00 || r.AfterMinor != 500_000_00 || r.Verdict != "closer" {
		t.Errorf("shares = %+v", r)
	}
	if r := row("kind", "bonds"); r.AfterMinor != 500_000_00 || r.Verdict != "closer" {
		t.Errorf("bonds = %+v", r)
	}
	if r := row("country", "US"); r.ShareBefore != "25.00" || r.ShareAfter != "28.57" || r.Target.MustGet() != "20.00" || r.Verdict != "further" {
		t.Errorf("US = %+v", r)
	}
	if r := row("country", "RU"); r.Verdict != "none" {
		t.Errorf("RU = %+v", r)
	}
	if len(got.Sales) != 1 || got.Sales[0].InstrumentId != sharePaper.ID || got.Sales[0].GainMinor != 16_666_67 || !got.Sales[0].Known ||
		got.TaxMinor.MustGet() != 2_166_67 || got.TaxRate.MustGet() != "13" {
		t.Errorf("sales %+v, tax %v at %v", got.Sales, got.TaxMinor, got.TaxRate)
	}

	for _, bad := range []string{
		`{"operations":[]}`,
		fmt.Sprintf(`{"operations":[{"kind":"buy","account_id":%q,"currency":"RUB","amount_minor":100}]}`, acc),
		fmt.Sprintf(`{"operations":[{"kind":"deposit","account_id":%q,"currency":"RUB","amount_minor":0}]}`, acc),
		fmt.Sprintf(`{"operations":[{"kind":"transfer","account_id":%[1]q,"to_account_id":%[1]q,"currency":"RUB","amount_minor":100}]}`, acc),
		fmt.Sprintf(`{"operations":[{"kind":"steal","account_id":%q,"currency":"RUB","amount_minor":100}]}`, acc),
	} {
		if r := apitest.Do(t, c, "POST", url+"/api/v1/allocation/what-if", bad); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, r.StatusCode)
		}
	}
}
