package operation_test

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"babki.my/babki/internal/platform/apitest"
)

// A transfer with no instrument_id says the field is missing (#19), not
// "no source history for instrument", which names a different mistake. The old
// sentence must be absent too.
func TestTransferWithoutAnInstrumentNamesTheMissingField(t *testing.T) {
	url, c := newAPI(t)

	resp := apitest.Do(t, c, "POST", url+"/api/v1/accounts",
		`{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create acc1 = %d: %s", resp.StatusCode, b)
	}
	var acc1 idResp
	apitest.Decode(t, resp, &acc1)

	resp = apitest.Do(t, c, "POST", url+"/api/v1/accounts",
		`{"name":"Брокер 2","type":"brokerage","currency":"RUB"}`)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create acc2 = %d: %s", resp.StatusCode, b)
	}
	var acc2 idResp
	apitest.Decode(t, resp, &acc2)

	body := fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,
		"quantity":"4","occurred_on":"2026-07-05"}`, acc1.ID, acc2.ID)
	resp = apitest.Do(t, c, "POST", url+"/api/v1/operations/transfer", body)
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 400 {
		t.Fatalf("transfer with no instrument_id = %d, want 400: %s", resp.StatusCode, got)
	}
	if !strings.Contains(string(got), "instrument_id is required") {
		t.Errorf("refusal = %s, want it to name instrument_id as the missing field", got)
	}
	if strings.Contains(string(got), "no source history") {
		t.Errorf("refusal = %s, want it not to blame the source account's journal", got)
	}
}

// An account outside the space is a 404 for a missing account, not a
// search of a journal that does not exist.
func TestTransferFromAnAccountThatIsNotThereSaysSo(t *testing.T) {
	url, c := newAPI(t)

	resp := apitest.Do(t, c, "POST", url+"/api/v1/accounts",
		`{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create account = %d: %s", resp.StatusCode, b)
	}
	var acc idResp
	apitest.Decode(t, resp, &acc)

	resp = apitest.Do(t, c, "POST", url+"/api/v1/instruments",
		`{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create instrument = %d: %s", resp.StatusCode, b)
	}
	var sber idResp
	apitest.Decode(t, resp, &sber)

	body := fmt.Sprintf(`{"from_account_id":"11111111-1111-1111-1111-111111111111",
		"to_account_id":%q,"instrument_id":%q,"quantity":"4","occurred_on":"2026-07-05"}`,
		acc.ID, sber.ID)
	resp = apitest.Do(t, c, "POST", url+"/api/v1/operations/transfer", body)
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 404 {
		t.Fatalf("transfer from an unknown account = %d, want 404: %s", resp.StatusCode, got)
	}
	if strings.Contains(string(got), "no source history") {
		t.Errorf("refusal = %s, want it not to blame the source account's journal for an account that is not there", got)
	}
}

// occurred_on is held to both ends of its range on both write paths (#19).
// The floor is a typo guard: a mistyped year lands at the front of the
// acquisition-date queue and is sold first. Years are literals, so a moved bound
// is caught; 1900-01-01 is the first allowed day. Both ends are pinned because
// loosening either went unnoticed before.
func TestOccurredOnIsHeldToBothEndsOfItsRangeOnBothWritePaths(t *testing.T) {
	url, c := newAPI(t)

	resp := apitest.Do(t, c, "POST", url+"/api/v1/accounts",
		`{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create acc1 = %d: %s", resp.StatusCode, b)
	}
	var acc1 idResp
	apitest.Decode(t, resp, &acc1)

	resp = apitest.Do(t, c, "POST", url+"/api/v1/accounts",
		`{"name":"Брокер 2","type":"brokerage","currency":"RUB"}`)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create acc2 = %d: %s", resp.StatusCode, b)
	}
	var acc2 idResp
	apitest.Decode(t, resp, &acc2)

	resp = apitest.Do(t, c, "POST", url+"/api/v1/instruments",
		`{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create instrument = %d: %s", resp.StatusCode, b)
	}
	var sber idResp
	apitest.Decode(t, resp, &sber)

	buy := func(date string) (int, string) {
		t.Helper()
		body := fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
			"occurred_on":%q,"quantity":"10","price":"100",
			"amount_minor":-100000,"currency":"RUB"}`, acc1.ID, sber.ID, date)
		r := apitest.Do(t, c, "POST", url+"/api/v1/operations", body)
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}

	// The typo the floor exists for.
	status, got := buy("1026-07-01")
	if status != 400 {
		t.Fatalf("buy dated 1026-07-01 = %d, want 400: %s", status, got)
	}
	if !strings.Contains(got, "1900-01-01") {
		t.Errorf("refusal = %s, want it to name the earliest date accepted", got)
	}

	// The first allowed day is a genuine 201.
	if status, got := buy("1900-01-01"); status != 201 {
		t.Errorf("buy dated 1900-01-01 = %d, want 201: %s", status, got)
	}

	// Tomorrow in UTC is the last day inside the one day of slack (for
	// someone east of UTC); the day after is outside.
	tomorrow := time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")
	if status, got := buy(tomorrow); status != 201 {
		t.Errorf("buy dated tomorrow (%s) = %d, want 201: %s", tomorrow, status, got)
	}
	dayAfterTomorrow := time.Now().UTC().AddDate(0, 0, 2).Format("2006-01-02")
	status, got = buy(dayAfterTomorrow)
	if status != 400 {
		t.Fatalf("buy dated the day after tomorrow (%s) = %d, want 400: %s", dayAfterTomorrow, status, got)
	}
	if !strings.Contains(got, "future") {
		t.Errorf("refusal = %s, want it to say the date is in the future", got)
	}

	// The transfer endpoint's own refusal: the transfer is otherwise valid.
	transfer := fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,
		"instrument_id":%q,"quantity":"4","occurred_on":"1026-07-05"}`,
		acc1.ID, acc2.ID, sber.ID)
	resp = apitest.Do(t, c, "POST", url+"/api/v1/operations/transfer", transfer)
	got2, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 400 {
		t.Fatalf("transfer dated 1026-07-05 = %d, want 400: %s", resp.StatusCode, got2)
	}
	if !strings.Contains(string(got2), "1900-01-01") {
		t.Errorf("transfer refusal = %s, want it to name the earliest date accepted", got2)
	}

	transferFuture := fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,
		"instrument_id":%q,"quantity":"4","occurred_on":%q}`,
		acc1.ID, acc2.ID, sber.ID, dayAfterTomorrow)
	resp = apitest.Do(t, c, "POST", url+"/api/v1/operations/transfer", transferFuture)
	got3, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 400 {
		t.Fatalf("transfer dated the day after tomorrow = %d, want 400: %s", resp.StatusCode, got3)
	}
	if !strings.Contains(string(got3), "future") {
		t.Errorf("transfer refusal = %s, want it to say the date is in the future", got3)
	}
}
