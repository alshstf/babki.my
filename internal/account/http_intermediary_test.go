package account_test

import (
	"net/http"
	"testing"

	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/apitypes"
)

// An account's bank or broker (decision Р-47): found by its «bank or broker»
// name — a former one too; picked by hand over it; found by name again when
// the pick is dropped; an id the catalog does not have is refused.
func TestAnAccountsIntermediary(t *testing.T) {
	url, c := newAPI(t)
	var card, broker apitypes.AccountWithBalance
	apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/accounts", `{"name":"Карта","type":"checking","currency":"RUB","institution":"Тинькофф"}`), &card)
	apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/accounts", `{"name":"Брокер","type":"brokerage","currency":"RUB","institution":"Тинькофф Инвестиции"}`), &broker)
	if card.IntermediaryId.MustGet() != "tbank" || *card.IntermediaryByHand {
		t.Errorf("card = %v, %v", card.IntermediaryId, card.IntermediaryByHand)
	}
	if broker.IntermediaryId.MustGet() != "tinvest" {
		t.Errorf("broker = %v", broker.IntermediaryId)
	}

	path := url + "/api/v1/accounts/" + card.Id.String()
	var got apitypes.AccountWithBalance
	apitest.Decode(t, apitest.Do(t, c, "PATCH", path, `{"intermediary_id":"alfa"}`), &got)
	if got.IntermediaryId.MustGet() != "alfa" || !*got.IntermediaryByHand {
		t.Errorf("picked = %v, %v", got.IntermediaryId, got.IntermediaryByHand)
	}
	apitest.Decode(t, apitest.Do(t, c, "PATCH", path, `{"intermediary_id":null}`), &got)
	if got.IntermediaryId.MustGet() != "tbank" || *got.IntermediaryByHand {
		t.Errorf("dropped = %v, %v", got.IntermediaryId, got.IntermediaryByHand)
	}
	if r := apitest.Do(t, c, "PATCH", path, `{"intermediary_id":"nobank"}`); r.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown one = %d, want 400", r.StatusCode)
	}
	var unknown apitypes.AccountWithBalance
	apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/accounts", `{"name":"Кошелёк","type":"cash","currency":"RUB","institution":""}`), &unknown)
	if !unknown.IntermediaryId.IsNull() || unknown.IntermediaryByHand != nil {
		t.Errorf("cash = %v, %v", unknown.IntermediaryId, unknown.IntermediaryByHand)
	}
}
