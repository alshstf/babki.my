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

// An account is valued on a past day from the journal as it then stood: what
// it held that day at that day's closing prices, its cash, each currency at
// that day's rate. A price more than ten days older than the day is no price.
func TestAnAccountIsValuedOnAPastDay(t *testing.T) {
	pool := testdb.New(t)
	md := marketdata.NewStore(pool)
	conv := marketdata.NewConverter(md)
	url, c := setupAPI(t, pool, &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}, conv)
	ctx := t.Context()
	if err := md.UpsertFxRates(ctx, []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: mustDate(t, "2026-07-01"), Rate: decimal.RequireFromString("80"), Source: "test"},
		{Base: "USD", Quote: "RUB", On: mustDate(t, "2026-08-01"), Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatal(err)
	}

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	acme := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	sber := createInstrument(t, c, url, `{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)
	op := func(body string) { createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,%s}`, acc.ID, body)) }
	op(`"type":"deposit","occurred_on":"2026-07-01","amount_minor":100000,"currency":"USD"`)
	op(`"type":"deposit","occurred_on":"2026-07-01","amount_minor":500000,"currency":"RUB"`)
	op(fmt.Sprintf(`"instrument_id":%q,"type":"buy","occurred_on":"2026-07-10","quantity":"5","price":"100","currency":"USD"`, acme.ID))
	op(fmt.Sprintf(`"instrument_id":%q,"type":"buy","occurred_on":"2026-08-05","quantity":"10","price":"300","currency":"RUB"`, sber.ID))
	quote := func(id, on, price, currency string) marketdata.Quote {
		return marketdata.Quote{InstrumentID: uuid.MustParse(id), On: mustDate(t, on), Price: decimal.RequireFromString(price), Currency: currency, Source: "test"}
	}
	if err := md.UpsertQuotes(ctx, []marketdata.Quote{
		quote(acme.ID, "2026-07-10", "100", "USD"),
		quote(acme.ID, "2026-07-31", "110", "USD"),
		quote(sber.ID, "2026-08-05", "300", "RUB"),
		quote(sber.ID, "2026-08-18", "310", "RUB"),
	}); err != nil {
		t.Fatal(err)
	}

	var spaceID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM spaces LIMIT 1`).Scan(&spaceID); err != nil {
		t.Fatal(err)
	}
	h := portfolio.NewService(operation.NewStore(pool), instrument.NewStore(pool), md, conv, family.NewStore(pool))
	for _, tc := range []struct {
		day           string
		minor         int64
		unpriced, ops int
	}{
		// Before anything: nothing to value.
		{"2026-06-30", 0, 0, 0},
		// Cash only: 1 000 $ × 80 + 5 000 ₽.
		{"2026-07-05", 8_500_000, 0, 2},
		// ACME 5 × 100 $ + 500 $ cash = 1 000 $ × 80, + 5 000 ₽.
		{"2026-07-10", 8_500_000, 0, 3},
		// The 31 July close (110) the next day, at 1 August's rate:
		// (550 + 500) $ × 90 + 5 000 ₽.
		{"2026-08-01", 9_950_000, 0, 3},
		// SBER bought at 300; ACME's price is from 31 July, still within ten days.
		// (550 + 500) $ × 90 + 10 × 300 + 2 000 ₽.
		{"2026-08-05", 9_950_000, 0, 4},
		// Twenty days on, ACME's last price is too old to value it; SBER's is
		// from two days before: 10 × 310 + 2 000 ₽ + 500 $ × 90.
		{"2026-08-20", 5_010_000, 1, 4},
	} {
		got, err := h.ValueOn(ctx, spaceID, uuid.MustParse(acc.ID), mustDate(t, tc.day))
		if err != nil {
			t.Fatalf("%s: %v", tc.day, err)
		}
		if got.Minor != tc.minor || got.Unpriced != tc.unpriced || got.Operations != tc.ops {
			t.Errorf("%s = %+v, want %d with %d unpriced from %d operations", tc.day, got, tc.minor, tc.unpriced, tc.ops)
		}
	}
}
