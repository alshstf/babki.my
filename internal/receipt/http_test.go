package receipt_test

import (
	"fmt"
	"log/slog"
	"net/http"
	"testing"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/receipt"
)

// A receipt read off its QR code finds the bank's row of the same purchase —
// its total, a day off — and completes it rather than standing for a second
// one; read again, it is the receipt written already. A broker's row, another
// total or a row completed already is no candidate.
func TestAReceiptCompletesTheBanksRow(t *testing.T) {
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	accStore, opStore := account.NewStore(pool), operation.NewStore(pool)
	conv := marketdata.NewConverter(marketdata.NewStore(pool))
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	account.NewHandler(accStore, famStore, conv, nil, auth, sm).Mount(srv)
	operation.NewHandler(operation.NewService(opStore), opStore, famStore, conv, auth, sm).Mount(srv)
	receipt.NewHandler(receipt.NewService(pool, opStore, accStore), auth, sm).Mount(srv)
	url, c := apitest.Serve(t, srv.Handler())

	mk := func(name, typ string) string {
		var a struct {
			ID string `json:"id"`
		}
		apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/accounts", fmt.Sprintf(`{"name":%q,"type":%q,"currency":"RUB"}`, name, typ)), &a)
		return a.ID
	}
	row := func(acc, on string, amount int64) string {
		var o struct {
			ID string `json:"id"`
		}
		typ := "withdrawal"
		if amount > 0 {
			typ = "deposit"
		}
		resp := apitest.Do(t, c, "POST", url+"/api/v1/operations",
			fmt.Sprintf(`{"account_id":%q,"type":%q,"occurred_on":%q,"amount_minor":%d,"currency":"RUB"}`, acc, typ, on, amount))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("row = %d", resp.StatusCode)
		}
		apitest.Decode(t, resp, &o)
		return o.ID
	}
	card, broker := mk("Карта", "checking"), mk("Брокер", "brokerage")
	row(card, "2026-10-01", 10_000_000)
	purchase := row(card, "2026-10-10", -234_090) // the bank shows it the next day
	row(card, "2026-10-09", -234_000)
	row(broker, "2026-10-01", 10_000_000)
	row(broker, "2026-10-09", -234_090)

	match := func(fd, kind string, total int64) apitypes.ReceiptLookup {
		var got apitypes.ReceiptLookup
		resp := apitest.Do(t, c, "GET", url+fmt.Sprintf("/api/v1/receipts/match?fn=7380440700123456&fd=%s&kind=%s&total_minor=%d&issued_at=2026-10-09T19:15", fd, kind, total), "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("match = %d", resp.StatusCode)
		}
		apitest.Decode(t, resp, &got)
		return got
	}
	got := match("51243", "purchase", 234_090)
	if !got.Receipt.IsNull() || len(got.Candidates) != 1 || got.Candidates[0].Id.String() != purchase || got.Candidates[0].OccurredOn != "2026-10-10" {
		t.Fatalf("a new receipt's candidates = %+v", got)
	}
	if got := match("51244", "refund", 234_090); len(got.Candidates) != 0 {
		t.Errorf("a refund's candidates = %+v, want none: the rows are spending", got.Candidates)
	}

	create := func(body string) *http.Response { return apitest.Do(t, c, "POST", url+"/api/v1/receipts", body) }
	body := func(op string, total int64) string {
		return fmt.Sprintf(`{"operation_id":%s,"fn":"7380440700123456","fd":"51243","fp":"1234567890","kind":"purchase","issued_at":"2026-10-09T19:15","total_minor":%d,"source":"qr"}`, op, total)
	}
	if r := create(body(fmt.Sprintf("%q", purchase), 234_000)); r.StatusCode != http.StatusBadRequest {
		t.Errorf("another total = %d, want 400", r.StatusCode)
	}
	resp := create(body(fmt.Sprintf("%q", purchase), 234_090))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	var made apitypes.Receipt
	apitest.Decode(t, resp, &made)
	if op, _ := made.OperationId.Get(); op.String() != purchase || made.IssuedAt != "2026-10-09T19:15" || made.Items == nil {
		t.Errorf("receipt = %+v", made)
	}
	if r := create(body("null", 234_090)); r.StatusCode != http.StatusConflict {
		t.Errorf("the same receipt again = %d, want 409", r.StatusCode)
	}

	got = match("51243", "purchase", 234_090)
	if to, err := got.WrittenTo.Get(); got.Receipt.IsNull() || err != nil || to.Id.String() != purchase || len(got.Candidates) != 0 {
		t.Errorf("a receipt written = %+v", got)
	}
	// Completed, the row is no candidate for another receipt of its total.
	if got := match("51245", "purchase", 234_090); len(got.Candidates) != 0 {
		t.Errorf("another receipt's candidates = %+v", got.Candidates)
	}
	// One waiting for a row: no operation.
	if r := create(`{"fn":"7380440700123456","fd":"60001","kind":"purchase","issued_at":"2026-10-09T10:00","total_minor":5000,"source":"qr"}`); r.StatusCode != http.StatusCreated {
		t.Errorf("a receipt with no row = %d", r.StatusCode)
	}
	for name, q := range map[string]string{
		"letters in fn": "fn=abc&fd=1&kind=purchase&total_minor=1&issued_at=2026-10-09T10:00",
		"no time":       "fn=1&fd=1&kind=purchase&total_minor=1&issued_at=2026-10-09",
		"a bad kind":    "fn=1&fd=1&kind=gift&total_minor=1&issued_at=2026-10-09T10:00",
		"zero":          "fn=1&fd=1&kind=purchase&total_minor=0&issued_at=2026-10-09T10:00",
	} {
		if r := apitest.Do(t, c, "GET", url+"/api/v1/receipts/match?"+q, ""); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, r.StatusCode)
		}
	}
}
