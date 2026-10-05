package operation_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/portfolio"
)

// newTestPool returns a migrated test database and a marketdata.Store on it
// for seeding fx_rates.
func newTestPool(t *testing.T) (*pgxpool.Pool, *marketdata.Store) {
	t.Helper()
	pool := testdb.New(t)
	return pool, marketdata.NewStore(pool)
}

// newAPIOn wires the full stack on pool as cmd/babki mounts it, with conv as
// the operation handler's converter, and returns the server URL and a logged-in
// client.
func newAPIOn(t *testing.T, pool *pgxpool.Pool, conv marketdata.RateSource) (string, *http.Client) {
	t.Helper()
	famStore := family.NewStore(pool)
	famSvc := family.NewService(famStore)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)

	opStore := operation.NewStore(pool)
	opSvc := operation.NewService(opStore)

	mdStore := marketdata.NewStore(pool)
	instStore := instrument.NewStore(pool)

	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(famSvc, famStore, auth, sm).Mount(srv)
	account.NewHandler(account.NewStore(pool), famStore, marketdata.NewConverter(mdStore), nil, auth, sm).Mount(srv)
	instrument.NewHandler(instStore, auth, sm).Mount(srv)
	operation.NewHandler(opSvc, opStore, famStore, conv, auth, sm).Mount(srv)
	// Positions use a real converter, so a journal row and its position can be
	// compared on one running stack (http_transfer_in_base_test.go).
	portfolio.NewHandler(opStore, instStore, mdStore, marketdata.NewConverter(mdStore), famStore, auth, sm).Mount(srv)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	resp, err := client.Post(ts.URL+"/api/v1/setup", "application/json",
		strings.NewReader(`{"space_name":"S","username":"alex","display_name":"A","password":"secret123"}`))
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("setup: %v %d", err, resp.StatusCode)
	}
	return ts.URL, client
}

// newAPIWithConverter is the standard fixture: the full stack with a real
// converter and its store for seeding rates.
func newAPIWithConverter(t *testing.T) (string, *http.Client, *marketdata.Store) {
	t.Helper()
	pool, mdStore := newTestPool(t)
	url, c := newAPIOn(t, pool, marketdata.NewConverter(mdStore))
	return url, c, mdStore
}

// newAPIWithConverterDouble swaps the operation handler's converter for
// conv.
func newAPIWithConverterDouble(t *testing.T, conv marketdata.RateSource) (string, *http.Client) {
	t.Helper()
	pool, _ := newTestPool(t)
	return newAPIOn(t, pool, conv)
}

// newAPI is newAPIWithConverter for tests that never touch fx rates.
func newAPI(t *testing.T) (string, *http.Client) {
	t.Helper()
	url, c, _ := newAPIWithConverter(t)
	return url, c
}

