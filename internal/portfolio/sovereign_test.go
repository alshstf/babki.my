package portfolio

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
)

// dollarRates is the official dollar in roubles by day; a day it lacks has no
// rate.
type dollarRates map[string]string

func (r dollarRates) Rate(_ context.Context, from, to string, on time.Time) (decimal.Decimal, time.Time, error) {
	if from == to {
		return decimal.NewFromInt(1), on, nil
	}
	if rate, ok := r[on.Format(time.DateOnly)]; ok && from == "USD" && to == "RUB" {
		return decimal.RequireFromString(rate), on, nil
	}
	return decimal.Decimal{}, time.Time{}, marketdata.ErrNoRate
}

func (dollarRates) RatesOn(context.Context, []marketdata.RateQuery) (marketdata.Rates, error) {
	return marketdata.Rates{}, nil
}

// rus28 folds a journal of one RUS-28 bond held in currency: bought on
// 2021-03-10 for 105 % of its 1 000 $ face, and, when sold, sold on 2025-05-15
// for 110 %.
func rus28(t *testing.T, currency string, bought, sold int64) *Position {
	t.Helper()
	paper := uuid.New()
	one := decimal.NewFromInt(1)
	ops := []Operation{{
		ID: uuid.New(), Type: TypeBuy, InstrumentID: &paper, OccurredOn: day(t, "2021-03-10"),
		Quantity: &one, AmountMinor: -bought, Currency: currency,
	}}
	if sold != 0 {
		ops = append(ops, Operation{
			ID: uuid.New(), Type: TypeSell, InstrumentID: &paper, OccurredOn: day(t, "2025-05-15"),
			Quantity: &one, AmountMinor: sold, Currency: currency,
		})
	}
	positions, err := Compute(ops)
	if err != nil {
		t.Fatal(err)
	}
	return positions[paper]
}

var today = time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)

// The memo's example: bought for 77 700 ₽ at 74 ₽ a dollar, sold for 110 000 ₽
// at 100. The tax counts the 1 050 $ paid at the sale's 100: a cost of
// 105 000 ₽ and a result of 5 000 ₽, not the 32 300 ₽ the roubles alone show.
func TestABondOfRussiasExternalLoansCountsItsRoubleCostAtTheSalesRate(t *testing.T) {
	s := &Service{}
	rates := marketdata.NewRateMemo(dollarRates{"2021-03-10": "74", "2025-05-15": "100", "2026-10-07": "90"})

	sold := rus28(t, "RUB", 7_770_000, 11_000_000)
	ok, err := s.restateAtSaleRate(t.Context(), sold, "USD", "RUB", today, rates)
	if err != nil || !ok {
		t.Fatalf("restated %v, %v", ok, err)
	}
	if got, _ := sold.RealizedPnL(); got != 500_000 {
		t.Errorf("realized %d, want 500000 — 110 000 ₽ less 1 050 $ at 100", got)
	}

	// What is still held is counted at today's rate, so the result does not jump
	// on the day it is sold: 1 050 $ at 90.
	held := rus28(t, "RUB", 7_770_000, 0)
	if ok, err := s.restateAtSaleRate(t.Context(), held, "USD", "RUB", today, rates); err != nil || !ok {
		t.Fatalf("restated %v, %v", ok, err)
	}
	if held.CostMinor != 9_450_000 || held.Lots[0].CostMinor != 9_450_000 {
		t.Errorf("held cost %d (lot %d), want 9450000", held.CostMinor, held.Lots[0].CostMinor)
	}
}

// Held in dollars with roubles as the base, the dollar figures stand and the
// rouble ones are dated by the sale: the cost's rate day becomes the sale's,
// today's for what is held.
func TestABondHeldInItsFaceCurrencyDatesItsRoubleCostBySale(t *testing.T) {
	s := &Service{}
	rates := marketdata.NewRateMemo(dollarRates{})

	sold := rus28(t, "USD", 105_000, 110_000)
	if ok, err := s.restateAtSaleRate(t.Context(), sold, "USD", "RUB", today, rates); err != nil || !ok {
		t.Fatalf("restated %v, %v", ok, err)
	}
	if got, _ := sold.RealizedPnL(); got != 5_000 {
		t.Errorf("realized %d $ minor, want the dollars' own 5000", got)
	}
	if pc := sold.Realizations[0].Released[0]; pc.RateOn == nil || !pc.RateOn.Equal(day(t, "2025-05-15")) {
		t.Errorf("the sold parcel is priced on %v, want the sale's 2025-05-15", pc.RateOn)
	}

	held := rus28(t, "USD", 105_000, 0)
	if ok, err := s.restateAtSaleRate(t.Context(), held, "USD", "RUB", today, rates); err != nil || !ok {
		t.Fatalf("restated %v, %v", ok, err)
	}
	if l := held.Lots[0]; l.RateOn == nil || !l.RateOn.Equal(today) || l.CostMinor != 105_000 {
		t.Errorf("held lot %+v, want 105000 $ minor priced today", l)
	}
}

// Nothing is mixed: a missing rate leaves the whole position as it was, and a
// face in roubles or a base other than roubles is not the rule's.
func TestTheSaleRateRuleLeavesAloneWhatItCannotOrNeedNotRestate(t *testing.T) {
	s := &Service{}
	for name, c := range map[string]struct {
		currency, face, base string
		rates                dollarRates
	}{
		"no rate on the day of purchase": {"RUB", "USD", "RUB", dollarRates{"2025-05-15": "100"}},
		"a rouble face":                  {"RUB", "RUB", "RUB", dollarRates{"2021-03-10": "74", "2025-05-15": "100"}},
		"dollars with a dollar base":     {"USD", "USD", "USD", dollarRates{}},
		"held in a third currency":       {"EUR", "USD", "RUB", dollarRates{}},
	} {
		t.Run(name, func(t *testing.T) {
			p := rus28(t, c.currency, 7_770_000, 11_000_000)
			ok, err := s.restateAtSaleRate(t.Context(), p, c.face, c.base, today, marketdata.NewRateMemo(c.rates))
			if err != nil || ok {
				t.Fatalf("restated %v, %v; want left alone", ok, err)
			}
			if got, _ := p.RealizedPnL(); got != 3_230_000 || p.Realizations[0].Released[0].RateOn != nil {
				t.Errorf("realized %d, rate day %v; want the untouched 3230000", got, p.Realizations[0].Released[0].RateOn)
			}
		})
	}
}
