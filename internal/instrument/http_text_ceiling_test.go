package instrument_test

import (
	"net/http"
	"strings"
	"testing"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/platform/apitest"
)

// A hand-made catalog row's name, ticker and FIGI are bounded in characters,
// at creation and at the PATCH alike.
func TestAnInstrumentsTextsHaveACeiling(t *testing.T) {
	url, c := newAPI(t)
	name := strings.Repeat("ж", instrument.MaxNameRunes)
	ticker := strings.Repeat("Ж", instrument.MaxTickerRunes)
	figi := strings.Repeat("Ж", instrument.MaxFIGIRunes)
	create := func(name, ticker, figi string) *http.Response {
		return apitest.Do(t, c, "POST", url+"/api/v1/instruments",
			`{"type":"share","name":"`+name+`","ticker":"`+ticker+`","figi":"`+figi+`","currency":"RUB"}`)
	}

	for field, resp := range map[string]*http.Response{
		"name":   create(name+"ж", "T", "F"),
		"ticker": create("N", ticker+"Ж", "F"),
		"figi":   create("N", "T", figi+"Ж"),
	} {
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("create with a %s one character too long = %d, want 400", field, resp.StatusCode)
		}
	}
	resp := create(name, ticker, figi)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create with every text at its ceiling = %d, want 201", resp.StatusCode)
	}

	id := mkShare(t, url, c)
	for field, body := range map[string]string{
		"name":   `{"name":"` + name + `ж"}`,
		"ticker": `{"ticker":"` + ticker + `Ж"}`,
		"figi":   `{"figi":"` + figi + `Ж"}`,
	} {
		if resp := apitest.Do(t, c, "PATCH", url+"/api/v1/instruments/"+id, body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("update with a %s one character too long = %d, want 400", field, resp.StatusCode)
		}
	}
}