func do(t *testing.T, c *http.Client, method, url, body string) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func decodeJSON(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

type idResp struct {
	ID string `json:"id"`
}

type opResp struct {
	ID          string  `json:"id"`
	AccountId   string  `json:"account_id"`
	Type        string  `json:"type"`
	Quantity    *string `json:"quantity"`
	Price       *string `json:"price"`
	AmountMinor int64   `json:"amount_minor"`
	// Published on every operation, this response included.
	HasUndatedLots bool `json:"has_undated_lots"`
	// Also on this response, which omits in_base (#67).
	AssembledFromLots bool `json:"assembled_from_lots"`
}

type transferResp struct {
	Out opResp `json:"out"`
	In  opResp `json:"in"`
}

func TestOperationsJournalAndTransfers(t *testing.T) {
	url, c := newAPI(t)

	// two accounts and one instrument, created via the API (matching the
	// brief's full-stack fixture requirement).
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

	// POST buy -> 201
	buyBody := fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"RUB","fee_minor":10}`, acc1.ID, sber.ID)
	resp = do(t, c, "POST", url+"/api/v1/operations", buyBody)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("buy = %d: %s", resp.StatusCode, b)
	}
	var buy opResp
	decodeJSON(t, resp, &buy)
	if buy.ID == "" || buy.Quantity == nil || *buy.Quantity != "10" ||
		buy.Price == nil || *buy.Price != "100" || buy.AmountMinor != -100000 {
		t.Fatalf("buy = %+v", buy)
	}

	// GET operations list: one item, quantity/price as strings
	resp = do(t, c, "GET", url+"/api/v1/accounts/"+acc1.ID+"/operations", "")
	if resp.StatusCode != 200 {
		t.Fatalf("list acc1 = %d", resp.StatusCode)
	}
	// The listing is an envelope since #86 (see OperationsResponse); these
	// assertions are about the rows inside it.
	var page1 struct {
		Operations []opResp `json:"operations"`
	}
	decodeJSON(t, resp, &page1)
	list1 := page1.Operations
	if len(list1) != 1 || list1[0].ID != buy.ID || list1[0].Quantity == nil || *list1[0].Quantity != "10" {
		t.Fatalf("list1 after buy = %+v", list1)
	}

	// POST sell exceeding the held quantity -> 409 inconsistent
	oversellBody := fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":"2026-07-02","quantity":"999","amount_minor":999000,"currency":"RUB"}`,
		acc1.ID, sber.ID)
	resp = do(t, c, "POST", url+"/api/v1/operations", oversellBody)
	if resp.StatusCode != 409 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("oversell = %d, want 409: %s", resp.StatusCode, b)
	}

	// POST transfer -> 201 with out/in legs
	transferBody := fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,
		"instrument_id":%q,"quantity":"4","occurred_on":"2026-07-05"}`,
		acc1.ID, acc2.ID, sber.ID)
	resp = do(t, c, "POST", url+"/api/v1/operations/transfer", transferBody)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("transfer = %d: %s", resp.StatusCode, b)
	}
	var transfer transferResp
	decodeJSON(t, resp, &transfer)
	if transfer.Out.ID == "" || transfer.In.ID == "" ||
		transfer.Out.AccountId != acc1.ID || transfer.In.AccountId != acc2.ID ||
		transfer.Out.Type != "transfer_out" || transfer.In.Type != "transfer_in" ||
		transfer.Out.Quantity == nil || *transfer.Out.Quantity != "4" {
		t.Fatalf("transfer = %+v", transfer)
	}

	// GET operations of both accounts see the respective leg
	resp = do(t, c, "GET", url+"/api/v1/accounts/"+acc1.ID+"/operations", "")
	decodeJSON(t, resp, &page1)
	list1 = page1.Operations
	if len(list1) != 2 {
		t.Fatalf("list acc1 after transfer = %+v, want 2", list1)
	}
	resp = do(t, c, "GET", url+"/api/v1/accounts/"+acc2.ID+"/operations", "")
	var page2 struct {
		Operations []opResp `json:"operations"`
	}
	decodeJSON(t, resp, &page2)
	list2 := page2.Operations
	if len(list2) != 1 || list2[0].ID != transfer.In.ID {
		t.Fatalf("list acc2 after transfer = %+v", list2)
	}

	// DELETE the transfer group -> 204, both legs disappear
	resp = do(t, c, "DELETE", url+"/api/v1/operations/"+transfer.Out.ID, "")
	if resp.StatusCode != 204 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("delete transfer = %d: %s", resp.StatusCode, b)
	}
	resp = do(t, c, "GET", url+"/api/v1/accounts/"+acc1.ID+"/operations", "")
	decodeJSON(t, resp, &page1)
	list1 = page1.Operations
	if len(list1) != 1 {
		t.Fatalf("list acc1 after delete transfer = %+v, want 1 (buy only)", list1)
	}
	resp = do(t, c, "GET", url+"/api/v1/accounts/"+acc2.ID+"/operations", "")
	decodeJSON(t, resp, &page2)
	list2 = page2.Operations
	if len(list2) != 0 {
		t.Fatalf("list acc2 after delete transfer = %+v, want 0", list2)
	}

	// create a sell so the buy can no longer be deleted without breaking it
	sellBody := fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":"2026-07-03","quantity":"5","price":"110",
		"amount_minor":55000,"currency":"RUB"}`, acc1.ID, sber.ID)
	resp = do(t, c, "POST", url+"/api/v1/operations", sellBody)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("sell = %d: %s", resp.StatusCode, b)
	}

	// DELETE buy while the sell still depends on it -> 409
	resp = do(t, c, "DELETE", url+"/api/v1/operations/"+buy.ID, "")
	if resp.StatusCode != 409 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("delete buy with live sell = %d, want 409: %s", resp.StatusCode, b)
	}

	// invalid quantity ("abc") -> 400, rejected before reaching the service
	badBody := fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"abc","amount_minor":-1000,"currency":"RUB"}`,
		acc1.ID, sber.ID)
	resp = do(t, c, "POST", url+"/api/v1/operations", badBody)
	if resp.StatusCode != 400 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("invalid quantity = %d, want 400: %s", resp.StatusCode, b)
	}

	// viewer can read but not write
	if resp = do(t, c, "POST", url+"/api/v1/members",
		`{"username":"vera","display_name":"V","password":"password9","role":"viewer"}`); resp.StatusCode != 201 {
		t.Fatalf("create viewer = %d", resp.StatusCode)
	}
	jar, _ := cookiejar.New(nil)
	vera := &http.Client{Jar: jar}
	if resp = do(t, vera, "POST", url+"/api/v1/auth/login",
		`{"username":"vera","password":"password9"}`); resp.StatusCode != 200 {
		t.Fatalf("vera login = %d", resp.StatusCode)
	}
	viewerBuyBody := fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"1","price":"1",
		"amount_minor":-100,"currency":"RUB"}`, acc1.ID, sber.ID)
	if resp = do(t, vera, "POST", url+"/api/v1/operations", viewerBuyBody); resp.StatusCode != 403 {
		t.Errorf("vera create = %d, want 403", resp.StatusCode)
	}
}

