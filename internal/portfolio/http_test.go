package portfolio_test

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

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/portfolio"
)

// newAPI wires the full stack: family + account + instrument + operation +
// portfolio modules, mirroring how cmd/babki/root.go's mountModules
// assembles them (same fixture shape as operation/http_test.go's newAPI).
func newAPI(t *testing.T) (string, *http.Client) {
	t.Helper()
	pool := testdb.New(t)
	if err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	famStore := family.NewStore(pool)
	famSvc := family.NewService(famStore)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)

	instStore := instrument.NewStore(pool)
	opStore := operation.NewStore(pool)
	opSvc := operation.NewService(opStore)

	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(famSvc, famStore, auth, sm).Mount(srv)
	account.NewHandler(account.NewStore(pool), auth, sm).Mount(srv)
	instrument.NewHandler(instStore, auth, sm).Mount(srv)
	operation.NewHandler(opSvc, opStore, auth, sm).Mount(srv)
	portfolio.NewHandler(opStore, instStore, auth, sm).Mount(srv)

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

func createAccount(t *testing.T, c *http.Client, url, body string) idResp {
	t.Helper()
	resp := do(t, c, "POST", url+"/api/v1/accounts", body)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create account = %d: %s", resp.StatusCode, b)
	}
	var out idResp
	decodeJSON(t, resp, &out)
	return out
}

func createInstrument(t *testing.T, c *http.Client, url, body string) idResp {
	t.Helper()
	resp := do(t, c, "POST", url+"/api/v1/instruments", body)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create instrument = %d: %s", resp.StatusCode, b)
	}
	var out idResp
	decodeJSON(t, resp, &out)
	return out
}

func createOperation(t *testing.T, c *http.Client, url, body string) {
	t.Helper()
	resp := do(t, c, "POST", url+"/api/v1/operations", body)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create operation = %d: %s", resp.StatusCode, b)
	}
}

type instrumentResp struct {
	Id   string `json:"id"`
	Name string `json:"name"`
}

type positionResp struct {
	Instrument       instrumentResp `json:"instrument"`
	Quantity         string         `json:"quantity"`
	CostMinor        int64          `json:"cost_minor"`
	Currency         string         `json:"currency"`
	RealizedPnlMinor int64          `json:"realized_pnl_minor"`
	IncomeMinor      int64          `json:"income_minor"`
	FeesMinor        int64          `json:"fees_minor"`
}

type positionsResp struct {
	Positions []positionResp `json:"positions"`
}

