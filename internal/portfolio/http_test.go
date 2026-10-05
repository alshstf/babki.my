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
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/portfolio"
)

// quoteStoreLike mirrors the handler's unexported quoteStore so this external
// package can name setupAPI's parameter.
type quoteStoreLike interface {
	LatestQuotes(ctx context.Context, instrumentIDs []uuid.UUID) (map[uuid.UUID]marketdata.Quote, error)
}

// converterLike mirrors the handler's unexported converter, as above.
type converterLike interface {
	Rate(ctx context.Context, from, to string, on time.Time) (decimal.Decimal, time.Time, error)
	RatesOn(ctx context.Context, queries []marketdata.RateQuery) (marketdata.Rates, error)
}

// journalStoreLike, instrumentStoreLike and spaceStoreLike mirror the rest of
// the handler's dependencies.
type (
	journalStoreLike interface {
		ListForEngine(ctx context.Context, spaceID, accountID uuid.UUID) ([]portfolio.Operation, error)
	}
	instrumentStoreLike interface {
		ByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]instrument.Instrument, error)
	}
	spaceStoreLike interface {
		SpaceByID(ctx context.Context, id uuid.UUID) (family.Space, error)
	}
)

// portfolioStores are the stores setupAPI hands the handler, named so a test
// can interpose counting doubles.
type portfolioStores struct {
	ops         journalStoreLike
	instruments instrumentStoreLike
	spaces      spaceStoreLike
}

// setupAPI wires family, account, instrument, operation and portfolio as
// cmd/babki does. The caller provides pool and quotes. wrap, when given,
// replaces the portfolio handler's stores only, so a double counts one
// screen's round trips and not the fixture's writes.
func setupAPI(t *testing.T, pool *pgxpool.Pool, quotes quoteStoreLike, conv converterLike, wrap ...func(portfolioStores) portfolioStores) (string, *http.Client) {
	t.Helper()
	famStore := family.NewStore(pool)
	famSvc := family.NewService(famStore)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)

	instStore := instrument.NewStore(pool)
	opStore := operation.NewStore(pool)
	opSvc := operation.NewService(opStore)

	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(famSvc, famStore, auth, sm).Mount(srv)
	account.NewHandler(account.NewStore(pool), famStore, marketdata.NewConverter(marketdata.NewStore(pool)), nil, auth, sm).Mount(srv)
	instrument.NewHandler(instStore, auth, sm).Mount(srv)
	// The operation handler gets its own real converter; conv controls only the
	// portfolio handler.
	operation.NewHandler(opSvc, opStore, famStore, marketdata.NewConverter(marketdata.NewStore(pool)), auth, sm).Mount(srv)

	stores := portfolioStores{ops: opStore, instruments: instStore, spaces: famStore}
	for _, w := range wrap {
		stores = w(stores)
	}
	portfolio.NewHandler(stores.ops, stores.instruments, quotes, conv, stores.spaces, auth, sm).Mount(srv)

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

// newAPI is setupAPI with an empty quote store and an unseeded converter: no
// quotes, so no valuations.
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
	Instrument                instrumentResp   `json:"instrument"`
	Quantity                  string           `json:"quantity"`
	CostMinor                 int64            `json:"cost_minor"`
	Currency                  string           `json:"currency"`
	RealizedPnlMinor          *int64           `json:"realized_pnl_minor"`
	SettledMinor              *int64           `json:"settled_minor"`
	TotalMinor                *int64           `json:"total_minor"`
	IncomeMinor               int64            `json:"income_minor"`
	IncomeByCurrency          []currencyIncome `json:"income_by_currency"`
	FeesMinor                 int64            `json:"fees_minor"`
	MarketValueMinor          *int64           `json:"market_value_minor"`
	MarketValueCurrency       *string          `json:"market_value_currency"`
	MarketValueSourceCurrency *string          `json:"market_value_source_currency"`
	MarketValueSourceMinor    *int64           `json:"market_value_source_minor"`
	Price                     *string          `json:"price"`
	PriceOn                   *string          `json:"price_on"`
	PriceMoneyMinor           *int64           `json:"price_money_minor"`
	UnrealizedPnlMinor        *int64           `json:"unrealized_pnl_minor"`
	HasUndatedLots            bool             `json:"has_undated_lots"`
	HasUndatedRealizations    bool             `json:"has_undated_realizations"`
	HasUnknownCost            bool             `json:"has_unknown_cost"`
	InBase                    *positionInBase  `json:"in_base"`
	// Pointers, so an explicit null can be told from a cause; the contract
	// requires both keys.
	InBaseGap      *string `json:"in_base_gap"`
	MarketValueGap *string `json:"market_value_gap"`
}