// A buy or sell with quantity and price may omit the amount; the server
// records quantity × price. Anything else without an amount is refused, and a
// given amount is taken as given.
func TestATradeWithoutAnAmountGetsTheServersOwn(t *testing.T) {
	url, c := newAPI(t)
	acc := mkAccount(t, url, c, "Брокер", "RUB")
	inst := mkInstrument(t, url, c, `{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)

	post := func(body string) (int, journalItem) {
		t.Helper()
		resp := do(t, c, "POST", url+"/api/v1/operations", body)
		var item journalItem
		if resp.StatusCode == 201 {
			decodeJSON(t, resp, &item)
		}
		return resp.StatusCode, item
	}
	trade := func(typ, extra string) string {
		return fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":%q,"occurred_on":"2026-03-02","currency":"RUB"%s}`,
			acc, inst, typ, extra)
	}

	// 3 × 0.335 = 1.005, half a kopeck over: 1.01, and a buy is money leaving.
	if status, got := post(trade("buy", `,"quantity":"3","price":"0.335"`)); status != 201 || got.AmountMinor != -101 {
		t.Errorf("buy without an amount: %d, amount %d; want 201 and -101", status, got.AmountMinor)
	}
	if status, got := post(trade("sell", `,"quantity":"1","price":"0.335"`)); status != 201 || got.AmountMinor != 34 {
		t.Errorf("sell without an amount: %d, amount %d; want 201 and 34", status, got.AmountMinor)
	}
	// Given, it is the figure: a total that includes what the price does not.
	if status, got := post(trade("buy", `,"quantity":"1","price":"100","amount_minor":-10300`)); status != 201 || got.AmountMinor != -10_300 {
		t.Errorf("buy with its own amount: %d, amount %d; want 201 and -10300", status, got.AmountMinor)
	}
	for name, body := range map[string]string{
		"a buy with no price":    trade("buy", `,"quantity":"3"`),
		"a buy with no quantity": trade("buy", `,"price":"3"`),
		"a dividend":             trade("dividend", ``),
	} {
		if status, _ := post(body); status != 400 {
			t.Errorf("%s and no amount: %d, want 400", name, status)
		}
	}
}