// TestPositionsEndpoint covers three scenarios on the GET
// /api/v1/accounts/{accountId}/positions endpoint: a real, still-open
// position folded from a mixed journal; an account with no operations at
// all (must yield an empty array, not null); and a fully closed position
// (bought and sold in full), which must still appear with quantity "0"
// since realized P&L on it remains meaningful.
func TestPositionsEndpoint(t *testing.T) {
	url, c := newAPI(t)

	acc1 := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	acc2 := createAccount(t, c, url, `{"name":"Пустой","type":"brokerage","currency":"RUB"}`)
	acc3 := createAccount(t, c, url, `{"name":"Закрытая позиция","type":"brokerage","currency":"RUB"}`)

	sber := createInstrument(t, c, url, `{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)
	lkoh := createInstrument(t, c, url, `{"type":"share","name":"Лукойл","ticker":"LKOH","currency":"RUB"}`)

	// --- acc1: deposit, 2 buys, 1 partial sell, dividend ---
	//
	// Manual computation (matches the engine's FIFO rules, see
	// portfolio/engine.go and engine_test.go):
	//
	//   deposit          amount 1_000_000               (cash-level, ignored by the engine)
	//   buy  10 @ 100.00 amount -100_000 fee 10          lot1: qty 10, cost 100_010
	//   buy  10 @ 110.00 amount -110_000 fee 11          lot2: qty 10, cost 110_011
	//   sell  5 @ 120.00 amount  60_000  fee  5
	//       released = floor(100_010 * 5/10) = 50_005 (partial piece of lot1)
	//       lot1 remainder: qty 5, cost 100_010-50_005 = 50_005
	//       realized += 60_000 - 50_005 - 5 = 9_990
	//   dividend         amount 5_000 (tagged to SBER)   income += 5_000
	//
	//   quantity    = 10 + 10 - 5              = 15
	//   cost_minor  = (100_010+110_011)-50_005 = 160_016
	//   realized    = 9_990
	//   income      = 5_000
	//   fees        = 10 + 11 + 5              = 26
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"deposit",
		"occurred_on":"2026-07-01","amount_minor":1000000,"currency":"RUB"}`, acc1.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-02","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"RUB","fee_minor":10}`, acc1.ID, sber.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-03","quantity":"10","price":"110",
		"amount_minor":-110000,"currency":"RUB","fee_minor":11}`, acc1.ID, sber.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":"2026-07-04","quantity":"5","price":"120",
		"amount_minor":60000,"currency":"RUB","fee_minor":5}`, acc1.ID, sber.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend",
		"occurred_on":"2026-07-05","amount_minor":5000,"currency":"RUB"}`, acc1.ID, sber.ID))

	resp := do(t, c, "GET", url+"/api/v1/accounts/"+acc1.ID+"/positions", "")
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET positions acc1 = %d: %s", resp.StatusCode, b)
	}
	body, _ := io.ReadAll(resp.Body)
	var got1 positionsResp
	if err := json.Unmarshal(body, &got1); err != nil {
		t.Fatalf("decode acc1 positions: %v, body=%s", err, body)
	}
	if len(got1.Positions) != 1 {
		t.Fatalf("acc1 positions = %+v, want exactly 1", got1.Positions)
	}
	p := got1.Positions[0]
	if p.Instrument.Id != sber.ID || p.Instrument.Name != "Сбербанк" {
		t.Errorf("acc1 position instrument = %+v, want id=%s name=Сбербанк", p.Instrument, sber.ID)
	}
	if p.Quantity != "15" {
		t.Errorf("acc1 position quantity = %q, want %q", p.Quantity, "15")
	}
	if p.CostMinor != 160016 {
		t.Errorf("acc1 position cost_minor = %d, want 160016", p.CostMinor)
	}
	if p.RealizedPnlMinor != 9990 {
		t.Errorf("acc1 position realized_pnl_minor = %d, want 9990", p.RealizedPnlMinor)
	}
	if p.IncomeMinor != 5000 {
		t.Errorf("acc1 position income_minor = %d, want 5000", p.IncomeMinor)
	}
	if p.FeesMinor != 26 {
		t.Errorf("acc1 position fees_minor = %d, want 26", p.FeesMinor)
	}
	if p.Currency != "RUB" {
		t.Errorf("acc1 position currency = %q, want RUB", p.Currency)
	}

	// --- acc2: no operations at all -> {"positions":[]}, not null ---
	resp = do(t, c, "GET", url+"/api/v1/accounts/"+acc2.ID+"/positions", "")
	if resp.StatusCode != 200 {
		t.Fatalf("GET positions acc2 = %d", resp.StatusCode)
	}
	body, _ = io.ReadAll(resp.Body)
	if strings.TrimSpace(string(body)) != `{"positions":[]}` {
		t.Errorf("acc2 positions body = %s, want {\"positions\":[]}", body)
	}

	// --- acc3: buy then sell the full quantity -> closed position (qty 0)
	// still present, since realized P&L on it is meaningful history.
	//
	//   buy 3 @ 200.00 amount -60_000 fee 0    lot: qty 3, cost 60_000
	//   sell 3 @ 210.00 amount 63_000 fee 0
	//       released = full lot cost = 60_000
	//       realized += 63_000 - 60_000 - 0 = 3_000
	//
	//   quantity = 0, cost_minor = 0, realized = 3_000, income = 0, fees = 0
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"3","price":"200",
		"amount_minor":-60000,"currency":"RUB"}`, acc3.ID, lkoh.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":"2026-07-02","quantity":"3","price":"210",
		"amount_minor":63000,"currency":"RUB"}`, acc3.ID, lkoh.ID))

	resp = do(t, c, "GET", url+"/api/v1/accounts/"+acc3.ID+"/positions", "")
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET positions acc3 = %d: %s", resp.StatusCode, b)
	}
	var got3 positionsResp
	decodeJSON(t, resp, &got3)
	if len(got3.Positions) != 1 {
		t.Fatalf("acc3 positions = %+v, want exactly 1 (closed position kept)", got3.Positions)
	}
	closed := got3.Positions[0]
	if closed.Instrument.Id != lkoh.ID {
		t.Errorf("acc3 position instrument id = %s, want %s", closed.Instrument.Id, lkoh.ID)
	}
	if closed.Quantity != "0" {
		t.Errorf("acc3 closed position quantity = %q, want %q", closed.Quantity, "0")
	}
	if closed.CostMinor != 0 {
		t.Errorf("acc3 closed position cost_minor = %d, want 0", closed.CostMinor)
	}
	if closed.RealizedPnlMinor != 3000 {
		t.Errorf("acc3 closed position realized_pnl_minor = %d, want 3000", closed.RealizedPnlMinor)
	}
}
