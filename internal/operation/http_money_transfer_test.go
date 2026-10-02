package operation_test

import (
	"fmt"
	"io"
	"net/http"
	"testing"
)

// journalRow is one row of a journal page, as far as these tests read it.
type journalRow struct {
	ID                   string  `json:"id"`
	Type                 string  `json:"type"`
	AmountMinor          int64   `json:"amount_minor"`
	Currency             string  `json:"currency"`
	Note                 string  `json:"note"`
	TransferGroupID      *string `json:"transfer_group_id"`
	CounterpartAccountID *string `json:"counterpart_account_id"`
}

func accountJournal(t *testing.T, c *http.Client, url, accountID string) []journalRow {
	t.Helper()
	resp := do(t, c, "GET", url+"/api/v1/accounts/"+accountID+"/operations", "")
	var page struct {
		Operations []journalRow `json:"operations"`
	}
	decodeJSON(t, resp, &page)
	return page.Operations
}

func moneyTransfer(t *testing.T, c *http.Client, url, body string) *http.Response {
	t.Helper()
	return do(t, c, "POST", url+"/api/v1/operations/money-transfer", body)
}

// Money moved from one account to another is a withdrawal on the first and a
// deposit on the second, one transfer: each journal names the other account,
// and deleting either half deletes both.
func TestMoneyMovedBetweenAccountsIsOneTransfer(t *testing.T) {
	url, c := newAPI(t)
	from := createID(t, c, url+"/api/v1/accounts", `{"name":"Т-Банк","type":"brokerage","currency":"RUB"}`)
	to := createID(t, c, url+"/api/v1/accounts", `{"name":"Альфа","type":"brokerage","currency":"RUB"}`)
	do(t, c, "POST", url+"/api/v1/operations", fmt.Sprintf(
		`{"account_id":%q,"type":"deposit","occurred_on":"2026-07-01","amount_minor":500000,"currency":"RUB"}`, from))

	resp := moneyTransfer(t, c, url, fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"occurred_on":"2026-07-02","amount_minor":200000,"currency":"RUB","note":"на Альфу"}`, from, to))
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("money transfer = %d: %s", resp.StatusCode, b)
	}
	var pair transferResp
	decodeJSON(t, resp, &pair)
	if pair.Out.Type != "withdrawal" || pair.Out.AmountMinor != -200_000 || pair.In.Type != "deposit" || pair.In.AmountMinor != 200_000 {
		t.Fatalf("pair = %+v, want a withdrawal of 2000 ₽ and a deposit of 2000 ₽", pair)
	}

	var out, in journalRow
	for _, row := range accountJournal(t, c, url, from) {
		if row.ID == pair.Out.ID {
			out = row
		}
	}
	for _, row := range accountJournal(t, c, url, to) {
		if row.ID == pair.In.ID {
			in = row
		}
	}
	if out.CounterpartAccountID == nil || *out.CounterpartAccountID != to {
		t.Errorf("the withdrawal names %v, want the account it went to", out.CounterpartAccountID)
	}
	if in.CounterpartAccountID == nil || *in.CounterpartAccountID != from {
		t.Errorf("the deposit names %v, want the account it came from", in.CounterpartAccountID)
	}
	if out.TransferGroupID == nil || in.TransferGroupID == nil || *out.TransferGroupID != *in.TransferGroupID {
		t.Errorf("groups %v and %v, want the two halves of one transfer", out.TransferGroupID, in.TransferGroupID)
	}
	if out.Note != "на Альфу" || in.Note != "на Альфу" {
		t.Errorf("notes %q and %q, want the note on both halves", out.Note, in.Note)
	}
	for _, row := range accountJournal(t, c, url, from) {
		if row.Type == "deposit" && row.CounterpartAccountID != nil {
			t.Errorf("a plain deposit names a counterpart %v, want none", *row.CounterpartAccountID)
		}
	}

	if resp := do(t, c, "DELETE", url+"/api/v1/operations/"+pair.In.ID, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete the deposit = %d", resp.StatusCode)
	}
	for _, row := range accountJournal(t, c, url, from) {
		if row.ID == pair.Out.ID {
			t.Error("the withdrawal outlived the deposit it was paired with")
		}
	}
}

// Money converted on the way arrives as what was received.
func TestMoneyConvertedOnTheWayArrivesAsReceived(t *testing.T) {
	url, c := newAPI(t)
	from := createID(t, c, url+"/api/v1/accounts", `{"name":"Рубли","type":"brokerage","currency":"RUB"}`)
	to := createID(t, c, url+"/api/v1/accounts", `{"name":"Доллары","type":"brokerage","currency":"USD"}`)
	resp := moneyTransfer(t, c, url, fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"occurred_on":"2026-07-02","amount_minor":900000,"currency":"RUB","received_minor":10000,"received_currency":"USD"}`, from, to))
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("money transfer = %d: %s", resp.StatusCode, b)
	}
	var pair transferResp
	decodeJSON(t, resp, &pair)
	rows := accountJournal(t, c, url, to)
	if len(rows) != 1 || rows[0].AmountMinor != 10_000 || rows[0].Currency != "USD" {
		t.Errorf("the deposit = %+v, want 100 $", rows)
	}
	if rows := accountJournal(t, c, url, from); len(rows) != 1 || rows[0].AmountMinor != -900_000 || rows[0].Currency != "RUB" {
		t.Errorf("the withdrawal = %+v, want 9000 ₽", rows)
	}
}

