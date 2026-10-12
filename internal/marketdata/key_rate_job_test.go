package marketdata_test

import (
	"context"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"babki.my/babki/internal/marketdata"
)

type fakeKeyRates struct {
	rates []marketdata.KeyRate
	asked *[][2]time.Time
}

func (f fakeKeyRates) KeyRates(_ context.Context, from, to time.Time) ([]marketdata.KeyRate, error) {
	*f.asked = append(*f.asked, [2]time.Time{from, to})
	return f.rates, nil
}

// The key rate's history the first time, the last week after; the year's
// highest counts the rate in force on its first day, set the year before.
func TestTheKeyRateIsKept(t *testing.T) {
	store, _, ctx := newJobsFixture(t)
	var asked [][2]time.Time
	provider := fakeKeyRates{asked: &asked, rates: []marketdata.KeyRate{
		{On: date("2024-12-27"), Rate: dec("21"), Source: "cbr"},
		{On: date("2025-06-09"), Rate: dec("20"), Source: "cbr"},
		{On: date("2026-02-16"), Rate: dec("16"), Source: "cbr"},
		{On: date("2026-10-12"), Rate: dec("14"), Source: "cbr"},
	}}
	today := date("2026-10-13")
	for range 2 {
		if err := marketdata.NewKeyRatesWorkerAt(store, provider, today).Work(ctx, &river.Job[marketdata.RefreshKeyRatesArgs]{}); err != nil {
			t.Fatal(err)
		}
	}
	if asked[0][0].Format(time.DateOnly) != "2013-09-13" || asked[1][0].Format(time.DateOnly) != "2026-10-05" {
		t.Fatalf("ranges = %v", asked)
	}
	for _, c := range []struct {
		from, to string
		want     string
	}{
		{"2025-01-01", "2025-12-31", "21"}, // in force on 1 January, set in December
		{"2026-01-01", "2026-12-31", "20"},
		{"2026-03-01", "2026-12-31", "16"},
	} {
		got, found, err := store.HighestKeyRate(ctx, date(c.from), date(c.to))
		if err != nil || !found || !got.Equal(dec(c.want)) {
			t.Errorf("%s..%s = %s, %v, %v; want %s", c.from, c.to, got, found, err, c.want)
		}
	}
	if _, found, _ := store.HighestKeyRate(ctx, date("2010-01-01"), date("2010-12-31")); found {
		t.Error("a year before any rate has one")
	}
}
