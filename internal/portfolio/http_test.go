package portfolio_test

import (
	"context"
	"encoding/json"
	"errors"
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

// converterLike mirrors portfolio's unexported converter interface, for the
// same reason quoteStoreLike does: it lets this external test package name
// setupAPI's parameter type. A real *marketdata.Converter (backed by the
// same pool as the rest of the fixture, so tests can seed fx_rates directly
// via mdStore.UpsertFxRates and see them picked up here) satisfies it
// structurally.
type converterLike interface {
	Convert(ctx context.Context, amountMinor int64, from, to string, on time.Time) (int64, error)
	Rate(ctx context.Context, from, to string, on time.Time) (decimal.Decimal, time.Time, error)
}

// setupAPI wires the full stack: family + account + instrument + operation +
// portfolio modules, mirroring how cmd/babki/root.go's mountModules
// assembles them (same fixture shape as operation/http_test.go's newAPI).
// quotes backs the portfolio handler's market-quote lookups. pool is
// provided by the caller (rather than created here) so tests that need
// direct DB access for setup, or that just want the default real
// marketdata.Store, can share the same pool the HTTP stack runs on.
func setupAPI(t *testing.T, pool *pgxpool.Pool, quotes quoteStoreLike, conv converterLike) (string, *http.Client) {
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
	// The operation handler gets its own real converter rather than conv:
	// these tests substitute conv to control the PORTFOLIO handler's fx
	// behavior, and the journal's own in_base conversion is covered by
	// package operation's tests.
	operation.NewHandler(opSvc, opStore, famStore, marketdata.NewConverter(marketdata.NewStore(pool)), auth, sm).Mount(srv)
	portfolio.NewHandler(opStore, instStore, quotes, conv, famStore, auth, sm).Mount(srv)

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

// newAPI is setupAPI backed by a real (empty) marketdata.Store and a real,
// unseeded Converter, for tests that don't care about market valuation: no
// quotes ever exist, so all positions come back with null
// market_value_minor/price/price_on regardless of the converter.
func newAPI(t *testing.T) (string, *http.Client) {
	t.Helper()
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	return setupAPI(t, pool, mdStore, marketdata.NewConverter(mdStore))
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
	Id     string `json:"id"`
	Name   string `json:"name"`
	Ticker string `json:"ticker"`
}

type positionResp struct {
	Instrument                instrumentResp  `json:"instrument"`
	Quantity                  string          `json:"quantity"`
	CostMinor                 int64           `json:"cost_minor"`
	Currency                  string          `json:"currency"`
	RealizedPnlMinor          int64           `json:"realized_pnl_minor"`
	IncomeMinor               int64           `json:"income_minor"`
	FeesMinor                 int64           `json:"fees_minor"`
	MarketValueMinor          *int64          `json:"market_value_minor"`
	MarketValueCurrency       *string         `json:"market_value_currency"`
	MarketValueSourceCurrency *string         `json:"market_value_source_currency"`
	MarketValueSourceMinor    *int64          `json:"market_value_source_minor"`
	Price                     *string         `json:"price"`
	PriceOn                   *string         `json:"price_on"`
	UnrealizedPnlMinor        *int64          `json:"unrealized_pnl_minor"`
	HasUndatedLots            bool            `json:"has_undated_lots"`
	HasUndatedRealizations    bool            `json:"has_undated_realizations"`
	InBase                    *positionInBase `json:"in_base"`
}

// positionInBase mirrors apitypes.PositionInBase for decoding in tests (see
// http_position_in_base_test.go). A nil *positionInBase on positionResp
// covers both an omitted key and an explicit JSON null — which is exactly
// what in_base always is (see the handler's positionInBase: it is always
// explicitly set to either a value or null, never left unset, mirroring
// account.Handler.balanceInBase).
type positionInBase struct {
	CostMinor          int64  `json:"cost_minor"`
	MarketValueMinor   *int64 `json:"market_value_minor"`
	UnrealizedPnlMinor *int64 `json:"unrealized_pnl_minor"`
	IncomeMinor        int64  `json:"income_minor"`
	RealizedPnlMinor   *int64 `json:"realized_pnl_minor"`
	Currency           string `json:"currency"`
	RateOn             string `json:"rate_on"`
}

type positionsResp struct {
	Positions     []positionResp    `json:"positions"`
	RealizedTotal realizedTotalResp `json:"realized_total"`
}

// realizedTotalResp mirrors apitypes.RealizedTotal for decoding in tests (see
// http_realized_total_test.go). InBase and InBaseGap are pointers so that a
// test can tell an explicit null — the account has no publishable total, and
// the gap says why — apart from a figure, which a zero value could not.
type realizedTotalResp struct {
	ByCurrency   []realizedCurrencyTotalResp `json:"by_currency"`
	BaseCurrency string                      `json:"base_currency"`
	InBase       *int64                      `json:"in_base"`
	InBaseGap    *string                     `json:"in_base_gap"`
}

type realizedCurrencyTotalResp struct {
	Currency         string `json:"currency"`
	RealizedPnlMinor int64  `json:"realized_pnl_minor"`
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

	// --- acc2: no operations at all -> positions is [], not null. The
	// cost_basis_rules declaration rides along even here (see
	// TestAnEmptyAccountStillDeclaresTheRules), so the assertion is on the
	// positions key rather than on the whole body. ---
	resp = do(t, c, "GET", url+"/api/v1/accounts/"+acc2.ID+"/positions", "")
	if resp.StatusCode != 200 {
		t.Fatalf("GET positions acc2 = %d", resp.StatusCode)
	}
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"positions":[]`) {
		t.Errorf("acc2 positions body = %s, want an empty positions array (not null)", body)
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
	// No fx_rates seeded on this pool: the bond fixture below deliberately
	// has a face_currency (USD) that differs from its position currency
	// (RUB), so its market valuation exercises the no-rate-available
	// fallback path (see toAPI) — the same null-unrealized/raw-currency
	// behavior this test already asserted before conversion existed. The
	// conversion-succeeds path has its own dedicated test,
	// TestPositionsMarketValueConvertsToPositionCurrency.
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(marketdata.NewStore(pool)))

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
	// Regression: market_value_currency already equals the position's own
	// currency here (RUB == RUB), so no conversion ever happens and the
	// source fields — which exist purely to disclose a conversion that
	// occurred — must stay null. Wiring in a converter must not perturb the
	// already-matching-currency case.
	if sharePos.MarketValueSourceCurrency != nil || sharePos.MarketValueSourceMinor != nil {
		t.Errorf("share market_value_source_currency/_minor = %v/%v, want both null (currency already matched, no conversion)",
			sharePos.MarketValueSourceCurrency, sharePos.MarketValueSourceMinor)
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
	// No USD->RUB fx_rates row exists on this test's pool, so the currency
	// mismatch above hits the ErrNoRate fallback (see toAPI): market_value_*
	// stay in the raw face_currency (USD) and, since no conversion actually
	// happened, the source fields must stay null too — they are not just "the
	// raw value repeated", they mean "a conversion occurred".
	if bondPos.MarketValueSourceCurrency != nil || bondPos.MarketValueSourceMinor != nil {
		t.Errorf("bond market_value_source_currency/_minor = %v/%v, want both null (no fx rate, nothing converted)",
			bondPos.MarketValueSourceCurrency, bondPos.MarketValueSourceMinor)
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

// pastOn returns a date safely before "today" (mirroring
// account/http_summary_test.go's helper of the same name) so a seeded fx
// rate is always found by Store.FxRateOn's nearest-earlier-date lookup
// regardless of when the test actually runs, since production code converts
// using time.Now().UTC() as the "on" date (see toAPI).
func pastOn() time.Time {
	return time.Now().UTC().AddDate(0, -1, 0).Truncate(24 * time.Hour)
}

// TestPositionsMarketValueConvertsToPositionCurrency is the owner-requested
// fix at the center of this change: a bond whose raw market valuation
// (face_currency) differs from its position's own currency must have that
// valuation converted into the position's currency — via the real
// marketdata.Converter, exercising an actual fx_rates round trip, not a
// fake — rather than leaving unrealized_pnl_minor permanently null just
// because a rate happens to exist.
//
// Manual arithmetic, chosen so every step is exact (no rounding to verify
// separately from marketdata.Converter's own rounding tests):
//
//	bond: face_value_minor 100_000 (1_000.00 USD face), face_currency USD,
//	      quantity 1, quote price 100.00 (=100% of face, i.e. priced at par)
//	  market_value_source_minor (raw, USD) = 100_000 * (100.00/100) * 1
//	                                       = 100_000  (= $1_000.00)
//
//	fx rate USD->RUB = 90 (RUB per 1 USD)
//	  market_value_minor (converted, RUB) = 100_000 * 90 = 9_000_000
//	                                      (= 90_000.00 RUB, i.e. $1_000 * 90)
//
//	buy 1 @ (amount_minor -80_000, fee 0) -> cost_minor = 80_000 (800.00 RUB)
//	  unrealized_pnl_minor = 9_000_000 - 80_000 = 8_920_000 (89_200.00 RUB)
//
// If this test is run against a handler that still nulls out
// market_value/unrealized_pnl on any currency mismatch (the pre-fix
// behavior), every assertion below fails: market_value_minor would be
// 100_000/USD instead of 9_000_000/RUB, unrealized_pnl_minor would be null
// instead of 8_920_000, and the source fields wouldn't exist at all. That
// was confirmed by hand during development (temporarily reverting toAPI's
// conversion branch) before this test was finalized.
func TestPositionsMarketValueConvertsToPositionCurrency(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))

	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: pastOn(), Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rate: %v", err)
	}

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	bond := createInstrument(t, c, url,
		`{"type":"bond","name":"Облигация с конвертацией","ticker":"BONDFX","currency":"RUB","face_value_minor":100000,"face_currency":"USD"}`)
	bondID, err := uuid.Parse(bond.ID)
	if err != nil {
		t.Fatalf("parse bond id: %v", err)
	}
	quotes.byInstrument[bondID] = marketdata.Quote{
		InstrumentID: bondID, On: mustDate(t, "2026-07-21"),
		Price: decimal.RequireFromString("100.00"), Currency: "RUB", Source: "test",
	}
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"1","price":"800",
		"amount_minor":-80000,"currency":"RUB"}`, acc.ID, bond.ID))

	resp := do(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/positions", "")
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

	// market_value_minor/currency are now in the POSITION's currency (RUB),
	// directly comparable to cost_minor — not the raw face_currency (USD).
	if p.MarketValueMinor == nil || *p.MarketValueMinor != 9000000 {
		t.Errorf("market_value_minor = %v, want 9000000 (converted to RUB)", p.MarketValueMinor)
	}
	if p.MarketValueCurrency == nil || *p.MarketValueCurrency != "RUB" {
		t.Errorf("market_value_currency = %v, want RUB (the position's own currency, post-conversion)", p.MarketValueCurrency)
	}
	// The original, unconverted figure is preserved for transparency.
	if p.MarketValueSourceCurrency == nil || *p.MarketValueSourceCurrency != "USD" {
		t.Errorf("market_value_source_currency = %v, want USD (the raw face_currency valuation)", p.MarketValueSourceCurrency)
	}
	if p.MarketValueSourceMinor == nil || *p.MarketValueSourceMinor != 100000 {
		t.Errorf("market_value_source_minor = %v, want 100000 (the raw, unconverted USD valuation)", p.MarketValueSourceMinor)
	}
	// unrealized_pnl_minor is now populated — both operands are in RUB.
	if p.UnrealizedPnlMinor == nil || *p.UnrealizedPnlMinor != 8920000 {
		t.Errorf("unrealized_pnl_minor = %v, want 8920000 (9000000 - 80000)", p.UnrealizedPnlMinor)
	}
}

// TestPositionsMarketValueFallsBackWithoutRate is
// TestPositionsMarketValueConvertsToPositionCurrency's negative twin: same
// currency-mismatched bond, but with NO USD->RUB fx_rates row seeded on this
// test's own pool, so marketdata.Converter.Convert resolves to ErrNoRate.
// The handler must fall back to exactly the pre-conversion behavior — the
// raw valuation in its own (face) currency, no unrealized P&L, no source
// fields — rather than erroring the whole request or fabricating a rate.
func TestPositionsMarketValueFallsBackWithoutRate(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))
	// Deliberately no mdStore.UpsertFxRates call: no USD->RUB rate exists.

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	bond := createInstrument(t, c, url,
		`{"type":"bond","name":"Облигация без курса","ticker":"BONDNORATE","currency":"RUB","face_value_minor":100000,"face_currency":"USD"}`)
	bondID, err := uuid.Parse(bond.ID)
	if err != nil {
		t.Fatalf("parse bond id: %v", err)
	}
	quotes.byInstrument[bondID] = marketdata.Quote{
		InstrumentID: bondID, On: mustDate(t, "2026-07-21"),
		Price: decimal.RequireFromString("100.00"), Currency: "RUB", Source: "test",
	}
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"1","price":"800",
		"amount_minor":-80000,"currency":"RUB"}`, acc.ID, bond.ID))

	resp := do(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/positions", "")
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

	// Same raw figure as market_value_source_minor in the conversion test
	// above (100000/USD) — but here it's published as market_value_minor
	// itself, since nothing could be converted.
	if p.MarketValueMinor == nil || *p.MarketValueMinor != 100000 {
		t.Errorf("market_value_minor = %v, want 100000 (raw USD valuation, unconverted)", p.MarketValueMinor)
	}
	if p.MarketValueCurrency == nil || *p.MarketValueCurrency != "USD" {
		t.Errorf("market_value_currency = %v, want USD (no rate to convert to RUB)", p.MarketValueCurrency)
	}
	if p.MarketValueSourceCurrency != nil || p.MarketValueSourceMinor != nil {
		t.Errorf("market_value_source_currency/_minor = %v/%v, want both null (no conversion happened)",
			p.MarketValueSourceCurrency, p.MarketValueSourceMinor)
	}
	if p.UnrealizedPnlMinor != nil {
		t.Errorf("unrealized_pnl_minor = %v, want null (market_value_currency USD != position currency RUB, no rate to bridge them)", p.UnrealizedPnlMinor)
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
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(marketdata.NewStore(pool)))

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
	// No fx_rates seeded: the bond fixture below keeps exercising the
	// no-rate fallback (see TestPositionsMarketValuation's comment) so
	// unrealized_pnl_minor null on that currency mismatch stays covered.
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(marketdata.NewStore(pool)))

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

// failingConverter is a converter double whose fx lookups always fail with
// the given error — deliberately NOT marketdata.ErrNoRate, standing in for a
// genuine outage (a dropped DB connection, a canceled context) rather than
// the ordinary "this pair has no rate" outcome. It exists so a test can pin
// that the handler tells those two apart; a real *marketdata.Converter can't
// be made to fail on demand.
type failingConverter struct{ err error }

func (c failingConverter) Convert(_ context.Context, _ int64, _, _ string, _ time.Time) (int64, error) {
	return 0, c.err
}

func (c failingConverter) Rate(_ context.Context, _, _ string, _ time.Time) (decimal.Decimal, time.Time, error) {
	return decimal.Decimal{}, time.Time{}, c.err
}

// TestPositionsRealRateErrorFailsRequest pins the distinction the whole
// in_base contract rests on: a genuine failure while resolving the fx rate
// (DB down, context canceled) must fail the request, NOT be rendered as
// in_base: null.
//
// Both outcomes look identical on screen otherwise — the frontend shows the
// native amount with a "no rate" marker — so an outage would be presented to
// the user as ordinary, expected degradation, and nobody would ever learn
// the database had stopped answering. Only the status code tells them apart,
// which is why this test asserts on it: without it, deleting positionInBase's
// error propagation (returning null instead of the error) breaks nothing
// visible and no other test notices.
func TestPositionsRealRateErrorFailsRequest(t *testing.T) {
	pool := testdb.New(t)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, failingConverter{err: errors.New("connection reset by peer")})

	// USD account in an RUB-based space: the position's currency differs
	// from the base currency, so in_base conversion is actually attempted
	// (Rate is called) instead of being short-circuited.
	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"USD"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, acc.ID, share.ID))

	resp := do(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/positions", "")
	if resp.StatusCode != http.StatusInternalServerError {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET positions with a failing rate lookup = %d, want 500 — a real outage must not be served as a 200 with in_base: null: %s",
			resp.StatusCode, b)
	}
}
