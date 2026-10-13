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

// A dollar account's 2026 for a tax return (decision Р-45): 10 of Apple
// bought in March 2022 at $100 (rate 90) and 5 in 2025 at $120 (rate 100), 12
// sold in February 2026 at $150 with a $2 fee (rate 80) — the 10 of 2022 and 2
// of 2025, each at its own day's rate; a $10 dividend with $1 withheld on it
// (rate 85); the broker's $2 interest; a $0.50 tax on no payment.
func TestTheYearsRows(t *testing.T) {
	pool := testdb.New(t)
	md := marketdata.NewStore(pool)
	conv := marketdata.NewConverter(md)
	url, c := setupAPI(t, pool, md, conv)
	acc := createAccount(t, c, url, `{"name":"Freedom","type":"brokerage","currency":"USD"}`)
	apple := createInstrument(t, c, url, `{"type":"share","name":"Apple","ticker":"AAPL","isin":"US0378331005","currency":"USD"}`)
	var rates []marketdata.FxRate
	for on, r := range map[string]int64{"2022-03-01": 90, "2025-04-01": 100, "2026-02-10": 80, "2026-05-15": 85, "2026-06-01": 84, "2026-06-30": 83} {
		rates = append(rates, marketdata.FxRate{Base: "USD", Quote: "RUB", On: yearDay(on), Rate: decimal.NewFromInt(r), Source: "test"})
	}
	if err := md.UpsertFxRates(t.Context(), rates); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{
		`{"account_id":%[1]q,"instrument_id":%[2]q,"type":"buy","occurred_on":"2022-03-01","quantity":"10","price":"100","amount_minor":-100000,"currency":"USD"}`,
		`{"account_id":%[1]q,"instrument_id":%[2]q,"type":"buy","occurred_on":"2025-04-01","quantity":"5","price":"120","amount_minor":-60000,"currency":"USD"}`,
		`{"account_id":%[1]q,"instrument_id":%[2]q,"type":"sell","occurred_on":"2026-02-10","quantity":"12","price":"150","amount_minor":180000,"fee_minor":200,"currency":"USD"}`,
		`{"account_id":%[1]q,"instrument_id":%[2]q,"type":"dividend","occurred_on":"2026-05-15","amount_minor":1000,"currency":"USD"}`,
		`{"account_id":%[1]q,"instrument_id":%[2]q,"type":"tax","occurred_on":"2026-05-15","amount_minor":-100,"currency":"USD"}`,
		`{"account_id":%[1]q,"type":"interest","occurred_on":"2026-06-01","amount_minor":200,"currency":"USD"}`,
		`{"account_id":%[1]q,"type":"tax","occurred_on":"2026-06-30","amount_minor":-50,"currency":"USD"}`,
	} {
		createOperation(t, c, url, fmt.Sprintf(op, acc.ID, apple.ID))
	}
	var space uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT id FROM spaces LIMIT 1`).Scan(&space); err != nil {
		t.Fatal(err)
	}
	svc := portfolio.NewService(operation.NewStore(pool), instrument.NewStore(pool), md, conv, family.NewStore(pool))
	got, err := svc.YearRows(t.Context(), space, uuid.MustParse(acc.ID), 2026, "RUB", 3)
	if err != nil {
		t.Fatal(err)
	}

	if len(got.Sales) != 1 {
		t.Fatalf("sales = %+v", got.Sales)
	}
	s := got.Sales[0]
	if !s.Known || !s.Quantity.Equal(decimal.NewFromInt(12)) || s.Proceeds != 180000 || s.Fee != 200 ||
		s.ProceedsInBase != 14_400_000 || s.FeeInBase != 16_000 || len(s.Lots) != 2 {
		t.Errorf("sale = %+v", s)
	}
	// $1 000 of 2022 at 90 and $240 of 2025 at 100: 90 000 + 24 000 against
	// 144 000 − 160 = 143 840; the 2022 lot, held over three years, gains
	// 143 840 × 10/12 − 90 000 = 29 867.
	if l := s.Lots[0]; !l.Known || l.Cost != 100000 || l.CostInBase != 9_000_000 || l.AcquiredOn.Format("2006-01-02") != "2022-03-01" {
		t.Errorf("the 2022 lot = %+v", l)
	}
	if l := s.Lots[1]; l.Cost != 24000 || l.CostInBase != 2_400_000 {
		t.Errorf("the 2025 lot = %+v", l)
	}
	if s.Result != 2_984_000 || s.Long != 2_986_667 {
		t.Errorf("result %d, long %d; want 2 984 000 and 2 986 667", s.Result, s.Long)
	}

	if len(got.Payouts) != 2 {
		t.Fatalf("payouts = %+v", got.Payouts)
	}
	if d := got.Payouts[0]; d.Type != portfolio.TypeDividend || d.Minor != 1000 || d.Withheld != 100 || d.InBase != 85_000 || d.WithheldInBase != 8_500 {
		t.Errorf("dividend = %+v", d)
	}
	if i := got.Payouts[1]; i.Type != portfolio.TypeInterest || i.Instrument != nil || i.InBase != 16_800 {
		t.Errorf("interest = %+v", i)
	}
	if len(got.OtherTaxes) != 1 || got.OtherTaxes[0].Withheld != 50 || got.OtherTaxes[0].WithheldInBase != 4_150 {
		t.Errorf("other taxes = %+v", got.OtherTaxes)
	}
}
