package operation_test

import (
	"fmt"
	"io"
	"strings"
	"testing"
)

// TestConflictIsNotOnlyAnOversell pins the two facts any caption for this
// endpoint's 409 has to survive, because a client has nothing but the status to
// write that caption from (#23).
//
// FACT ONE: A BUY GETS 409. A purchase releases nothing — it can only add to a
// position — so a refusal of one cannot be "you sold more than you hold" under
// any reading. Here it is the currency rule that refuses (see Compute's get in
// internal/portfolio/engine.go: everything that moves cost, quantity or fees
// must repeat the currency the position's cost is already kept in, and only a
// dividend, a coupon or a tax may arrive in another).
//
// FACT TWO: THE ROW REFUSED NEED NOT BE THE ROW POSTED. Every write replays the
// account's WHOLE journal (Service.Create → checkJournalOps), so the entry the
// engine names can be one stored long ago. The backdated buy below is what makes
// that visible: sorted into the journal ahead of the row already there, it
// settles the position's currency itself, and the refusal then names the STORED
// operation and the STORED operation's date — neither of which the client sent.
// A caption saying anything about "this operation" or "this date" is therefore
// false here, and «Недостаточно бумаг на счете на эту дату» was both.
//
// FACT THREE, in the last leg: an ordinary oversell is the SAME 409. Two
// unrelated causes, one status, nothing to tell them apart by — which is the
// whole of why the screen names no cause (see operations.conflict in
// web/src/i18n/ru.json).
//
// This test is meant to go red if the server ever does learn to tell its
// conflicts apart — that would be the moment the screen may name one.
func TestConflictIsNotOnlyAnOversell(t *testing.T) {
	url, c := newAPI(t)

	resp := do(t, c, "POST", url+"/api/v1/accounts",
		`{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create account = %d: %s", resp.StatusCode, b)
	}
	var acc idResp
	decodeJSON(t, resp, &acc)

	resp = do(t, c, "POST", url+"/api/v1/instruments",
		`{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create instrument = %d: %s", resp.StatusCode, b)
	}
	var sber idResp
	decodeJSON(t, resp, &sber)

	// The row that settles the position's currency, and the one the refusal
	// below will name. Its date is deliberately the LATER of the two.
	stored := fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-10","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"RUB"}`, acc.ID, sber.ID)
	resp = do(t, c, "POST", url+"/api/v1/operations", stored)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("first buy = %d, want 201: %s", resp.StatusCode, b)
	}

	// A buy, in another currency, dated BEFORE the one above.
	posted := fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"5","price":"1",
		"amount_minor":-500,"currency":"USD"}`, acc.ID, sber.ID)
	resp = do(t, c, "POST", url+"/api/v1/operations", posted)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 409 {
		t.Fatalf("buy in a second currency = %d, want 409: %s", resp.StatusCode, body)
	}
	// The whole point of the two dates: the engine names the row it could not
	// fold, and that is the one already in the journal. Checked in both
	// directions — a message naming the posted date instead would mean the
	// refusal IS about what the client sent, and a caption could then say so.
	if !strings.Contains(string(body), "2026-07-10") {
		t.Fatalf("refusal does not name the stored row's date 2026-07-10: %s", body)
	}
	if strings.Contains(string(body), "2026-07-01") {
		t.Fatalf("refusal names the posted row's date 2026-07-01, so it is about the posted row: %s", body)
	}

	// And an oversell — a different cause entirely — comes back as the same
	// status, from the same endpoint, with nothing in the response to separate
	// the two.
	oversell := fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":"2026-07-20","quantity":"999","amount_minor":999000,"currency":"RUB"}`,
		acc.ID, sber.ID)
	resp = do(t, c, "POST", url+"/api/v1/operations", oversell)
	if resp.StatusCode != 409 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("oversell = %d, want 409: %s", resp.StatusCode, b)
	}
}
