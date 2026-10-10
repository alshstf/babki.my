package portfolio_test

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/portfolio"
)

// A year of an account: 100 shares bought at 1 000 ₽, 50 000 ₽ added in
// January, 10 shares arriving from another broker in February, a 3 000 ₽
// dividend, the price at 1 100 ₽ by the end. The deposit and the arrival are
// money put in; the dividend and the price are what was earned.
func TestAPeriodsReturnIsReckonedFromItsEdges(t *testing.T) {
	pool := testdb.New(t)
	md := marketdata.NewStore(pool)
	conv := marketdata.NewConverter(md)
	url, c := setupAPI(t, pool, &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}, conv)
	ctx := t.Context()

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	sber := createInstrument(t, c, url, `{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)
	op := func(body string) { createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,%s}`, acc.ID, body)) }
	op(`"type":"deposit","occurred_on":"2025-06-30","amount_minor":10000000,"currency":"RUB"`)
	op(fmt.Sprintf(`"instrument_id":%q,"type":"buy","occurred_on":"2025-06-30","quantity":"100","price":"1000","currency":"RUB"`, sber.ID))
	op(`"type":"deposit","occurred_on":"2026-01-01","amount_minor":5000000,"currency":"RUB"`)
	op(fmt.Sprintf(`"instrument_id":%q,"type":"dividend","occurred_on":"2026-03-01","amount_minor":300000,"currency":"RUB"`, sber.ID))
	resp, err := c.Post(url+"/api/v1/operations/arrivals", "application/json", strings.NewReader(fmt.Sprintf(
		`{"account_id":%q,"instrument_id":%q,"occurred_on":"2026-02-02","quantity":"10","currency":"RUB"}`, acc.ID, sber.ID)))
	if err != nil || resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("arrival: %v %s", err, b)
	}
	q := func(on, price string) marketdata.Quote {
		return marketdata.Quote{InstrumentID: uuid.MustParse(sber.ID), On: mustDate(t, on), Price: decimal.RequireFromString(price), Currency: "RUB", Source: "test"}
	}
	if err := md.UpsertQuotes(ctx, []marketdata.Quote{q("2025-06-30", "1000"), q("2026-01-01", "1000"), q("2026-02-02", "1050"), q("2026-06-30", "1100")}); err != nil {
		t.Fatal(err)
	}

	var spaceID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM spaces LIMIT 1`).Scan(&spaceID); err != nil {
		t.Fatal(err)
	}
	h := portfolio.NewService(operation.NewStore(pool), instrument.NewStore(pool), md, conv, family.NewStore(pool))
	from, to := mustDate(t, "2025-06-30"), mustDate(t, "2026-06-30")
	b, err := h.ReturnBasis(ctx, spaceID, uuid.MustParse(acc.ID), from, to)
	if err != nil {
		t.Fatal(err)
	}
	// Start: 100 × 1 000. End: 110 × 1 100 + 50 000 + 3 000.
	if b.Start.Minor != 10_000_000 || b.End.Minor != 17_400_000 || !b.Complete {
		t.Fatalf("basis = %+v, want 100 000 at the start and 174 000 at the end, complete", b)
	}
	if len(b.Flows) != 2 || b.Flows[0].Minor != -5_000_000 || b.Flows[1].Minor != -1_050_000 {
		t.Errorf("flows = %+v, want the January deposit and the arrival at 10 × 1 050", b.Flows)
	}
	// 174 000 − 100 000 − 50 000 − 10 500: 10 000 + 500 on the price, 3 000 of dividend.
	if profit, err := b.Profit(); err != nil || profit != 1_350_000 {
		t.Errorf("profit = %d, %v; want 13 500 ₽", profit, err)
	}
	rate, ok := b.AnnualRate(from, to)
	if !ok || rate < 0.10 || rate > 0.12 {
		t.Errorf("annual rate = %v, %v; want a little over 10%%", rate, ok)
	}

	// Time-weighted (#405): 100 000 to 150 000 with 50 000 put in — flat; to
	// 165 500 with 10 500 of shares in — 155 000 / 150 000; to 174 000 —
	// 174 000 / 165 500. Chained: 8.64 %, the money's timing aside.
	twr, ok, err := h.TimeWeighted(ctx, spaceID, uuid.MustParse(acc.ID), b.Flows, from, to)
	if err != nil || !ok || math.Abs(twr-(155_000.0/150_000*174_000/165_500-1)) > 1e-6 {
		t.Errorf("time-weighted = %v, %v, %v; want 8.64 %%", twr, ok, err)
	}

	// A paper with no price at the end makes the period incomplete.
	if err := md.UpsertQuotes(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if b, err := h.ReturnBasis(ctx, spaceID, uuid.MustParse(acc.ID), from, mustDate(t, "2026-09-30")); err != nil || b.Complete {
		t.Errorf("a period ending where the price is three months old = %+v, %v; want incomplete", b, err)
	}
}
