package compare_test

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/compare"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/structure"
	"babki.my/babki/internal/taxyear"
)

type noStructure struct{}

func (noStructure) Of(context.Context, uuid.UUID, structure.Valuation) (structure.Structure, error) {
	return structure.Structure{InsuredLimit: 1_400_000_00}, nil
}

type noTaxes struct{}

func (noTaxes) Of(context.Context, uuid.UUID, int) (taxyear.Year, error) {
	return taxyear.Year{}, taxyear.ErrNoEstimate
}

// Over HTTP: 1 000 000 ₽ for a year at a key rate of 12 % set — rows by what
// is left, the key taken, a choice of a condition kept on its row; a horizon
// of five years and a month is refused.
func TestTheComparisonOverHTTP(t *testing.T) {
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	accStore, md := account.NewStore(pool), marketdata.NewStore(pool)
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	compare.NewHandler(compare.NewService(md, nil, accStore, noStructure{}, noTaxes{}, famStore, famStore), auth, sm).Mount(srv)
	url, c := apitest.Serve(t, srv.Handler())
	if err := md.UpsertKeyRates(t.Context(), []marketdata.KeyRate{{On: time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC), Rate: decimal.NewFromInt(14), Source: "test"}}); err != nil {
		t.Fatal(err)
	}

	body := `{"amount_minor":100000000,"months":12,"key_percent":"12","preset":"no",
		"choices":[{"programme_id":"vtb-schet","variant_id":"minimum","condition":"salary","state":"yes"}]}`
	var got apitypes.DepositComparison
	apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/deposit-comparison", body), &got)
	if got.KeyPercent != "12" || len(got.Rows) < 30 || got.Rows[0].NetMinor < got.Rows[len(got.Rows)-1].NetMinor {
		t.Fatalf("comparison: key %s, %d rows", got.KeyPercent, len(got.Rows))
	}
	found := false
	for _, r := range got.Rows {
		if r.ProgrammeId != "vtb-schet" || r.VariantId != "minimum" {
			continue
		}
		found = true
		for _, cond := range r.Conditions {
			want := cond.Id == "salary"
			if cond.Met != want || (cond.State == apitypes.DepositComparisonConditionStateYes) != want {
				t.Errorf("%s: met %v, state %s", cond.Id, cond.Met, cond.State)
			}
		}
		if r.InsuredMinor.MustGet() != 1_000_000_00 || !r.TaxMinor.IsNull() {
			t.Errorf("insured %v, tax %v", r.InsuredMinor, r.TaxMinor)
		}
	}
	if !found {
		t.Error("no ВТБ-Счёт on the minimal balance")
	}
	if r := apitest.Do(t, c, "POST", url+"/api/v1/deposit-comparison", `{"amount_minor":100000000,"months":61}`); r.StatusCode != http.StatusBadRequest {
		t.Errorf("61 months = %d, want 400", r.StatusCode)
	}
}