// currencyIncome mirrors apitypes.PositionCurrencyIncome.
type currencyIncome struct {
	Currency    string `json:"currency"`
	IncomeMinor int64  `json:"income_minor"`
}

// positionInBase mirrors apitypes.PositionInBase; nil covers absent and null
// alike.
type positionInBase struct {
	CostMinor          int64  `json:"cost_minor"`
	MarketValueMinor   *int64 `json:"market_value_minor"`
	UnrealizedPnlMinor *int64 `json:"unrealized_pnl_minor"`
	IncomeMinor        int64  `json:"income_minor"`
	RealizedPnlMinor   *int64 `json:"realized_pnl_minor"`
	Currency           string `json:"currency"`
	// A pointer, so null (no valuation) can be told from a date.
	RateOn *string `json:"rate_on"`
}

type positionsResp struct {
	Positions     []positionResp     `json:"positions"`
	RealizedTotal realizedTotalResp  `json:"realized_total"`
	AccountTotal  accountTotalResp   `json:"account_total"`
	Cash          []cashPositionResp `json:"cash"`
}

// cashPositionResp mirrors apitypes.CashPosition; base figures are pointers,
// since each may be null beside a gap.
type cashPositionResp struct {
	Currency    string `json:"currency"`
	AmountMinor int64  `json:"amount_minor"`
	InBase      struct {
		Currency           string  `json:"currency"`
		ValueMinor         *int64  `json:"value_minor"`
		CostMinor          *int64  `json:"cost_minor"`
		UnrealizedPnlMinor *int64  `json:"unrealized_pnl_minor"`
		RealizedPnlMinor   *int64  `json:"realized_pnl_minor"`
		Gap                *string `json:"gap"`
	} `json:"in_base"`
}

// accountTotalResp mirrors apitypes.AccountTotal; pointers tell "made nothing"
// from "no figure".
type accountTotalResp struct {
	ByCurrency               []accountCurrencyTotalResp `json:"by_currency"`
	BaseCurrency             string                     `json:"base_currency"`
	InBase                   *int64                     `json:"in_base"`
	InBaseGap                *string                    `json:"in_base_gap"`
	CashFxInBase             *int64                     `json:"cash_fx_in_base"`
	ZeroValuedPositions      int                        `json:"zero_valued_positions"`
	ZeroValuedCostByCurrency []currencyAmountResp       `json:"zero_valued_cost_by_currency"`
	NoRateCurrencies         []string                   `json:"no_rate_currencies"`
	UndatedPositions         int                        `json:"undated_positions"`
	UnknownCostPositions     int                        `json:"unknown_cost_positions"`
}

type accountCurrencyTotalResp struct {
	Currency    string `json:"currency"`
	AmountMinor *int64 `json:"amount_minor"`
}

// realizedTotalResp mirrors apitypes.RealizedTotal; pointers tell null from a
// figure.
type realizedTotalResp struct {
	ByCurrency            []realizedCurrencyTotalResp `json:"by_currency"`
	TaxWithheldByCurrency []currencyAmountResp        `json:"tax_withheld_by_currency"`
	BaseCurrency          string                      `json:"base_currency"`
	InBase                *int64                      `json:"in_base"`
	InBaseGap             *string                     `json:"in_base_gap"`
	UndatedPositions      int                         `json:"undated_positions"`
	UnknownCostPositions  int                         `json:"unknown_cost_positions"`
}

