package operation_test

import (
	"context"
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
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/portfolio"
)

// newTestPool spins up a migrated test database and returns it together
// with a marketdata.Store on it, so a test can seed fx_rates on the very
// pool the handler under test reads from.
func newTestPool(t *testing.T) (*pgxpool.Pool, *marketdata.Store) {
	t.Helper()
	pool := testdb.New(t)
	if err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool, marketdata.NewStore(pool)
}

// newAPIOn wires the full stack on an existing pool: family + account +
// instrument + operation modules, mirroring how cmd/babki/root.go's
// mountModules assembles them, with the operation handler's fx converter
// supplied by the caller (conv) so a test can substitute a double for the
// real, Postgres-backed one. It returns the server URL and a logged-in
// client for the space created by /api/v1/setup.
func newAPIOn(t *testing.T, pool *pgxpool.Pool, conv converterLike) (string, *http.Client) {
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
	account.NewHandler(account.NewStore(pool), famStore, marketdata.NewConverter(mdStore), auth, sm).Mount(srv)
	instrument.NewHandler(instStore, auth, sm).Mount(srv)
	operation.NewHandler(opSvc, opStore, famStore, conv, auth, sm).Mount(srv)
	// The positions endpoint is mounted here too, with a real converter of its
	// own rather than the caller's double: a journal row and the position built
	// from the same purchases must agree on what those purchases cost in the
	// base currency, and that claim can only be checked by asking both
	// endpoints of one running stack (see http_transfer_in_base_test.go).
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

// newAPIWithConverter is the standard fixture: the full stack on a fresh
// pool with a real, Postgres-backed fx converter, plus the marketdata store
// behind it so the test can seed the fx rates that converter will resolve.
func newAPIWithConverter(t *testing.T) (string, *http.Client, *marketdata.Store) {
	t.Helper()
	pool, mdStore := newTestPool(t)
	url, c := newAPIOn(t, pool, marketdata.NewConverter(mdStore))
	return url, c, mdStore
}

// newAPIWithConverterDouble is newAPIWithConverter with the operation
// handler's fx converter replaced by conv, for tests that need a specific
// failure mode out of it.
func newAPIWithConverterDouble(t *testing.T, conv converterLike) (string, *http.Client) {
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
	var list1 []opResp
	decodeJSON(t, resp, &list1)
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
	decodeJSON(t, resp, &list1)
	if len(list1) != 2 {
		t.Fatalf("list acc1 after transfer = %+v, want 2", list1)
	}
	resp = do(t, c, "GET", url+"/api/v1/accounts/"+acc2.ID+"/operations", "")
	var list2 []opResp
	decodeJSON(t, resp, &list2)
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
	decodeJSON(t, resp, &list1)
	if len(list1) != 1 {
		t.Fatalf("list acc1 after delete transfer = %+v, want 1 (buy only)", list1)
	}
	resp = do(t, c, "GET", url+"/api/v1/accounts/"+acc2.ID+"/operations", "")
	decodeJSON(t, resp, &list2)
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
