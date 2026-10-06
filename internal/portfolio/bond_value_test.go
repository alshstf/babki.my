package portfolio_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/testdb"
)

// A bond half repaid since it was catalogued at 1 000 ₽ is quoted at 94 % of
// the 500 ₽ the exchange now states, with 49,29 ₽ of coupon accrued per bond.
//
//	10 × (500 × 94 % + 49,29) = 5 192,90 ₽, not 10 × 1 000 × 94 % = 9 400 ₽
func TestABondIsWorthItsPriceOnTheCurrentFacePlusTheAccruedInterest(t *testing.T) {
	statedAccrued := int64(4_929)
	for _, tc := range []struct {
		name string
		// statedDaysAgo is when the exchange last stated the face and the interest.
		statedDaysAgo int
		worth         int64
		accrued       *int64
	}{
		{"stated today", 0, 519_290, &statedAccrued},
		// Interest a month old is no figure: the price on the current face alone.
		{"stated a month ago", 30, 470_000, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testdb.New(t)
			md := marketdata.NewStore(pool)
			url, c := setupAPI(t, pool, md, marketdata.NewConverter(md))
			today := time.Now().UTC().Truncate(24 * time.Hour)

			acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
			bond := createInstrument(t, c, url,
				`{"type":"bond","name":"Амортизируемая","ticker":"AMRT","currency":"RUB","face_value_minor":100000,"face_currency":"RUB"}`)
			createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
				"occurred_on":"2026-01-15","quantity":"10","price":"95","amount_minor":-500000,"currency":"RUB"}`, acc.ID, bond.ID))
			id := uuid.MustParse(bond.ID)
			if err := md.UpsertQuotes(t.Context(), []marketdata.Quote{
				{InstrumentID: id, On: today, Price: decimal.NewFromInt(94), Currency: "RUB", Source: "moex"},
			}); err != nil {
				t.Fatal(err)
			}
			accrued := decimal.RequireFromString("49.29")
			if err := md.UpsertBondDays(t.Context(), []marketdata.BondDay{
				{InstrumentID: id, On: today.AddDate(0, 0, -tc.statedDaysAgo), Face: decimal.NewFromInt(500), Accrued: &accrued, Currency: "RUB", Source: "moex"},
			}); err != nil {
				t.Fatal(err)
			}

			p := accountPositions(t, c, url, acc.ID).Positions[0]
			if p.MarketValueMinor == nil || *p.MarketValueMinor != tc.worth {
				t.Errorf("worth = %v, want %d", p.MarketValueMinor, tc.worth)
			}
			if p.PriceMoneyMinor == nil || *p.PriceMoneyMinor != 47_000 {
				t.Errorf("price in money = %v, want 47000: 94 %% of the current 500 ₽", p.PriceMoneyMinor)
			}
			if (p.AccruedInterestMinor == nil) != (tc.accrued == nil) ||
				(tc.accrued != nil && *p.AccruedInterestMinor != *tc.accrued) {
				t.Errorf("accrued interest = %v, want %v", p.AccruedInterestMinor, tc.accrued)
			}
		})
	}
}
