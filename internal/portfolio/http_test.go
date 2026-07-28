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
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

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

// quoteStoreLike mirrors portfolio's unexported quoteStore interface so this
// external test package can name the parameter type of setupAPI. Any value
// satisfying it (a real *marketdata.Store or a test fake) is structurally
// assignable to portfolio.NewHandler's quoteStore parameter — Go interface
// assignability is by method set, not by the interface's (unexported) name.
type quoteStoreLike interface {
	LatestQuotes(ctx context.Context, instrumentIDs []uuid.UUID) (map[uuid.UUID]marketdata.Quote, error)
}

// setupAPI wires the full stack: family + account + instrument + operation +
// portfolio modules, mirroring how cmd/babki/root.go's mountModules
// assembles them (same fixture shape as operation/http_test.go's newAPI).
// quotes backs the portfolio handler's market-quote lookups. pool is
// provided by the caller (rather than created here) so tests that need
// direct DB access for setup, or that just want the default real
// marketdata.Store, can share the same pool the HTTP stack runs on.
func setupAPI(t *testing.T, pool *pgxpool.Pool, quotes quoteStoreLike) (string, *http.Client) {
	t.Helper()
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
	account.NewHandler(account.NewStore(pool), famStore, marketdata.NewConverter(marketdata.NewStore(pool)), auth, sm).Mount(srv)
	instrument.NewHandler(instStore, auth, sm).Mount(srv)
	operation.NewHandler(opSvc, opStore, auth, sm).Mount(srv)
	portfolio.NewHandler(opStore, instStore, quotes, auth, sm).Mount(srv)

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

// newAPI is setupAPI backed by a real (empty) marketdata.Store, for tests
// that don't care about market valuation: no quotes ever exist, so all
// positions come back with null market_value_minor/price/price_on.
func newAPI(t *testing.T) (string, *http.Client) {
	t.Helper()
	pool := testdb.New(t)
	return setupAPI(t, pool, marketdata.NewStore(pool))
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
	Instrument          instrumentResp `json:"instrument"`
	Quantity            string         `json:"quantity"`
	CostMinor           int64          `json:"cost_minor"`
	Currency            string         `json:"currency"`
	RealizedPnlMinor    int64          `json:"realized_pnl_minor"`
	IncomeMinor         int64          `json:"income_minor"`
	FeesMinor           int64          `json:"fees_minor"`
	MarketValueMinor    *int64         `json:"market_value_minor"`
	MarketValueCurrency *string        `json:"market_value_currency"`
	Price               *string        `json:"price"`
	PriceOn             *string        `json:"price_on"`
	UnrealizedPnlMinor  *int64         `json:"unrealized_pnl_minor"`
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
	// newAPI backs this handler with a real, empty marketdata.Store — no
	// quote was ever ingested for sber, so all three valuation fields (plus
	// the unrealized P&L derived from them) must come back null (see
	// TestPositionsMarketValuation and TestPositionsUnrealizedPnl for the
	// priced cases).
	if p.MarketValueMinor != nil || p.Price != nil || p.PriceOn != nil || p.UnrealizedPnlMinor != nil {
		t.Errorf("acc1 position with no quote = %+v, want market_value_minor/price/price_on/unrealized_pnl_minor all null", p)
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

// fakeQuoteStore is a minimal, in-memory quoteStore double. It counts calls
// to LatestQuotes so tests can assert positions are valued with exactly one
// batched round trip — never one call per position (N+1) — and it echoes
// back only the quotes it was seeded with for the requested IDs, mirroring
// marketdata.Store.LatestQuotes's real contract that an instrument with no
// quote is simply absent from the result map, not zero-valued.
type fakeQuoteStore struct {
	byInstrument map[uuid.UUID]marketdata.Quote
	calls        int
}

func (f *fakeQuoteStore) LatestQuotes(_ context.Context, instrumentIDs []uuid.UUID) (map[uuid.UUID]marketdata.Quote, error) {
	f.calls++
	out := make(map[uuid.UUID]marketdata.Quote, len(instrumentIDs))
	for _, id := range instrumentIDs {
		if q, ok := f.byInstrument[id]; ok {
			out[id] = q
		}
	}
	return out, nil
}

// TestPositionsMarketValuation covers the market-value calculation added to
// GET /api/v1/accounts/{id}/positions: a share priced at quote × quantity
// (exercising half-up rounding at the exact .5 boundary), a bond priced as
// a percentage of its face value, an instrument with no quote at all, and
// an instrument whose type (custom) has no defined valuation model even
// though a quote exists for it — all four resolved from a single batched
// LatestQuotes call, not one per position.
func TestPositionsMarketValuation(t *testing.T) {
	pool := testdb.New(t)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes)

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)

	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"RUB"}`)
	// face_currency (USD) deliberately differs from the quote's own currency
	// (RUB, set below): a bond's price is a dimensionless percent-of-face, so
	// quote.Currency carries no real currency meaning for a bond — fix (2)'s
	// regression case. If market_value_currency ever regressed to echoing
	// quote.Currency for bonds, this test would catch it (RUB != USD).
	bond := createInstrument(t, c, url,
		`{"type":"bond","name":"Облигация","ticker":"BOND1","currency":"RUB","face_value_minor":100000,"face_currency":"USD"}`)
	noQuote := createInstrument(t, c, url, `{"type":"share","name":"Без Котировки","ticker":"NOQ","currency":"RUB"}`)
	custom := createInstrument(t, c, url, `{"type":"custom","name":"Прочее","currency":"RUB"}`)

	shareID, err := uuid.Parse(share.ID)
	if err != nil {
		t.Fatalf("parse share id: %v", err)
	}
	bondID, err := uuid.Parse(bond.ID)
	if err != nil {
		t.Fatalf("parse bond id: %v", err)
	}
	customID, err := uuid.Parse(custom.ID)
	if err != nil {
		t.Fatalf("parse custom id: %v", err)
	}

	// share: price 100.005 (major RUB) x quantity 1 = 100.005 major = 10000.5
	// minor exactly at the half-unit boundary. Half-away-from-zero rounds a
	// positive amount up, so market_value_minor must be 10001, not 10000.
	quotes.byInstrument[shareID] = marketdata.Quote{
		InstrumentID: shareID, On: mustDate(t, "2026-07-20"),
		Price: decimal.RequireFromString("100.005"), Currency: "RUB", Source: "test",
	}
	// bond: price is a percent of face value. face_value_minor 100000 (=
	// 1000.00 RUB) x 95.20% x quantity 100 = 100000 * 0.952 * 100 =
	// 9_520_000 minor units exactly — the worked example from the task brief.
	quotes.byInstrument[bondID] = marketdata.Quote{
		InstrumentID: bondID, On: mustDate(t, "2026-07-21"),
		Price: decimal.RequireFromString("95.20"), Currency: "RUB", Source: "test",
	}
	// custom: a quote exists, but "custom" has no defined valuation model,
	// so it must still come back null despite the quote being present.
	quotes.byInstrument[customID] = marketdata.Quote{
		InstrumentID: customID, On: mustDate(t, "2026-07-22"),
		Price: decimal.RequireFromString("50"), Currency: "RUB", Source: "test",
	}
	// noQuote deliberately gets no entry in quotes.byInstrument.

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"1","price":"100",
		"amount_minor":-10000,"currency":"RUB"}`, acc.ID, share.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"100","price":"950",
		"amount_minor":-9500000,"currency":"RUB"}`, acc.ID, bond.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"5","price":"10",
		"amount_minor":-5000,"currency":"RUB"}`, acc.ID, noQuote.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"2","price":"20",
		"amount_minor":-4000,"currency":"RUB"}`, acc.ID, custom.ID))

	resp := do(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/positions", "")
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET positions = %d: %s", resp.StatusCode, b)
	}
	var got positionsResp
	decodeJSON(t, resp, &got)
	if len(got.Positions) != 4 {
		t.Fatalf("positions = %+v, want exactly 4", got.Positions)
	}

	if quotes.calls != 1 {
		t.Errorf("LatestQuotes calls = %d, want exactly 1 (batched, not N+1)", quotes.calls)
	}

	byID := make(map[string]positionResp, len(got.Positions))
	for _, p := range got.Positions {
		byID[p.Instrument.Id] = p
	}

	sharePos, ok := byID[share.ID]
	if !ok {
		t.Fatalf("no position for share instrument")
	}
	if sharePos.MarketValueMinor == nil || *sharePos.MarketValueMinor != 10001 {
		t.Errorf("share market_value_minor = %v, want 10001", sharePos.MarketValueMinor)
	}
	// share/etf valuation is in the quote's own currency (RUB here) — see
	// fix (2): a share's price and instrument currency always agree, so
	// this is the trivial case, but the field must still be populated.
	if sharePos.MarketValueCurrency == nil || *sharePos.MarketValueCurrency != "RUB" {
		t.Errorf("share market_value_currency = %v, want RUB (the quote's currency)", sharePos.MarketValueCurrency)
	}
	if sharePos.Price == nil || *sharePos.Price != "100.005" {
		t.Errorf("share price = %v, want 100.005", sharePos.Price)
	}
	if sharePos.PriceOn == nil || *sharePos.PriceOn != "2026-07-20" {
		t.Errorf("share price_on = %v, want 2026-07-20", sharePos.PriceOn)
	}
	// unrealized = market_value_minor(10001) - cost_minor(10000) = 1. Both
	// currencies are RUB (the quote's currency and the buy operation's
	// currency agree for a share), so the field must be populated, not null
	// — see TestPositionsUnrealizedPnl for dedicated profit/loss/null cases.
	if sharePos.UnrealizedPnlMinor == nil || *sharePos.UnrealizedPnlMinor != 1 {
		t.Errorf("share unrealized_pnl_minor = %v, want 1", sharePos.UnrealizedPnlMinor)
	}

	bondPos, ok := byID[bond.ID]
	if !ok {
		t.Fatalf("no position for bond instrument")
	}
	if bondPos.MarketValueMinor == nil || *bondPos.MarketValueMinor != 9520000 {
		t.Errorf("bond market_value_minor = %v, want 9520000", bondPos.MarketValueMinor)
	}
	// The valuation is denominated in the instrument's face_currency (USD),
	// never the quote's currency (RUB) — see fix (2)'s doc comment on
	// marketValue.
	if bondPos.MarketValueCurrency == nil || *bondPos.MarketValueCurrency != "USD" {
		t.Errorf("bond market_value_currency = %v, want USD (face_currency, not the quote's RUB)", bondPos.MarketValueCurrency)
	}
	// shopspring/decimal normalizes trailing zeros on parse, so the quote's
	// price round-trips through Quote.Price.String() as "95.2", not "95.20".
	if bondPos.Price == nil || *bondPos.Price != "95.2" {
		t.Errorf("bond price = %v, want 95.2", bondPos.Price)
	}
	if bondPos.PriceOn == nil || *bondPos.PriceOn != "2026-07-21" {
		t.Errorf("bond price_on = %v, want 2026-07-21", bondPos.PriceOn)
	}
	// The bond's buy operation (and therefore position.Currency) is RUB, but
	// market_value_currency is USD (face_currency) — different currencies,
	// so unrealized_pnl_minor must be null even though market_value_minor
	// itself is populated. See TestPositionsUnrealizedPnl for the dedicated
	// case.
	if bondPos.UnrealizedPnlMinor != nil {
		t.Errorf("bond unrealized_pnl_minor = %v, want null (market_value_currency USD != position currency RUB)", bondPos.UnrealizedPnlMinor)
	}

	noQuotePos, ok := byID[noQuote.ID]
	if !ok {
		t.Fatalf("no position for noQuote instrument")
	}
	if noQuotePos.MarketValueMinor != nil || noQuotePos.MarketValueCurrency != nil || noQuotePos.Price != nil || noQuotePos.PriceOn != nil || noQuotePos.UnrealizedPnlMinor != nil {
		t.Errorf("noQuote position = %+v, want market_value_minor/market_value_currency/price/price_on/unrealized_pnl_minor all null", noQuotePos)
	}

	customPos, ok := byID[custom.ID]
	if !ok {
		t.Fatalf("no position for custom instrument")
	}
	if customPos.MarketValueMinor != nil || customPos.MarketValueCurrency != nil || customPos.Price != nil || customPos.PriceOn != nil || customPos.UnrealizedPnlMinor != nil {
		t.Errorf("custom position (quote present, unsupported type) = %+v, want market_value_minor/market_value_currency/price/price_on/unrealized_pnl_minor all null", customPos)
	}
}

// TestPositionsBondWithoutFaceCurrencyHasNoValuation covers fix (2)'s edge
// case: a bond that carries a face_value_minor but no face_currency (only
// reachable by PATCHing face_currency to null after creation — POST
// /instruments requires the two fields together, see
// instrument.handleCreate) has no currency to label its market value with,
// so it must get NO valuation at all — every one of
// market_value_minor/market_value_currency/price/price_on stays null, even
// though a quote exists and face_value_minor is still set. Publishing a
// bare number with no currency would be actively misleading, worse than
// publishing nothing.
func TestPositionsBondWithoutFaceCurrencyHasNoValuation(t *testing.T) {
	pool := testdb.New(t)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes)

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	bond := createInstrument(t, c, url,
		`{"type":"bond","name":"Облигация без валюты номинала","ticker":"BOND2","currency":"RUB","face_value_minor":100000,"face_currency":"RUB"}`)
	bondID, err := uuid.Parse(bond.ID)
	if err != nil {
		t.Fatalf("parse bond id: %v", err)
	}
	quotes.byInstrument[bondID] = marketdata.Quote{
		InstrumentID: bondID, On: mustDate(t, "2026-07-21"),
		Price: decimal.RequireFromString("95.20"), Currency: "RUB", Source: "test",
	}

	// Drift face_currency to null while leaving face_value_minor set — the
	// only way to reach this state, since creation requires both or neither.
	resp := do(t, c, "PATCH", url+"/api/v1/instruments/"+bond.ID, `{"face_currency":null}`)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("PATCH instrument face_currency=null = %d: %s", resp.StatusCode, b)
	}

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"100","price":"950",
		"amount_minor":-9500000,"currency":"RUB"}`, acc.ID, bond.ID))

	resp = do(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/positions", "")
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET positions = %d: %s", resp.StatusCode, b)
	}
	var got positionsResp
	decodeJSON(t, resp, &got)
	if len(got.Positions) != 1 {
		t.Fatalf("positions = %+v, want exactly 1", got.Positions)
	}

	p := got.Positions[0]
	if p.MarketValueMinor != nil || p.MarketValueCurrency != nil || p.Price != nil || p.PriceOn != nil || p.UnrealizedPnlMinor != nil {
		t.Errorf("bond without face_currency = %+v, want market_value_minor/market_value_currency/price/price_on/unrealized_pnl_minor all null", p)
	}
}

