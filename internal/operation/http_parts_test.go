package operation_test

import (
	"fmt"
	"net/http"
	"testing"

	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/apitypes"
)

// A spending is split across spending categories adding up to it (decision
// Р-36); the journal finds it by a part's category; a new amount drops the
// parts, and the row can be made one category's again.
func TestARowIsSplitAcrossCategories(t *testing.T) {
	url, c := newAPI(t)
	cats := categoryIDs(t, url, c)
	card := mkAccountOfType(t, url, c, "Карта", "checking")
	row := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"withdrawal","occurred_on":"2026-10-09","amount_minor":-234090,"currency":"RUB","category_id":%q}`,
		card, cats["expense/Продукты"]))
	path := url + "/api/v1/operations/" + row + "/parts"
	parts := func(items ...string) string { return fmt.Sprintf(`{"parts":[%s]}`, join(items)) }
	part := func(cat string, amount int64) string {
		return fmt.Sprintf(`{"category_id":%q,"amount_minor":%d}`, cats[cat], amount)
	}

	for name, body := range map[string]string{
		"one part":           parts(part("expense/Продукты", 234_090)),
		"not adding up":      parts(part("expense/Продукты", 150_000), part("expense/Дом", 50_000)),
		"an income one":      parts(part("expense/Продукты", 150_000), part("income/Зарплата", 84_090)),
		"a zero part":        parts(part("expense/Продукты", 234_090), part("expense/Дом", 0)),
		"no category at all": `{"parts":[{"category_id":"00000000-0000-0000-0000-000000000000","amount_minor":1},{"category_id":"00000000-0000-0000-0000-000000000000","amount_minor":234089}]}`,
	} {
		if r := apitest.Do(t, c, "PUT", path, body); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, r.StatusCode)
		}
	}
	resp := apitest.Do(t, c, "PUT", path, parts(part("expense/Продукты", 169_092), part("expense/Дом", 64_998)))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("split = %d", resp.StatusCode)
	}
	var got apitypes.Operation
	apitest.Decode(t, resp, &got)
	if len(got.Parts) != 2 || got.Parts[1].CategoryId.String() != cats["expense/Дом"] || got.Parts[1].AmountMinor != 64_998 {
		t.Fatalf("parts = %+v", got.Parts)
	}

	// The journal of «Дом» finds the row by its part.
	var page apitypes.OperationsResponse
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/accounts/"+card+"/operations?limit=10&category="+cats["expense/Дом"], ""), &page)
	if len(page.Operations) != 1 || page.Operations[0].Id.String() != row || len(page.Operations[0].Parts) != 2 {
		t.Errorf("by a part's category = %+v", page.Operations)
	}

	// A new amount: the parts no longer add up and are gone.
	upd := fmt.Sprintf(`{"account_id":%q,"type":"withdrawal","occurred_on":"2026-10-09","amount_minor":-240000,"currency":"RUB","category_id":%q}`, card, cats["expense/Продукты"])
	resp = apitest.Do(t, c, "PUT", url+"/api/v1/operations/"+row, upd)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update = %d", resp.StatusCode)
	}
	apitest.Decode(t, resp, &got)
	if len(got.Parts) != 0 {
		t.Errorf("after a new amount parts = %+v", got.Parts)
	}

	// Split again and made whole.
	if r := apitest.Do(t, c, "PUT", path, parts(part("expense/Продукты", 200_000), part("expense/Дом", 40_000))); r.StatusCode != http.StatusOK {
		t.Fatalf("split again = %d", r.StatusCode)
	}
	resp = apitest.Do(t, c, "DELETE", path, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear = %d", resp.StatusCode)
	}
	apitest.Decode(t, resp, &got)
	if len(got.Parts) != 0 || got.CategoryId.MustGet().String() != cats["expense/Продукты"] {
		t.Errorf("made whole = %+v", got)
	}
}

func join(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}
