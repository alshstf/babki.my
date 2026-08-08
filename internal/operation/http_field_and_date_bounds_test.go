package operation_test

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// TestTransferWithoutAnInstrumentNamesTheMissingField pins the sentence a
// transfer request with no instrument_id comes back with (#19).
//
// It used to come back «no source history for instrument», which is the message
// for a DIFFERENT and perfectly plausible mistake: you asked to move a paper
// this account has never held. A reader given that sentence goes and reads the
// source account's journal — which is fine — instead of the one field they left
// out. The status was 400 either way; only the sentence was wrong, and a wrong
// sentence over a right number is the failure this project keeps finding.
//
// Both halves are asserted, and the negative half is the load-bearing one: a
// check that only looked for the new wording would stay green if the old
// sentence were appended to it.
func TestTransferWithoutAnInstrumentNamesTheMissingField(t *testing.T) {
	url, c := newAPI(t)

	resp := do(t, c, "POST", url+"/api/v1/accounts",
		`{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create acc1 = %d: %s", resp.StatusCode, b)
	}
	var acc1 idResp
	decodeJSON(t, resp, &acc1)

	resp = do(t, c, "POST", url+"/api/v1/accounts",
		`{"name":"Брокер 2","type":"brokerage","currency":"RUB"}`)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create acc2 = %d: %s", resp.StatusCode, b)
	}
	var acc2 idResp
	decodeJSON(t, resp, &acc2)

	body := fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,
		"quantity":"4","occurred_on":"2026-07-05"}`, acc1.ID, acc2.ID)
	resp = do(t, c, "POST", url+"/api/v1/operations/transfer", body)
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

// TestTransferFromAnAccountThatIsNotThereSaysSo is the same lesson one field
// along, and it arrived with the journal lock (#17): the write path now settles
// which accounts it is about BEFORE it reads anything, so an account id that is
// not this space's is answered as a missing account — 404 — rather than by
// searching a journal that does not exist.
//
// It used to be answered with «no source history for instrument», 400: a
// sentence about a paper the account has never held, said about an account that
// is not there at all. A reader given that goes looking through a journal for a
// row that was never the problem, exactly as in #19.
func TestTransferFromAnAccountThatIsNotThereSaysSo(t *testing.T) {
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

	body := fmt.Sprintf(`{"from_account_id":"11111111-1111-1111-1111-111111111111",
		"to_account_id":%q,"instrument_id":%q,"quantity":"4","occurred_on":"2026-07-05"}`,
		acc.ID, sber.ID)
	resp = do(t, c, "POST", url+"/api/v1/operations/transfer", body)
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 404 {
		t.Fatalf("transfer from an unknown account = %d, want 404: %s", resp.StatusCode, got)
	}
	if strings.Contains(string(got), "no source history") {
		t.Errorf("refusal = %s, want it not to blame the source account's journal for an account that is not there", got)
	}
}

// TestOccurredOnIsHeldToBothEndsOfItsRangeOnBothWritePaths pins the dates an
// operation may carry, on the ordinary write path and on the transfer one
// alike (#19).
//
// WHY A FLOOR AT ALL, given that a date nine centuries old harms nothing by
// being old: this journal's queue is ordered by acquisition date, so the row
// does not sit somewhere visibly odd — it sits at the FRONT, and the next sale
// releases it first and reports a cost basis built from it. Nothing on any
// screen remarks on a strange date, because to a comparison there is nothing
// strange about one. A mistyped leading digit (1026 for 2026) is one keystroke.
//
// The year below is written as a literal rather than derived from the bound the
// code holds — the whole point of a bound test is to disagree with a bound that
// has been moved. Same for the year in the accepted case: 1900-01-01 is the
// first date on the allowed side, and asserting that it passes is what tells a
// floor apart from a floor set one day too high.
//
// THE CEILING IS HERE BECAUSE NOTHING IN THIS PACKAGE COVERED IT. It has been
// enforced since the beginning and was checked twice, once per write path;
// measured by loosening it a year, every test in internal/operation stayed
// green. Now that both ends are one function, an accident to either would go
// unnoticed just as easily, so both ends are pinned in one place.
func TestOccurredOnIsHeldToBothEndsOfItsRangeOnBothWritePaths(t *testing.T) {
	url, c := newAPI(t)

	resp := do(t, c, "POST", url+"/api/v1/accounts",
		`{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create acc1 = %d: %s", resp.StatusCode, b)
	}
	var acc1 idResp
	decodeJSON(t, resp, &acc1)

	resp = do(t, c, "POST", url+"/api/v1/accounts",
		`{"name":"Брокер 2","type":"brokerage","currency":"RUB"}`)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create acc2 = %d: %s", resp.StatusCode, b)
	}
	var acc2 idResp
	decodeJSON(t, resp, &acc2)

	resp = do(t, c, "POST", url+"/api/v1/instruments",
		`{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create instrument = %d: %s", resp.StatusCode, b)
	}
	var sber idResp
	decodeJSON(t, resp, &sber)

	buy := func(date string) (int, string) {
		t.Helper()
		body := fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
			"occurred_on":%q,"quantity":"10","price":"100",
			"amount_minor":-100000,"currency":"RUB"}`, acc1.ID, sber.ID, date)
		r := do(t, c, "POST", url+"/api/v1/operations", body)
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

	// The first date on the allowed side. It is a genuine 201, not merely "not
	// a 400": a floor that also swallowed its own boundary would be a different
	// bug, and this is the assertion that separates them.
	if status, got := buy("1900-01-01"); status != 201 {
		t.Errorf("buy dated 1900-01-01 = %d, want 201: %s", status, got)
	}

	// The other end. Day-after-tomorrow in UTC is outside the one day of slack
	// from whatever zone this runs in, and tomorrow is the last day inside it —
	// the slack exists so someone east of UTC can record what they did this
	// evening, and a test that only refused a far-future date would pass on a
	// ceiling that had swallowed the slack entirely.
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

	// And the transfer endpoint, which had the ceiling and not the floor until
	// both moved behind one check. Its own refusal is what is being read here —
	// the buy above landed on acc1, so a transfer of it would otherwise be a
	// perfectly good request.
	transfer := fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,
		"instrument_id":%q,"quantity":"4","occurred_on":"1026-07-05"}`,
		acc1.ID, acc2.ID, sber.ID)
	resp = do(t, c, "POST", url+"/api/v1/operations/transfer", transfer)
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
	resp = do(t, c, "POST", url+"/api/v1/operations/transfer", transferFuture)
	got3, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 400 {
		t.Fatalf("transfer dated the day after tomorrow = %d, want 400: %s", resp.StatusCode, got3)
	}
	if !strings.Contains(string(got3), "future") {
		t.Errorf("transfer refusal = %s, want it to say the date is in the future", got3)
	}
}