// currencyAmountResp mirrors apitypes.CurrencyAmount.
type currencyAmountResp struct {
	Currency    string `json:"currency"`
	AmountMinor int64  `json:"amount_minor"`
}

type realizedCurrencyTotalResp struct {
	Currency         string `json:"currency"`
	RealizedPnlMinor *int64 `json:"realized_pnl_minor"`
}

// Positions endpoint: an open position from a mixed journal; an account with
// no operations (an empty array, not null); a fully closed position, still
// listed with quantity "0".
func TestPositionsEndpoint(t *testing.T) {
	url, c := newAPI(t)

	acc1 := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	acc2 := createAccount(t, c, url, `{"name":"Пустой","type":"brokerage","currency":"RUB"}`)
	acc3 := createAccount(t, c, url, `{"name":"Закрытая позиция","type":"brokerage","currency":"RUB"}`)

	sber := createInstrument(t, c, url, `{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)
	lkoh := createInstrument(t, c, url, `{"type":"share","name":"Лукойл","ticker":"LKOH","currency":"RUB"}`)

	// acc1: deposit, two buys, a partial sell, a dividend.
	//
	// 	deposit          1_000_000              (cash-level, ignored)
	// 	buy  10 @ 100.00 −100_000 fee 10        lot1: 10, 100_010
	// 	buy  10 @ 110.00 −110_000 fee 11        lot2: 10, 110_011
	// 	sell  5 @ 120.00   60_000 fee  5        releases floor(100_010×5/10) = 50_005
	// 	                                        realized 60_000 − 50_005 − 5 = 9_990
	// 	dividend 5_000                          income 5_000
	// 	quantity 15, cost 160_016, realized 9_990, income 5_000, fees 26
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
	if realizedFigure(t, p.RealizedPnlMinor) != 9990 {
		t.Errorf("acc1 position realized_pnl_minor = %d, want 9990", realizedFigure(t, p.RealizedPnlMinor))
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
	// No quote, so the valuation fields are null.
	if p.MarketValueMinor != nil || p.Price != nil || p.PriceOn != nil || p.UnrealizedPnlMinor != nil {
		t.Errorf("acc1 position with no quote = %+v, want market_value_minor/price/price_on/unrealized_pnl_minor all null", p)
	}

	// acc2: no operations: positions is [], not null (the rules declaration is
	// still present, so only that key is checked).
	resp = do(t, c, "GET", url+"/api/v1/accounts/"+acc2.ID+"/positions", "")
	if resp.StatusCode != 200 {
		t.Fatalf("GET positions acc2 = %d", resp.StatusCode)
	}
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"positions":[]`) {
		t.Errorf("acc2 positions body = %s, want an empty positions array (not null)", body)
	}

	// acc3: bought and sold in full, still listed.
	// 	buy 3 @ 200.00 −60_000; sell 3 @ 210.00 63_000
	// 	quantity 0, cost 0, realized 3_000
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
	if realizedFigure(t, closed.RealizedPnlMinor) != 3000 {
		t.Errorf("acc3 closed position realized_pnl_minor = %d, want 3000", realizedFigure(t, closed.RealizedPnlMinor))
	}
}

// fakeQuoteStore is an in-memory quoteStore that counts LatestQuotes calls and
// omits instruments without a seeded quote, like the real store.
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

