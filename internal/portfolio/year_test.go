package portfolio_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/portfolio"
)

// A year as an estimate of its tax reads it (decision Р-41): 150 of 200
// shares sold in 2026 for 45 000 — the 100 of 2020 at 10 000, held over three
// years, gain 20 000, and 50 of the 100 of 2025 at 10 000 gain 5 000; a
// dollar coupon at the day's rate; a dividend; a tax row taken as withheld;
// a sale of 2025 and a dividend of 2027 are not the year's.
func TestTheYearsFacts(t *testing.T) {
	pool := testdb.New(t)
	md := marketdata.NewStore(pool)
	conv := marketdata.NewConverter(md)
	url, c := setupAPI(t, pool, md, conv)
	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	paper := createInstrument(t, c, url, `{"type":"share","name":"Сбер","ticker":"SBER","currency":"RUB"}`)
	bond := createInstrument(t, c, url, `{"type":"bond","name":"Евробонд","ticker":"XS1","currency":"USD"}`)
	if err := md.UpsertFxRates(t.Context(), []marketdata.FxRate{{Base: "USD", Quote: "RUB", On: yearDay("2026-01-05"), Rate: decimal.NewFromInt(80), Source: "test"}}); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{
		`{"account_id":%q,"instrument_id":%q,"type":"buy","occurred_on":"2020-02-10","quantity":"100","price":"100","amount_minor":-1000000,"currency":"RUB"}`,
		`{"account_id":%q,"instrument_id":%q,"type":"buy","occurred_on":"2025-02-10","quantity":"110","price":"200","amount_minor":-2200000,"currency":"RUB"}`,
		`{"account_id":%q,"instrument_id":%q,"type":"sell","occurred_on":"2025-06-10","quantity":"10","price":"200","amount_minor":200000,"currency":"RUB"}`,
		`{"account_id":%q,"instrument_id":%q,"type":"sell","occurred_on":"2026-03-10","quantity":"150","price":"300","amount_minor":4500000,"currency":"RUB"}`,
		`{"account_id":%q,"instrument_id":%q,"type":"dividend","occurred_on":"2026-07-01","amount_minor":300000,"currency":"RUB"}`,
	} {
		createOperation(t, c, url, fmt.Sprintf(op, acc.ID, paper.ID))
	}
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"coupon","occurred_on":"2026-02-01","amount_minor":5000,"currency":"USD"}`, acc.ID, bond.ID))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"type":"tax","occurred_on":"2026-07-01","amount_minor":-39000,"currency":"RUB"}`, acc.ID))

	var space uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT id FROM spaces LIMIT 1`).Scan(&space); err != nil {
		t.Fatal(err)
	}
	svc := portfolio.NewService(operation.NewStore(pool), instrument.NewStore(pool), md, conv, family.NewStore(pool))
	got, err := svc.YearFacts(t.Context(), space, uuid.MustParse(acc.ID), 2026, "RUB", 3)
	if err != nil {
		t.Fatal(err)
	}
	// The 2025 sale took 10 of the 2020 lot first: 90 of it left, so 2026's
	// 150 are 90 of 2020 (cost 9 000) and 60 of 2025 (cost 12 000) for 45 000:
	// 27 000 − 9 000 = 18 000 long, 18 000 − 12 000 = 6 000.
	want := portfolio.YearFacts{Sales: 2_400_000, SalesLong: 1_800_000, Coupons: 400_000, Dividends: 300_000, Withheld: 39_000}
	if got != want {
		t.Errorf("2026 = %+v, want %+v", got, want)
	}
	if got, _ := svc.YearFacts(t.Context(), space, uuid.MustParse(acc.ID), 2025, "RUB", 3); got.Sales != 100_000 || got.SalesLong != 100_000 || got.Dividends != 0 {
		t.Errorf("2025 = %+v: the 10 of 2020 sold for 2 000 against 1 000, held over three years", got)
	}
}

func yearDay(s string) time.Time {
	d, _ := time.Parse(time.DateOnly, s)
	return d
}