// TestPositionsUnrealizedPnl covers unrealized_pnl_minor end to end: a share
// position in profit, one in a loss (unrealized_pnl_minor negative), a
// position with no quote at all (null, same as market_value_minor), a bond
// whose market valuation is denominated in a different currency than the
// position (market_value_minor present, unrealized_pnl_minor still null),
// and a fully closed position (quantity/cost both zero) that still has a
// quote — its unrealized_pnl_minor must come back as the integer 0, not
// null, since a valid subtraction (0 - 0) did happen.
func TestPositionsUnrealizedPnl(t *testing.T) {
	pool := testdb.New(t)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes)

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)

	profit := createInstrument(t, c, url, `{"type":"share","name":"В плюсе","ticker":"PROFIT","currency":"RUB"}`)
	loss := createInstrument(t, c, url, `{"type":"share","name":"В минусе","ticker":"LOSS","currency":"RUB"}`)
	noQuote := createInstrument(t, c, url, `{"type":"share","name":"Без котировки","ticker":"UPNOQ","currency":"RUB"}`)
	bond := createInstrument(t, c, url,
		`{"type":"bond","name":"Валютная облигация","ticker":"UPBOND","currency":"RUB","face_value_minor":100000,"face_currency":"USD"}`)
	closed := createInstrument(t, c, url, `{"type":"share","name":"Закрыта","ticker":"UPCLOSED","currency":"RUB"}`)

	profitID, err := uuid.Parse(profit.ID)
	if err != nil {
		t.Fatalf("parse profit id: %v", err)
	}
	lossID, err := uuid.Parse(loss.ID)
	if err != nil {
		t.Fatalf("parse loss id: %v", err)
	}
	bondID, err := uuid.Parse(bond.ID)
	if err != nil {
		t.Fatalf("parse bond id: %v", err)
	}
	closedID, err := uuid.Parse(closed.ID)
	if err != nil {
		t.Fatalf("parse closed id: %v", err)
	}

	// profit: buy 10 @ 100.00 (cost_minor = 100_000, no fee), quote 150.00.
	// market_value_minor = 150.00 * 10 = 1_500.00 major = 150_000 minor.
	// unrealized_pnl_minor = 150_000 - 100_000 = 50_000 (profit).
	quotes.byInstrument[profitID] = marketdata.Quote{
		InstrumentID: profitID, On: mustDate(t, "2026-07-22"),
		Price: decimal.RequireFromString("150.00"), Currency: "RUB", Source: "test",
	}
	// loss: buy 10 @ 100.00 (cost_minor = 100_000, no fee), quote 60.00.
	// market_value_minor = 60.00 * 10 = 600.00 major = 60_000 minor.
	// unrealized_pnl_minor = 60_000 - 100_000 = -40_000 (loss, negative).
	quotes.byInstrument[lossID] = marketdata.Quote{
		InstrumentID: lossID, On: mustDate(t, "2026-07-22"),
		Price: decimal.RequireFromString("60.00"), Currency: "RUB", Source: "test",
	}
	// bond: buy 100 @ 950.00 (cost_minor = 9_500_000, no fee), quote 95.20%
	// of face. market_value_minor = 100_000 * 0.952 * 100 = 9_520_000, but
	// denominated in face_currency USD while the position (from the buy
	// operation's currency) is RUB — different currencies, so
	// unrealized_pnl_minor must be null despite market_value_minor existing.
	quotes.byInstrument[bondID] = marketdata.Quote{
		InstrumentID: bondID, On: mustDate(t, "2026-07-22"),
		Price: decimal.RequireFromString("95.20"), Currency: "RUB", Source: "test",
	}
	// closed: buy 4 @ 100.00 then sell 4 @ 100.00, fully closing the
	// position. quantity = 0, cost_minor = 0. A quote still exists (70.00),
	// so market_value_minor = 70.00 * 0 = 0 (ok=true, not absent). Both
	// currencies are RUB, so unrealized_pnl_minor = 0 - 0 = 0: a real
	// integer result, not null — this pins that closed positions still get
	// a computed (zero) unrealized_pnl_minor rather than one suppressed by
	// some qty==0 special case.
	quotes.byInstrument[closedID] = marketdata.Quote{
		InstrumentID: closedID, On: mustDate(t, "2026-07-22"),
		Price: decimal.RequireFromString("70.00"), Currency: "RUB", Source: "test",
	}
	// noQuote deliberately gets no entry in quotes.byInstrument.

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"RUB"}`, acc.ID, profit.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"RUB"}`, acc.ID, loss.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"5","price":"10",
		"amount_minor":-5000,"currency":"RUB"}`, acc.ID, noQuote.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"100","price":"950",
		"amount_minor":-9500000,"currency":"RUB"}`, acc.ID, bond.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"4","price":"100",
		"amount_minor":-40000,"currency":"RUB"}`, acc.ID, closed.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell",
		"occurred_on":"2026-07-02","quantity":"4","price":"100",
		"amount_minor":40000,"currency":"RUB"}`, acc.ID, closed.ID))

	resp := do(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/positions", "")
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET positions = %d: %s", resp.StatusCode, b)
	}
	var got positionsResp
	decodeJSON(t, resp, &got)
	if len(got.Positions) != 5 {
		t.Fatalf("positions = %+v, want exactly 5", got.Positions)
	}

	byID := make(map[string]positionResp, len(got.Positions))
	for _, p := range got.Positions {
		byID[p.Instrument.Id] = p
	}

	profitPos, ok := byID[profit.ID]
	if !ok {
		t.Fatalf("no position for profit instrument")
	}
	if profitPos.UnrealizedPnlMinor == nil || *profitPos.UnrealizedPnlMinor != 50000 {
		t.Errorf("profit unrealized_pnl_minor = %v, want 50000", profitPos.UnrealizedPnlMinor)
	}

	lossPos, ok := byID[loss.ID]
	if !ok {
		t.Fatalf("no position for loss instrument")
	}
	if lossPos.UnrealizedPnlMinor == nil || *lossPos.UnrealizedPnlMinor != -40000 {
		t.Errorf("loss unrealized_pnl_minor = %v, want -40000", lossPos.UnrealizedPnlMinor)
	}

	noQuotePos, ok := byID[noQuote.ID]
	if !ok {
		t.Fatalf("no position for noQuote instrument")
	}
	if noQuotePos.MarketValueMinor != nil || noQuotePos.UnrealizedPnlMinor != nil {
		t.Errorf("noQuote position = %+v, want market_value_minor/unrealized_pnl_minor both null", noQuotePos)
	}

	bondPos, ok := byID[bond.ID]
	if !ok {
		t.Fatalf("no position for bond instrument")
	}
	if bondPos.MarketValueMinor == nil || *bondPos.MarketValueMinor != 9520000 {
		t.Errorf("bond market_value_minor = %v, want 9520000", bondPos.MarketValueMinor)
	}
	if bondPos.UnrealizedPnlMinor != nil {
		t.Errorf("bond unrealized_pnl_minor = %v, want null (market_value_currency USD != position currency RUB)", bondPos.UnrealizedPnlMinor)
	}

	closedPos, ok := byID[closed.ID]
	if !ok {
		t.Fatalf("no position for closed instrument")
	}
	if closedPos.Quantity != "0" || closedPos.CostMinor != 0 {
		t.Fatalf("closed position = %+v, want quantity=0 cost_minor=0", closedPos)
	}
	if closedPos.MarketValueMinor == nil || *closedPos.MarketValueMinor != 0 {
		t.Errorf("closed market_value_minor = %v, want 0 (not null)", closedPos.MarketValueMinor)
	}
	if closedPos.UnrealizedPnlMinor == nil || *closedPos.UnrealizedPnlMinor != 0 {
		t.Errorf("closed unrealized_pnl_minor = %v, want 0 (not null)", closedPos.UnrealizedPnlMinor)
	}
}

// mustDate parses a YYYY-MM-DD date for test fixtures, failing the test on
// a malformed literal rather than silently building a zero time.Time.
func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse date %q: %v", s, err)
	}
	return d
}
