package marketdata

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/platform/testdb"
)

// stubCoins knows bitcoin by «BTC»; it records what it was asked.
type stubCoins struct{ asked *[]string }

func (s stubCoins) CoinFor(_ context.Context, ticker string) (string, bool, error) {
	*s.asked = append(*s.asked, "coin "+ticker)
	return "bitcoin", ticker == "BTC", nil
}

func (s stubCoins) History(_ context.Context, coin, currency string, from, till time.Time) ([]DayPrice, error) {
	*s.asked = append(*s.asked, "history "+coin+" "+currency+" "+from.Format(time.DateOnly))
	return []DayPrice{{Day: till.AddDate(0, 0, -1), Price: decimal.NewFromInt(85770), Currency: currency}}, nil
}

func (s stubCoins) Current(_ context.Context, coins []string, currency string) (map[string]DayPrice, error) {
	out := map[string]DayPrice{}
	for _, coin := range coins {
		out[coin] = DayPrice{Day: day("2026-10-06"), Price: decimal.NewFromInt(85587), Currency: currency}
	}
	return out, nil
}

func (stubCoins) HistoryWindow() time.Duration { return 364 * 24 * time.Hour }
func (stubCoins) Name() string                 { return "stub-coins" }

// A cryptocurrency is priced from the coin feed: its coin picked by ticker
// once and kept, its last year's days and today's price stored. A share is
// not asked about; history is asked only as far back as the feed answers.
func TestCryptocurrenciesArePricedFromTheCoinFeed(t *testing.T) {
	pool := testdb.New(t)
	ctx := t.Context()
	store, papers := NewStore(pool), instrument.NewStore(pool)
	mk := func(inst instrument.Instrument) uuid.UUID {
		created, err := papers.Create(ctx, inst)
		if err != nil {
			t.Fatal(err)
		}
		return created.ID
	}
	btc := mk(instrument.Instrument{Type: instrument.TypeCrypto, Name: "Bitcoin", Ticker: "BTC", Currency: "USD"})
	sber := mk(instrument.Instrument{Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER", ISIN: "RU0009029540", Currency: "RUB"})

	var asked []string
	w := NewCryptoPricesWorker(store, firstDays{btc: day("2021-03-01"), sber: day("2024-01-10")}, papers,
		stubCoins{&asked}, slog.Default()).(*cryptoPricesWorker)
	w.now = func() time.Time { return day("2026-10-07").Add(3 * time.Hour) }
	if err := w.Work(ctx, nil); err != nil {
		t.Fatal(err)
	}

	if len(asked) != 2 || asked[0] != "coin BTC" || asked[1] != "history bitcoin USD 2025-10-08" {
		t.Errorf("asked %v, want the coin for BTC and its history from a year back", asked)
	}
	got, err := papers.ByIDs(ctx, []uuid.UUID{btc})
	if err != nil {
		t.Fatal(err)
	}
	if got[btc].CoinGeckoID != "bitcoin" {
		t.Errorf("coin kept = %q, want bitcoin", got[btc].CoinGeckoID)
	}
	quotes, err := store.QuotesOn(ctx, []uuid.UUID{btc, sber}, day("2026-10-06"))
	if err != nil {
		t.Fatal(err)
	}
	if q := quotes[btc]; !q.Price.Equal(decimal.NewFromInt(85587)) || q.Source != "stub-coins" || q.Currency != "USD" {
		t.Errorf("bitcoin on 2026-10-06 = %+v, want today's 85587 USD from the feed", q)
	}
	if _, ok := quotes[sber]; ok {
		t.Error("a share was priced by the coin feed")
	}

	// The coin, once picked, is not looked up again.
	asked = nil
	if err := w.Work(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0] != "history bitcoin USD 2025-10-08" {
		t.Errorf("second run asked %v, want only the history", asked)
	}
}
