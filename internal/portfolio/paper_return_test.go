package portfolio_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/testdb"
)

type periodResp struct {
	Currency           string  `json:"currency"`
	StartMinor         int64   `json:"start_minor"`
	EndMinor           int64   `json:"end_minor"`
	ContributionsMinor int64   `json:"contributions_minor"`
	ProfitMinor        int64   `json:"profit_minor"`
	AnnualRate         *string `json:"annual_rate"`
	Complete           bool    `json:"complete"`
}

func paperReturn(t *testing.T, c *http.Client, url, id, from, to string) (int, periodResp) {
	t.Helper()
	resp := do(t, c, "GET", fmt.Sprintf("%s/api/v1/instruments/%s/return?from=%s&to=%s", url, id, from, to), "")
	var out periodResp
	if resp.StatusCode == http.StatusOK {
		decodeJSON(t, resp, &out)
	}
	return resp.StatusCode, out
}

// One paper's period across the family: bought on one account, partly moved to
// another (no flow — it stays in the family), paid a dividend less tax, partly
// sold, and joined by shares from another broker at that day's closing price.
// The profit is the worth at the end less what went in net of what came out.
func TestAPapersReturnAcrossTheFamily(t *testing.T) {
	pool := testdb.New(t)
	md := marketdata.NewStore(pool)
	url, c := setupAPI(t, pool, md, marketdata.NewConverter(md))
	a := createAccount(t, c, url, `{"name":"А","type":"brokerage","currency":"RUB"}`).ID
	b := createAccount(t, c, url, `{"name":"Б","type":"brokerage","currency":"RUB"}`).ID
	cAcc := createAccount(t, c, url, `{"name":"В","type":"brokerage","currency":"RUB"}`).ID
	sber := createInstrument(t, c, url, `{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`).ID
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"deposit","occurred_on":"2026-01-10","amount_minor":1000000,"currency":"RUB"}`, a))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy","occurred_on":"2026-01-10","quantity":"10","price":"100","fee_minor":100,"currency":"RUB"}`, a, sber))
	if resp := do(t, c, "POST", url+"/api/v1/operations/transfer", fmt.Sprintf(
		`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"2","occurred_on":"2026-02-01"}`, a, b, sber)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("transfer = %d", resp.StatusCode)
	}
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend","occurred_on":"2026-02-15","amount_minor":5000,"currency":"RUB"}`, a, sber))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"tax","occurred_on":"2026-02-15","amount_minor":-650,"currency":"RUB"}`, a, sber))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"sell","occurred_on":"2026-03-01","quantity":"4","price":"120","currency":"RUB"}`, a, sber))
	if resp := do(t, c, "POST", url+"/api/v1/operations/arrivals", fmt.Sprintf(
		`{"account_id":%q,"instrument_id":%q,"occurred_on":"2026-03-15","quantity":"3","currency":"RUB"}`, cAcc, sber)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("arrival = %d", resp.StatusCode)
	}
	id := uuid.MustParse(sber)
	if err := md.UpsertQuotes(t.Context(), []marketdata.Quote{
		{InstrumentID: id, On: mustDate(t, "2026-02-27"), Price: decimal.RequireFromString("130"), Currency: "RUB", Source: "moex_history"},
		{InstrumentID: id, On: mustDate(t, "2026-03-15"), Price: decimal.RequireFromString("140"), Currency: "RUB", Source: "moex_history"},
		{InstrumentID: id, On: mustDate(t, "2026-03-31"), Price: decimal.RequireFromString("150"), Currency: "RUB", Source: "moex_history"},
	}); err != nil {
		t.Fatal(err)
	}

	code, got := paperReturn(t, c, url, sber, "2026-01-09", "2026-03-31")
	if code != http.StatusOK {
		t.Fatalf("return = %d", code)
	}
	// End: 4 on А, 2 on Б, 3 on В at 150 ₽. Flows: −1001 ₽ bought with its
	// commission, +50 ₽
	// dividend, −6,50 ₽ tax, +480 ₽ sold, −420 ₽ arrived at 140 ₽.
	want := periodResp{Currency: "RUB", StartMinor: 0, EndMinor: 135_000, ContributionsMinor: 89_750, ProfitMinor: 45_250, Complete: true}
	if got.Currency != want.Currency || got.StartMinor != want.StartMinor || got.EndMinor != want.EndMinor ||
		got.ContributionsMinor != want.ContributionsMinor || got.ProfitMinor != want.ProfitMinor || got.Complete != want.Complete {
		t.Errorf("return = %+v, want %+v", got, want)
	}
	if got.AnnualRate == nil {
		t.Error("no annual rate for a complete period")
	}

	// A later period starts from what was held then — ten units at 130 ₽ — and
	// counts only what moved after it began: the sale and the arrival.
	if _, got := paperReturn(t, c, url, sber, "2026-02-28", "2026-03-31"); got.StartMinor != 130_000 || got.ProfitMinor != 11_000 || !got.Complete {
		t.Errorf("March = %+v, want a start of 130000 and a profit of 11000", got)
	}

	// A period whose end has no price is not complete.
	if _, got := paperReturn(t, c, url, sber, "2026-01-09", "2026-05-31"); got.Complete {
		t.Errorf("a period ending two months past the last price: %+v, want it incomplete", got)
	}
}

