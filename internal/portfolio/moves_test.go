package portfolio_test

import (
	"fmt"
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

// A dollar account's 2026 for the report on an account abroad (decision Р-45):
// $2 000 in and 10 Apple at $100 in 2025 leave $1 000 at the start; 2026 brings
// $750 of a sale less a $1 fee, a $10 dividend, and takes a $1 tax and $500
// out — $759 in, $501 out, $1 258 at the end. The papers: 10 at $180 on the
// last day of 2025, 5 at $200 on the last of 2026.
func TestTheYearsMoves(t *testing.T) {
	pool := testdb.New(t)
	md := marketdata.NewStore(pool)
	conv := marketdata.NewConverter(md)
	url, c := setupAPI(t, pool, md, conv)
	acc := createAccount(t, c, url, `{"name":"Freedom","type":"brokerage","currency":"USD"}`)
	apple := createInstrument(t, c, url, `{"type":"share","name":"Apple","ticker":"AAPL","currency":"USD"}`)
	op := func(body string) {
		createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,%s}`, acc.ID, body))
	}
	op(`"type":"deposit","occurred_on":"2025-03-01","amount_minor":200000,"currency":"USD"`)
	op(fmt.Sprintf(`"instrument_id":%q,"type":"buy","occurred_on":"2025-03-02","quantity":"10","price":"100","amount_minor":-100000,"currency":"USD"`, apple.ID))
	op(fmt.Sprintf(`"instrument_id":%q,"type":"sell","occurred_on":"2026-02-10","quantity":"5","price":"150","amount_minor":75000,"fee_minor":100,"currency":"USD"`, apple.ID))
	op(fmt.Sprintf(`"instrument_id":%q,"type":"dividend","occurred_on":"2026-05-15","amount_minor":1000,"currency":"USD"`, apple.ID))
	op(fmt.Sprintf(`"instrument_id":%q,"type":"tax","occurred_on":"2026-05-15","amount_minor":-100,"currency":"USD"`, apple.ID))
	op(`"type":"withdrawal","occurred_on":"2026-09-01","amount_minor":-50000,"currency":"USD"`)
	quote := func(on, price string) marketdata.Quote {
		return marketdata.Quote{InstrumentID: uuid.MustParse(apple.ID), On: yearDay(on), Price: decimal.RequireFromString(price), Currency: "USD", Source: "test"}
	}
	if err := md.UpsertQuotes(t.Context(), []marketdata.Quote{quote("2025-12-30", "180"), quote("2026-12-30", "200")}); err != nil {
		t.Fatal(err)
	}
	var space uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT id FROM spaces LIMIT 1`).Scan(&space); err != nil {
		t.Fatal(err)
	}
	svc := portfolio.NewService(operation.NewStore(pool), instrument.NewStore(pool), md, conv, family.NewStore(pool))
	got, err := svc.YearMoves(t.Context(), space, uuid.MustParse(acc.ID), 2026)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Cash) != 1 {
		t.Fatalf("cash = %+v", got.Cash)
	}
	if m := got.Cash[0]; m.Currency != "USD" || m.Start != 100_000 || m.In != 75_900 || m.Out != 50_100 || m.End != 125_800 {
		t.Errorf("dollars = %+v; want 1 000 at the start, 759 in, 501 out, 1 258 at the end", m)
	}
	if got.PapersStart["USD"] != 180_000 || got.PapersEnd["USD"] != 100_000 || got.Unpriced != 0 {
		t.Errorf("papers %v → %v, unpriced %d; want $1 800 → $1 000", got.PapersStart, got.PapersEnd, got.Unpriced)
	}
}
