package marketdata_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"babki.my/babki/internal/marketdata"
)

// fakeMetals answers with its prices and remembers each range asked.
type fakeMetals struct {
	prices []marketdata.MetalPrice
	asked  *[][2]time.Time
}

func (f fakeMetals) MetalPrices(_ context.Context, from, to time.Time) ([]marketdata.MetalPrice, error) {
	*f.asked = append(*f.asked, [2]time.Time{from, to})
	return f.prices, nil
}

// The first run takes the whole history; later ones the last week again,
// so a price the bank restates is caught. A day without a price answers
// with the nearest earlier one.
func TestMetalPricesAreKept(t *testing.T) {
	store, _, ctx := newJobsFixture(t)
	if _, err := store.MetalPriceOn(ctx, marketdata.Gold, date("2026-10-12")); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("before any price: %v", err)
	}
	var asked [][2]time.Time
	provider := fakeMetals{asked: &asked, prices: []marketdata.MetalPrice{
		{Metal: marketdata.Gold, On: date("2026-10-09"), Price: dec("11300.5"), Source: "cbr"},
		{Metal: marketdata.Silver, On: date("2026-10-09"), Price: dec("165.2"), Source: "cbr"},
	}}
	today := date("2026-10-12")
	work := func() {
		t.Helper()
		if err := marketdata.NewMetalPricesWorkerAt(store, provider, today).Work(ctx, &river.Job[marketdata.RefreshMetalPricesArgs]{}); err != nil {
			t.Fatal(err)
		}
	}
	work()
	if len(asked) != 1 || asked[0][0].Format(time.DateOnly) != "2000-01-01" || !asked[0][1].Equal(today) {
		t.Fatalf("first range = %v", asked)
	}
	got, err := store.MetalPriceOn(ctx, marketdata.Gold, date("2026-10-11"))
	if err != nil || !got.Price.Equal(dec("11300.5")) || got.On.Format(time.DateOnly) != "2026-10-09" {
		t.Fatalf("gold on a weekend = %+v, %v", got, err)
	}
	work()
	if len(asked) != 2 || asked[1][0].Format(time.DateOnly) != "2026-10-02" {
		t.Fatalf("second range = %v", asked)
	}
}