// A spin-off carries part of the paper's worth onto another paper, which no
// closing price says: the period is not complete.
func TestAPapersPeriodWithASpinoffIsNotComplete(t *testing.T) {
	pool := testdb.New(t)
	md := marketdata.NewStore(pool)
	url, c := setupAPI(t, pool, md, marketdata.NewConverter(md))
	a := createAccount(t, c, url, `{"name":"А","type":"brokerage","currency":"RUB"}`).ID
	fund := createInstrument(t, c, url, `{"type":"etf","name":"Фонд","ticker":"FUND","currency":"RUB"}`).ID
	carved := createInstrument(t, c, url, `{"type":"etf","name":"Фонд (заблокированное)","ticker":"FUNDZ","currency":"RUB"}`).ID
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"deposit","occurred_on":"2026-01-10","amount_minor":1000000,"currency":"RUB"}`, a))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy","occurred_on":"2026-01-10","quantity":"10","price":"100","currency":"RUB"}`, a, fund))
	svc := operation.NewService(operation.NewStore(pool))
	if _, _, err := svc.CreateSpinoff(context.Background(), spaceOf(t, pool), operation.SpinoffParams{
		AccountID: uuid.MustParse(a), FromInstrumentID: uuid.MustParse(fund), ToInstrumentID: uuid.MustParse(carved),
		RatioFrom: decimal.NewFromInt(1), RatioTo: decimal.NewFromInt(1), BasisShare: decimal.RequireFromString("0.2"),
		OccurredOn: mustDate(t, "2026-02-01"), Source: operation.SourceRegistry,
	}); err != nil {
		t.Fatalf("spin-off: %v", err)
	}
	if err := md.UpsertQuotes(t.Context(), []marketdata.Quote{
		{InstrumentID: uuid.MustParse(fund), On: mustDate(t, "2026-03-31"), Price: decimal.RequireFromString("90"), Currency: "RUB", Source: "moex_history"},
	}); err != nil {
		t.Fatal(err)
	}
	if code, got := paperReturn(t, c, url, fund, "2026-01-09", "2026-03-31"); code != http.StatusOK || got.Complete {
		t.Errorf("a period with a spin-off = %d %+v, want 200 and incomplete", code, got)
	}
}

func TestAPapersReturnRefusesWhatTheAccountsDoes(t *testing.T) {
	pool := testdb.New(t)
	md := marketdata.NewStore(pool)
	url, c := setupAPI(t, pool, md, marketdata.NewConverter(md))
	sber := createInstrument(t, c, url, `{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`).ID
	if code, _ := paperReturn(t, c, url, uuid.NewString(), "2026-01-01", "2026-02-01"); code != http.StatusNotFound {
		t.Errorf("unknown paper = %d, want 404", code)
	}
	for _, q := range [][2]string{{"2026-02-01", "2026-01-01"}, {"2026-01-01", "2099-01-01"}, {"x", "2026-01-01"}} {
		if code, _ := paperReturn(t, c, url, sber, q[0], q[1]); code != http.StatusBadRequest {
			t.Errorf("from=%s to=%s = %d, want 400", q[0], q[1], code)
		}
	}
}

func spaceOf(t *testing.T, pool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
},
) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT id FROM spaces LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("read the space: %v", err)
	}
	return id
}
