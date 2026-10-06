package operation_test

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"babki.my/babki/internal/platform/apitest"
)

// What a client may say about a 409 (#23):
//
//   - A buy can get one: here the currency rule refuses it.
//   - The refused row need not be the posted one: every write replays the whole
//     journal, and this backdated buy makes a stored row fail, named with its own
//     date.
//   - An ordinary oversell gets the same 409.
//
// So the screen names no cause (operations.conflict in web/src/i18n/ru.json).
// This test should go red if the server learns to tell its conflicts apart.
func TestConflictIsNotOnlyAnOversell(t *testing.T) {
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

	// The row that settles the position's currency, and the one the refusal
	// below will name. Its date is deliberately the LATER of the two.
	stored := fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-10","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"RUB"}`, acc.ID, sber.ID)
	resp = apitest.Do(t, c, "POST", url+"/api/v1/operations", stored)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("first buy = %d, want 201: %s", resp.StatusCode, b)
	}

	// A buy, in another currency, dated BEFORE the one above.
	posted := fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"5","price":"1",
		"amount_minor":-500,"currency":"USD"}`, acc.ID, sber.ID)
	resp = apitest.Do(t, c, "POST", url+"/api/v1/operations", posted)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 409 {
		t.Fatalf("buy in a second currency = %d, want 409: %s", resp.StatusCode, body)
	}
	// The engine names the stored row and its date, not the posted one.
	if !strings.Contains(string(body), "2026-07-10") {
		t.Fatalf("refusal does not name the stored row's date 2026-07-10: %s", body)
	}
	if strings.Contains(string(body), "2026-07-01") {
		t.Fatalf("refusal names the posted row's date 2026-07-01, so it is about the posted row: %s", body)
	}

	// An oversell: the same status, nothing to tell them apart.
	oversell := fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":"2026-07-20","quantity":"999","amount_minor":999000,"currency":"RUB"}`,
		acc.ID, sber.ID)
	resp = apitest.Do(t, c, "POST", url+"/api/v1/operations", oversell)
	if resp.StatusCode != 409 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("oversell = %d, want 409: %s", resp.StatusCode, b)
	}
}
