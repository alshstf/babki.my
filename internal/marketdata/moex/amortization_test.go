package moex

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// ОФЗ 46018 as the exchange lists it: 1 000 ₽ repaid 300, 300 and 400 ₽.
// Before the second repayment 700 ₽ are outstanding, before the third 400 ₽;
// a payment a few days after its schedule day still finds it.
func TestFaceBeforeIsTheInitialFaceLessEarlierRepayments(t *testing.T) {
	day := func(s string) time.Time { d, _ := time.Parse(time.DateOnly, s); return d }
	dec := decimal.RequireFromString
	schedule := []Amortization{
		{On: day("2019-11-27"), Value: dec("300"), InitialFace: dec("1000"), Currency: "RUB"},
		{On: day("2020-11-25"), Value: dec("300"), InitialFace: dec("1000"), Currency: "RUB"},
		{On: day("2021-11-24"), Value: dec("400"), InitialFace: dec("1000"), Currency: "RUB"},
	}
	week := 7 * 24 * time.Hour
	for on, want := range map[string]string{"2019-11-27": "1000", "2020-11-27": "700", "2021-11-24": "400"} {
		face, currency, ok := FaceBefore(schedule, day(on), week)
		if !ok || !face.Equal(dec(want)) || currency != "RUB" {
			t.Errorf("before the repayment of %s: %s %s (found %v), want %s RUB", on, face, currency, ok, want)
		}
	}
	if _, _, ok := FaceBefore(schedule, day("2020-06-01"), week); ok {
		t.Error("a day with no repayment near it found one")
	}
	if got := faceUnit("RUR"); got != "RUB" {
		t.Errorf("RUR reads as %s, want RUB", got)
	}
}
