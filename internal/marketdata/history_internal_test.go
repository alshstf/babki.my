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

type firstDays map[uuid.UUID]time.Time

func (f firstDays) FirstDaysByInstrument(context.Context) (map[uuid.UUID]time.Time, error) {
	return f, nil
}

type historyCall struct{ market, secid, from, till string }

// stubExchange knows Sberbank and an OFZ by ISIN, and answers AT&T's ISIN with
// the exchange's ruble «T»: a clash the currency check must catch.
type stubExchange struct{ calls *[]historyCall }

func (stubExchange) Locate(_ context.Context, code string) (string, string, string, bool, error) {
	switch code {
	case "RU0009029540":
		return "SBER", "shares", "RUB", true, nil
	case "US00206R1023":
		return "T", "shares", "RUB", true, nil
	case "RU000A1038V6":
		return "SU26238RMFS4", "bonds", "RUB", true, nil
	case "FLAT":
		return "FLAT", "shares", "RUB", true, nil
	}
	return "", "", "", false, nil
}

func (s stubExchange) History(_ context.Context, market, secid string, from, till time.Time) ([]DayPrice, error) {
	*s.calls = append(*s.calls, historyCall{market, secid, from.Format(time.DateOnly), till.Format(time.DateOnly)})
	var out []DayPrice
	for d := from; !d.After(till); d = d.AddDate(0, 0, 1) {
		price := DayPrice{Day: d, Price: decimal.NewFromInt(int64(100 + d.Day())), Currency: "RUB"}
		if market == "bonds" {
			accrued := decimal.NewFromInt(int64(d.Day()))
			price.Bond = &BondDay{Face: decimal.NewFromInt(1000), Accrued: &accrued, Currency: "RUB"}
		}
		out = append(out, price)
	}
	return out, nil
}

func day(s string) time.Time {
	d, _ := time.Parse(time.DateOnly, s)
	return d
}

// Closing prices are fetched from a month before each paper's first operation,
// then only new days; a paper known on the exchange only in another currency
// is not priced from it.
func TestQuoteHistoryIsBackfilledFromTheFirstOperation(t *testing.T) {
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
	sber := mk(instrument.Instrument{Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER", ISIN: "RU0009029540", Currency: "RUB"})
	att := mk(instrument.Instrument{Type: instrument.TypeShare, Name: "AT&T", Ticker: "T", ISIN: "US00206R1023", Currency: "USD"})
	ofz := mk(instrument.Instrument{Type: instrument.TypeBond, Name: "ОФЗ 26238", Ticker: "SU26238RMFS4", ISIN: "RU000A1038V6", Currency: "RUB"})
	// Kinds without exchange history are not asked about.
	custom := mk(instrument.Instrument{Type: instrument.TypeCustom, Name: "Квартира", Ticker: "FLAT", Currency: "RUB"})

	var calls []historyCall
	w := NewBackfillQuotesWorker(store, firstDays{
		sber: day("2026-07-10"), att: day("2026-07-10"), ofz: day("2026-07-15"), custom: day("2026-07-01"),
	}, papers, stubExchange{&calls}, slog.Default()).(*backfillQuotesWorker)
	w.now = func() time.Time { return day("2026-08-01").Add(15 * time.Hour) }
	if err := w.Work(ctx, nil); err != nil {
		t.Fatal(err)
	}
	want := map[historyCall]bool{
		{"shares", "SBER", "2026-06-09", "2026-08-01"}:        true,
		{"bonds", "SU26238RMFS4", "2026-06-14", "2026-08-01"}: true,
	}
	if len(calls) != len(want) {
		t.Fatalf("asked %+v, want %v", calls, want)
	}
	for _, c := range calls {
		if !want[c] {
			t.Errorf("asked %+v, not among %v", c, want)
		}
	}
	got, err := store.QuoteOn(ctx, sber, day("2026-07-20"))
	if err != nil || got.Source != QuoteHistorySource || !got.Price.Equal(decimal.NewFromInt(120)) {
		t.Errorf("SBER on 2026-07-20 = %+v, %v; want 120 from the history", got, err)
	}
	if _, err := store.QuoteOn(ctx, att, day("2026-07-20")); err == nil {
		t.Error("AT&T was priced from the exchange's ruble «T»")
	}
	// A bond's day keeps its face and the interest accrued that day.
	bonds, err := store.BondDaysOn(ctx, []uuid.UUID{ofz, sber}, day("2026-07-20"))
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := bonds[ofz]; !ok || !b.Face.Equal(decimal.NewFromInt(1000)) || b.Accrued == nil || !b.Accrued.Equal(decimal.NewFromInt(20)) || b.Currency != "RUB" {
		t.Errorf("OFZ on 2026-07-20 = %+v; want face 1000 ₽ with 20 ₽ accrued", b)
	}
	if _, ok := bonds[sber]; ok {
		t.Error("a share got a bond day")
	}

	calls = nil
	w.now = func() time.Time { return day("2026-08-03") }
	if err := w.Work(ctx, nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range calls {
		if c.from != "2026-08-02" || c.till != "2026-08-03" {
			t.Errorf("second run asked %+v, want only the days after 2026-08-01", c)
		}
	}
	if len(calls) != 2 {
		t.Errorf("second run asked %d papers, want 2", len(calls))
	}
}
