package marketdata

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/platform/testdb"
)

// stubNAV values FXIT (in dollars), one day per asked day.
type stubNAV struct{ asked *[]string }

func (stubNAV) Funds(context.Context) (map[string]string, error) {
	return map[string]string{"IE00BD3QJ757": "FXIT"}, nil
}

func (s stubNAV) NAVHistory(_ context.Context, ticker string, from time.Time) ([]DayPrice, error) {
	*s.asked = append(*s.asked, ticker+" "+from.Format(time.DateOnly))
	var out []DayPrice
	for d := from; !d.After(day("2026-08-03")); d = d.AddDate(0, 0, 1) {
		out = append(out, DayPrice{Day: d, Price: decimal.NewFromInt(330), Currency: "USD"})
	}
	return out, nil
}

func (stubNAV) Name() string { return "stub-nav" }

// stubForeign knows Microsoft and fails for Walt Disney.
type stubForeign struct{ asked *[]string }

func (stubForeign) SymbolFor(_ context.Context, isin string) (string, bool, error) {
	switch isin {
	case "US5949181045":
		return "MSFT", true, nil
	case "US2546871060":
		return "DIS", true, nil
	}
	return "", false, nil
}

func (s stubForeign) Closes(_ context.Context, symbol string, from, to time.Time) ([]DayPrice, error) {
	*s.asked = append(*s.asked, symbol+" "+from.Format(time.DateOnly)+" "+to.Format(time.DateOnly))
	if symbol == "DIS" {
		return nil, errors.New("status 429")
	}
	return []DayPrice{{Day: to, Price: decimal.NewFromInt(525), Currency: "USD"}}, nil
}

func (stubForeign) Name() string { return "stub-foreign" }

// A fund is valued from its NAV and a foreign share from its home exchange,
// from a month before the first operation and then only new days; a Russian
// share is not asked about, and one share's failure does not stop the rest.
func TestReferencePricesAreFetchedByKind(t *testing.T) {
	pool := testdb.New(t)
	ctx := t.Context()
	store, papers := NewStore(pool), instrument.NewStore(pool)
	mk := func(inst instrument.Instrument) uuid.UUID {
		created, err := papers.Create(ctx, inst)
		if err != nil {
			t.Fatalf("create %s: %v", inst.Name, err)
		}
		return created.ID
	}
	fxit := mk(instrument.Instrument{Type: instrument.TypeETF, Name: "FinEx ИТ", Ticker: "IE00BD3QJ757", ISIN: "IE00BD3QJ757", Currency: "RUB"})
	msft := mk(instrument.Instrument{Type: instrument.TypeShare, Name: "Microsoft", Ticker: "MSFT-RM", ISIN: "US5949181045", Currency: "RUB"})
	dis := mk(instrument.Instrument{Type: instrument.TypeShare, Name: "Disney", Ticker: "DIS", ISIN: "US2546871060", Currency: "USD"})
	sber := mk(instrument.Instrument{Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER", ISIN: "RU0009029540", Currency: "RUB"})

	var navAsked, foreignAsked []string
	w := NewReferencePricesWorker(store, firstDays{
		fxit: day("2026-07-10"), msft: day("2026-07-20"), dis: day("2026-07-20"), sber: day("2026-07-01"),
	}, papers, stubNAV{&navAsked}, stubForeign{&foreignAsked}, slog.Default()).(*referencePricesWorker)
	w.now = func() time.Time { return day("2026-08-03").Add(15 * time.Hour) }

	if err := w.Work(ctx, nil); err == nil {
		t.Error("Disney's failure was swallowed: the job must fail so it is retried")
	}
	if len(navAsked) != 1 || navAsked[0] != "FXIT 2026-06-09" {
		t.Errorf("NAV asked %v, want FXIT from 2026-06-09", navAsked)
	}
	if len(foreignAsked) != 2 {
		t.Errorf("home exchange asked %v, want Microsoft and Disney, not Sberbank", foreignAsked)
	}
	prices, err := store.ReferencePricesOn(ctx, []uuid.UUID{fxit, msft, sber}, day("2026-08-03"))
	if err != nil {
		t.Fatal(err)
	}
	nav := prices[fxit][ReferenceNAV]
	if !nav.Price.Equal(decimal.NewFromInt(330)) || nav.Currency != "USD" || nav.Source != "stub-nav" {
		t.Errorf("FXIT = %+v, want 330 USD from the NAV source", nav)
	}
	if got := prices[msft][ReferenceForeign]; !got.Price.Equal(decimal.NewFromInt(525)) || !got.On.Equal(day("2026-08-03")) {
		t.Errorf("Microsoft = %+v, want 525 on 2026-08-03", got)
	}
	if _, ok := prices[sber]; ok {
		t.Error("a Russian share has a reference price")
	}

	navAsked, foreignAsked = nil, nil
	w.now = func() time.Time { return day("2026-08-05") }
	_ = w.Work(ctx, nil)
	if len(navAsked) != 1 || navAsked[0] != "FXIT 2026-08-04" {
		t.Errorf("second run asked NAV %v, want only the days after 2026-08-03", navAsked)
	}
}

// The price on a day is the latest on or before it, per kind.
func TestReferencePricesOnTakeTheLatestEarlierDay(t *testing.T) {
	pool := testdb.New(t)
	ctx := t.Context()
	store, papers := NewStore(pool), instrument.NewStore(pool)
	created, err := papers.Create(ctx, instrument.Instrument{Type: instrument.TypeETF, Name: "FinEx ИТ", Ticker: "FXIT", ISIN: "IE00BD3QJ757", Currency: "RUB"})
	if err != nil {
		t.Fatal(err)
	}
	id := created.ID
	if err := store.UpsertReferencePrices(ctx, []ReferencePrice{
		{InstrumentID: id, Kind: ReferenceNAV, On: day("2026-08-01"), Price: decimal.NewFromInt(320), Currency: "USD", Source: "finex"},
		{InstrumentID: id, Kind: ReferenceNAV, On: day("2026-08-28"), Price: decimal.NewFromInt(330), Currency: "USD", Source: "finex"},
	}); err != nil {
		t.Fatal(err)
	}
	for on, want := range map[string]int64{"2026-08-27": 320, "2026-10-06": 330} {
		got, err := store.ReferencePricesOn(ctx, []uuid.UUID{id}, day(on))
		if err != nil || !got[id][ReferenceNAV].Price.Equal(decimal.NewFromInt(want)) {
			t.Errorf("on %s: %+v, %v; want %d", on, got[id], err, want)
		}
	}
	if got, _ := store.ReferencePricesOn(ctx, []uuid.UUID{id}, day("2026-07-31")); len(got) != 0 {
		t.Errorf("before the first day: %+v, want nothing", got)
	}
	series, err := store.ReferenceSeries(ctx, id, ReferenceNAV, day("2026-08-01"), day("2026-08-31"))
	if err != nil || len(series) != 2 {
		t.Errorf("series = %+v, %v; want both days", series, err)
	}
}
