package instrument_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/apitypes"
)

// A paper's class as the family sees it: the source's, then its own country
// and sector over it, then the source's again; what is refused. The
// family's own is its alone.
func TestAPapersClassIsSetByHand(t *testing.T) {
	url, c, store := newAPIWithCatalog(t)
	ctx := context.Background()
	fund, err := store.Create(ctx, instrument.Instrument{Type: instrument.TypeETF, Name: "Альфа-Капитал Управляемые облигации", Ticker: "AKMB", ISIN: "RU000A101PN3", Currency: "RUB"})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	if err := store.SetClass(ctx, fund.ID, instrument.Class{Country: "RU", Sector: instrument.SectorMunicipal, Source: instrument.ClassFromTInvest, Raw: "municipal equity", FetchedAt: at}); err != nil {
		t.Fatal(err)
	}
	path := url + "/api/v1/instruments/" + fund.ID.String() + "/class"
	var got apitypes.InstrumentClass
	apitest.Decode(t, apitest.Do(t, c, "GET", path, ""), &got)
	if got.Country != "RU" || got.Sector != "municipal" || got.SectorByHand || got.Source != "tinvest" || got.Raw != "municipal equity" ||
		!got.FetchedAt.MustGet().Equal(at) || len(got.Sectors) != len(instrument.Sectors) {
		t.Fatalf("from the broker = %+v", got)
	}

	apitest.Decode(t, apitest.Do(t, c, "PUT", path, `{"country":null,"sector":"funds_bonds"}`), &got)
	if got.Sector != "funds_bonds" || !got.SectorByHand || got.CountryByHand || got.Country != "RU" || got.SourceSector != "municipal" {
		t.Errorf("by hand = %+v", got)
	}
	seen, err := store.ClassesFor(ctx, uuid.New(), []uuid.UUID{fund.ID})
	if err != nil || seen[fund.ID].Sector != instrument.SectorMunicipal || seen[fund.ID].SectorByHand {
		t.Errorf("another family sees %+v, %v", seen[fund.ID], err)
	}

	for _, bad := range []string{`{"country":"USA","sector":null}`, `{"country":null,"sector":"banks"}`} {
		if r := apitest.Do(t, c, "PUT", path, bad); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, r.StatusCode)
		}
	}
	if r := apitest.Do(t, c, "GET", url+"/api/v1/instruments/"+uuid.NewString()+"/class", ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("no such paper = %d, want 404", r.StatusCode)
	}

	apitest.Decode(t, apitest.Do(t, c, "PUT", path, `{"country":null,"sector":null}`), &got)
	if got.Sector != "municipal" || got.SectorByHand {
		t.Errorf("back to the broker's = %+v", got)
	}
}