// Market valuation: a share at quote × quantity (rounding at .5), a bond at a
// percentage of face, an instrument without a quote, and a custom one with a
// quote but no model — all from one batched LatestQuotes call.
func TestPositionsMarketValuation(t *testing.T) {
	pool := testdb.New(t)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	// No rates: the bond's face currency (USD) differs from its position's (RUB),
	// so it takes the no-rate fallback.
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(marketdata.NewStore(pool)))

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)

	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"RUB"}`)
	// The face currency differs from the quote's own currency: a bond's quote is a
	// percentage, so the valuation must be in the face currency.
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

	// 100.005 × 1 = 10000.5 minor: rounds up to 10001.
	quotes.byInstrument[shareID] = marketdata.Quote{
		InstrumentID: shareID, On: mustDate(t, "2026-07-20"),
		Price: decimal.RequireFromString("100.005"), Currency: "RUB", Source: "test",
	}
	// 100000 × 95.2% × 100 = 9_520_000.
	quotes.byInstrument[bondID] = marketdata.Quote{
		InstrumentID: bondID, On: mustDate(t, "2026-07-21"),
		Price: decimal.RequireFromString("95.20"), Currency: "RUB", Source: "test",
	}
	// Custom has no valuation model despite the quote.
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
	// A share's valuation is in the quote's currency.
	if sharePos.MarketValueCurrency == nil || *sharePos.MarketValueCurrency != "RUB" {
		t.Errorf("share market_value_currency = %v, want RUB (the quote's currency)", sharePos.MarketValueCurrency)
	}
	if sharePos.Price == nil || *sharePos.Price != "100.005" {
		t.Errorf("share price = %v, want 100.005", sharePos.Price)
	}
	if sharePos.PriceOn == nil || *sharePos.PriceOn != "2026-07-20" {
		t.Errorf("share price_on = %v, want 2026-07-20", sharePos.PriceOn)
	}
	// Unrealized = 10001 − 10000 = 1.
	if sharePos.UnrealizedPnlMinor == nil || *sharePos.UnrealizedPnlMinor != 1 {
		t.Errorf("share unrealized_pnl_minor = %v, want 1", sharePos.UnrealizedPnlMinor)
	}
	// No conversion happened, so the source fields stay null.
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
	// The bond's valuation is in its face currency, not the quote's.
	if bondPos.MarketValueCurrency == nil || *bondPos.MarketValueCurrency != "USD" {
		t.Errorf("bond market_value_currency = %v, want USD (face_currency, not the quote's RUB)", bondPos.MarketValueCurrency)
	}
	// decimal normalizes trailing zeros: "95.2".
	if bondPos.Price == nil || *bondPos.Price != "95.2" {
		t.Errorf("bond price = %v, want 95.2", bondPos.Price)
	}
	if bondPos.PriceOn == nil || *bondPos.PriceOn != "2026-07-21" {
		t.Errorf("bond price_on = %v, want 2026-07-21", bondPos.PriceOn)
	}
	// One bond's money price: 1 000,00 face at 95.2% = 952,00, in the face currency;
	// a literal, since dividing the valuation would round twice.
	if bondPos.PriceMoneyMinor == nil || *bondPos.PriceMoneyMinor != 95200 {
		t.Errorf("bond price_money_minor = %v, want 95200", bondPos.PriceMoneyMinor)
	}
	// A share's quote is already money; no price_money_minor.
	if sharePos.PriceMoneyMinor != nil {
		t.Errorf("share price_money_minor = %v, want null (a share's price is already money)", sharePos.PriceMoneyMinor)
	}
	// The bond's currencies differ, so unrealized is null.
	if bondPos.UnrealizedPnlMinor != nil {
		t.Errorf("bond unrealized_pnl_minor = %v, want null (market_value_currency USD != position currency RUB)", bondPos.UnrealizedPnlMinor)
	}
	// No USD->RUB rate: the raw valuation is published and, since nothing was
	// converted, the source fields stay null.
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
	// The empty cell says which absence it is (#78): no quote.
	if noQuotePos.MarketValueGap == nil || *noQuotePos.MarketValueGap != "no_quote" {
		t.Errorf("noQuote market_value_gap = %v, want no_quote", noQuotePos.MarketValueGap)
	}

	customPos, ok := byID[custom.ID]
	if !ok {
		t.Fatalf("no position for custom instrument")
	}
	if customPos.MarketValueMinor != nil || customPos.MarketValueCurrency != nil || customPos.Price != nil || customPos.PriceOn != nil || customPos.UnrealizedPnlMinor != nil {
		t.Errorf("custom position (quote present, unsupported type) = %+v, want market_value_minor/market_value_currency/price/price_on/unrealized_pnl_minor all null", customPos)
	}
	// Same nulls for the opposite reason: a fresh quote, no model (#78).
	if customPos.MarketValueGap == nil || *customPos.MarketValueGap != "type_not_priced" {
		t.Errorf("custom market_value_gap = %v, want type_not_priced: a quote exists for this instrument", customPos.MarketValueGap)
	}
}

// pastOn returns a date safely before today, so a seeded rate is always
// found.
func pastOn() time.Time {
	return time.Now().UTC().AddDate(0, -1, 0).Truncate(24 * time.Hour)
}

// A bond valued in its face currency is converted into the position's
// currency through a real fx row.
//
//	bond: face 1 000 USD at par, quantity 1 -> source 100_000 USD
//	USD->RUB 90 -> market_value_minor 9_000_000 RUB
//	cost 80_000 RUB -> unrealized 8_920_000
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

	// The valuation is now in the position's currency.
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

// Three figures each round once, half away from zero, like Convert: the raw
// valuation, its conversion into the position's currency and its conversion
// into the base. The price and rates put all three on a half unit:
//
//	face 1 000 USD, quoted 99.9995% -> 99 999.5 -> 100 000 (truncation 99 999)
//	USD->EUR 0.900005 -> 90 000.5 -> 90 001 (truncation 90 000)
//	USD->RUB 0.440005 -> 44 000.5 -> 44 001 (truncation 44 000)
//
// The rates form no consistent triangle on purpose: a chained conversion
// would give 45 001 (#39).
func TestPositionsConvertedValuationRoundsHalfAwayFromZero(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))

	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "EUR", On: mustDate(t, "2026-01-01"), Rate: decimal.RequireFromString("0.900005"), Source: "test"},
		{Base: "EUR", Quote: "RUB", On: mustDate(t, "2026-01-01"), Rate: decimal.RequireFromString("0.5"), Source: "test"},
		{Base: "USD", Quote: "RUB", On: mustDate(t, "2026-01-01"), Rate: decimal.RequireFromString("0.440005"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rates: %v", err)
	}

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"EUR"}`)
	bond := createInstrument(t, c, url,
		`{"type":"bond","name":"Облигация","ticker":"BONDR","currency":"EUR","face_value_minor":100000,"face_currency":"USD"}`)
	bondID, err := uuid.Parse(bond.ID)
	if err != nil {
		t.Fatalf("parse bond id: %v", err)
	}
	quotes.byInstrument[bondID] = marketdata.Quote{
		InstrumentID: bondID, On: mustDate(t, "2026-07-21"),
		// Not par: the raw valuation must land on a half unit too.
		Price: decimal.RequireFromString("99.9995"), Currency: "EUR", Source: "test",
	}
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"1","price":"800",
		"amount_minor":-80000,"currency":"EUR"}`, acc.ID, bond.ID))

	p := onlyPosition(t, c, url, acc.ID)
	if p.MarketValueMinor == nil {
		t.Fatalf("market_value_minor = null, want 90001: the bond is quoted and its USD -> EUR rate is seeded")
	}
	if *p.MarketValueMinor != 90001 {
		t.Errorf("market_value_minor = %d, want 90001 (100000 * 0,900005 = 90000,5, rounded away from zero) — 90000 is truncation",
			*p.MarketValueMinor)
	}
	if p.InBase == nil {
		t.Fatalf("in_base = null, want the object: every rate this position needs is seeded")
	}
	if p.InBase.MarketValueMinor == nil {
		t.Fatalf("in_base.market_value_minor = null, want 44001: the USD -> RUB rate is seeded too")
	}
	if *p.InBase.MarketValueMinor != 44001 {
		t.Errorf("in_base.market_value_minor = %d, want 44001 (100000 * 0,440005 = 44000,5, rounded away from zero) — 44000 is truncation, 45001 is the valuation converted a second time through the euro",
			*p.InBase.MarketValueMinor)
	}
}

// Without a rate, the bond's valuation falls back to its raw face-currency
// figure, with no unrealized profit and no source fields.
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

	// The raw figure is published as market_value_minor itself.
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

// The face-value-without-currency case can no longer be reached through the
// API (#93); TestMarketValueNeedsBothHalvesOfTheFacePair covers it directly.

// Unrealized profit end to end: profit, loss, no quote (null), a bond valued
// in another currency (null), and a closed position with a quote (0, not
// null).
func TestPositionsUnrealizedPnl(t *testing.T) {
	pool := testdb.New(t)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	// No rates, so the bond stays on the fallback.
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

	// profit: cost 100_000, quote 150 -> value 150_000, unrealized 50_000.
	quotes.byInstrument[profitID] = marketdata.Quote{
		InstrumentID: profitID, On: mustDate(t, "2026-07-22"),
		Price: decimal.RequireFromString("150.00"), Currency: "RUB", Source: "test",
	}
	// loss: cost 100_000, quote 60 -> value 60_000, unrealized −40_000.
	quotes.byInstrument[lossID] = marketdata.Quote{
		InstrumentID: lossID, On: mustDate(t, "2026-07-22"),
		Price: decimal.RequireFromString("60.00"), Currency: "RUB", Source: "test",
	}
	// bond: value 9_520_000 in USD against a RUB position, so unrealized is null.
	quotes.byInstrument[bondID] = marketdata.Quote{
		InstrumentID: bondID, On: mustDate(t, "2026-07-22"),
		Price: decimal.RequireFromString("95.20"), Currency: "RUB", Source: "test",
	}
	// closed: quantity 0, cost 0, quote 70 -> value 0 and unrealized 0, not
	// null.
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

// mustDate parses a YYYY-MM-DD fixture date.
func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse date %q: %v", s, err)
	}
	return d
}

// failingConverter fails every lookup with a real error, not ErrNoRate.
type failingConverter struct{ err error }

func (c failingConverter) Rate(_ context.Context, _, _ string, _ time.Time) (decimal.Decimal, time.Time, error) {
	return decimal.Decimal{}, time.Time{}, c.err
}

func (c failingConverter) RatesOn(ctx context.Context, queries []marketdata.RateQuery) (marketdata.Rates, error) {
	return ratesFromRate(ctx, c, queries)
}

// rateResolver is the one-pair half of converterLike.
type rateResolver interface {
	Rate(ctx context.Context, from, to string, on time.Time) (decimal.Decimal, time.Time, error)
}

// ratesFromRate answers a batch from the double's own Rate, so a double cannot
// answer the batch and the pair differently. ErrNoRate stays with its query;
// anything else voids the batch, as RatesOn does.
func ratesFromRate(ctx context.Context, r rateResolver, queries []marketdata.RateQuery) (marketdata.Rates, error) {
	out := make(map[marketdata.RateQuery]marketdata.RateResult, len(queries))
	for _, q := range queries {
		rate, on, err := r.Rate(ctx, q.From, q.To, q.On)
		switch {
		case err == nil:
			out[q] = marketdata.RateResult{Rate: rate, RateDate: on}
		case errors.Is(err, marketdata.ErrNoRate):
			out[q] = marketdata.RateResult{Err: err}
		default:
			return marketdata.Rates{}, err
		}
	}
	return marketdata.NewRates(out), nil
}

// A real rate failure fails the request rather than showing in_base: null.
func TestPositionsRealRateErrorFailsRequest(t *testing.T) {
	pool := testdb.New(t)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, failingConverter{err: errors.New("connection reset by peer")})

	// A USD account in a RUB space, so conversion is attempted.
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

// realizedFigure dereferences a realized figure and fails on null, which means
// no figure in one currency, not zero.
func realizedFigure(t *testing.T, minor *int64) int64 {
	t.Helper()
	if minor == nil {
		t.Fatalf("realized_pnl_minor is null: the payload says this has no result in one currency")
	}
	return *minor
}
