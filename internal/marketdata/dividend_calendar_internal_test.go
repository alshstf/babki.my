package marketdata

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/platform/testdb"
)

// stubDividends knows Coca-Cola and TSMC, two dividends each.
type stubDividends struct{ asked *[]string }

func (stubDividends) SymbolFor(_ context.Context, isin string) (string, bool, error) {
	switch isin {
	case "US1912161007":
		return "KO", true, nil
	case "US8740391003":
		return "TSM", true, nil
	}
	return "", false, nil
}

func (s stubDividends) Dividends(_ context.Context, symbol string, from, _ time.Time) ([]Dividend, error) {
	*s.asked = append(*s.asked, symbol+" "+from.Format(time.DateOnly))
	return []Dividend{
		{RecordDate: day("2026-03-13"), PerShare: decimal.RequireFromString("0.51"), Currency: "USD"},
		{RecordDate: day("2026-06-13"), PerShare: decimal.RequireFromString("0.51"), Currency: "USD"},
	}, nil
}

func (stubDividends) Name() string { return "stub-feed" }

// The feed's calendar is stored for a foreign paper no broker's calendar
// covers, from a year before its first entry; a Russian paper is not asked
// about; a paper the broker's calendar covers is not asked about either, and
// what the feed said of it before goes.
func TestTheFeedsCalendarCoversWhatNoBrokersDoes(t *testing.T) {
	pool := testdb.New(t)
	ctx := t.Context()
	store, papers := NewStore(pool), instrument.NewStore(pool)
	mk := func(name, isin string) uuid.UUID {
		p, err := papers.Create(ctx, instrument.Instrument{Type: instrument.TypeShare, Name: name, Ticker: name, ISIN: isin, Currency: "USD"})
		if err != nil {
			t.Fatal(err)
		}
		return p.ID
	}
	ko, tsm, sber := mk("KO", "US1912161007"), mk("TSM", "US8740391003"), mk("SBER", "RU0009029540")
	for _, d := range []Dividend{
		{InstrumentID: tsm, Source: "tinvest", RecordDate: day("2026-03-12"), PerShare: decimal.RequireFromString("0.6"), Currency: "USD"},
	} {
		if err := store.ReplaceDividends(ctx, tsm, d.Source, []Dividend{d}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ReplaceDividends(ctx, tsm, "stub-feed", []Dividend{
		{InstrumentID: tsm, Source: "stub-feed", RecordDate: day("2026-03-12"), PerShare: decimal.RequireFromString("0.6"), Currency: "USD"},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}

	var asked []string
	w := NewDividendCalendarWorker(store, firstDays{ko: day("2025-06-10"), tsm: day("2025-06-10"), sber: day("2025-06-10")},
		papers, stubDividends{&asked}, slog.Default()).(*dividendCalendarWorker)
	if err := w.Work(ctx, nil); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(asked, []string{"KO 2024-06-10"}) {
		t.Errorf("asked %v, want only Coca-Cola, from a year before its first entry", asked)
	}
	got, err := store.DividendsOf(ctx, []uuid.UUID{ko, tsm, sber})
	if err != nil {
		t.Fatal(err)
	}
	if len(got[ko]) != 2 || got[ko][0].Source != "stub-feed" || got[ko][0].InstrumentID != ko {
		t.Errorf("Coca-Cola's calendar = %+v, want the feed's two dividends", got[ko])
	}
	if len(got[tsm]) != 1 || got[tsm][0].Source != "tinvest" {
		t.Errorf("TSMC's calendar = %+v, want the broker's alone", got[tsm])
	}
	if len(got[sber]) != 0 {
		t.Errorf("Sberbank's calendar = %+v, want none", got[sber])
	}
}