func TestAMoneyTransferIsRefusedForWhatEachHalfWouldBeRefusedFor(t *testing.T) {
	url, c := newAPI(t)
	a := createID(t, c, url+"/api/v1/accounts", `{"name":"А","type":"brokerage","currency":"RUB"}`)
	b := createID(t, c, url+"/api/v1/accounts", `{"name":"Б","type":"brokerage","currency":"RUB"}`)
	archived := createID(t, c, url+"/api/v1/accounts", `{"name":"В архиве","type":"brokerage","currency":"RUB"}`)
	do(t, c, "DELETE", url+"/api/v1/accounts/"+archived, "")
	body := func(from, to, rest string) string {
		return fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,"occurred_on":"2026-07-02",%s}`, from, to, rest)
	}
	for name, req := range map[string]string{
		"the same account":          body(a, a, `"amount_minor":100,"currency":"RUB"`),
		"a zero amount":             body(a, b, `"amount_minor":0,"currency":"RUB"`),
		"a negative amount":         body(a, b, `"amount_minor":-100,"currency":"RUB"`),
		"an amount past the bound":  body(a, b, `"amount_minor":1000000000000001,"currency":"RUB"`),
		"a lowercase currency":      body(a, b, `"amount_minor":100,"currency":"rub"`),
		"a received amount alone":   body(a, b, `"amount_minor":100,"currency":"RUB","received_minor":100`),
		"a received currency alone": body(a, b, `"amount_minor":100,"currency":"RUB","received_currency":"USD"`),
		"nothing received":          body(a, b, `"amount_minor":100,"currency":"RUB","received_minor":0,"received_currency":"USD"`),
		"into an archived account":  body(a, archived, `"amount_minor":100,"currency":"RUB"`),
		"a future date":             fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,"occurred_on":"2099-01-01","amount_minor":100,"currency":"RUB"}`, a, b),
	} {
		if resp := moneyTransfer(t, c, url, req); resp.StatusCode != http.StatusBadRequest {
			got, _ := io.ReadAll(resp.Body)
			t.Errorf("%s = %d, want 400: %s", name, resp.StatusCode, got)
		}
	}
	if rows := accountJournal(t, c, url, a); len(rows) != 0 {
		t.Errorf("refused transfers left %d rows behind", len(rows))
	}
}

// A move of shares names the other account too.
func TestASharesTransferNamesTheOtherAccount(t *testing.T) {
	url, c := newAPI(t)
	a := createID(t, c, url+"/api/v1/accounts", `{"name":"А","type":"brokerage","currency":"RUB"}`)
	b := createID(t, c, url+"/api/v1/accounts", `{"name":"Б","type":"brokerage","currency":"RUB"}`)
	sber := createID(t, c, url+"/api/v1/instruments", `{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)
	do(t, c, "POST", url+"/api/v1/operations", fmt.Sprintf(
		`{"account_id":%q,"instrument_id":%q,"type":"buy","occurred_on":"2026-07-01","quantity":"10","price":"100","currency":"RUB"}`, a, sber))
	resp := do(t, c, "POST", url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"4","occurred_on":"2026-07-02"}`, a, b, sber))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("transfer = %d", resp.StatusCode)
	}
	rows := accountJournal(t, c, url, b)
	if len(rows) != 1 || rows[0].CounterpartAccountID == nil || *rows[0].CounterpartAccountID != a {
		t.Errorf("the arriving shares: %+v, want them to name the account they left", rows)
	}
}

// One paper's rows across the family: both accounts' rows of it, newest first,
// each saying whose it is; another paper's rows and money rows are not there.
func TestAPapersRowsAcrossEveryAccount(t *testing.T) {
	url, c := newAPI(t)
	a := createID(t, c, url+"/api/v1/accounts", `{"name":"А","type":"brokerage","currency":"RUB"}`)
	b := createID(t, c, url+"/api/v1/accounts", `{"name":"Б","type":"brokerage","currency":"RUB"}`)
	sber := createID(t, c, url+"/api/v1/instruments", `{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)
	gazp := createID(t, c, url+"/api/v1/instruments", `{"type":"share","name":"Газпром","ticker":"GAZP","currency":"RUB"}`)
	for _, op := range []string{
		fmt.Sprintf(`{"account_id":%q,"type":"deposit","occurred_on":"2026-07-01","amount_minor":1000000,"currency":"RUB"}`, a),
		fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy","occurred_on":"2026-07-02","quantity":"10","price":"100","currency":"RUB"}`, a, sber),
		fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy","occurred_on":"2026-07-03","quantity":"1","price":"100","currency":"RUB"}`, a, gazp),
		fmt.Sprintf(`{"account_id":%q,"type":"deposit","occurred_on":"2026-07-01","amount_minor":1000000,"currency":"RUB"}`, b),
		fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy","occurred_on":"2026-07-04","quantity":"5","price":"110","currency":"RUB"}`, b, sber),
	} {
		if resp := do(t, c, "POST", url+"/api/v1/operations", op); resp.StatusCode != http.StatusCreated {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("create = %d: %s", resp.StatusCode, b)
		}
	}
	resp := do(t, c, "GET", url+"/api/v1/instruments/"+sber+"/operations", "")
	var page struct {
		Operations []struct {
			AccountID  string `json:"account_id"`
			OccurredOn string `json:"occurred_on"`
		} `json:"operations"`
		HasMore bool `json:"has_more"`
	}
	decodeJSON(t, resp, &page)
	if len(page.Operations) != 2 || page.Operations[0].AccountID != b || page.Operations[1].AccountID != a ||
		page.Operations[0].OccurredOn != "2026-07-04" || page.HasMore {
		t.Errorf("SBER's rows = %+v, want Б's buy then А's, and nothing else", page)
	}
	resp = do(t, c, "GET", url+"/api/v1/instruments/"+sber+"/operations?limit=1", "")
	decodeJSON(t, resp, &page)
	if len(page.Operations) != 1 || !page.HasMore {
		t.Errorf("a page of one = %+v, want one row and more to come", page)
	}
}
