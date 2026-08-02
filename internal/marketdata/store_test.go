package marketdata_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/testdb"
)

type fixture struct {
	store *marketdata.Store
	ctx   context.Context
	insts []uuid.UUID // 4 instruments: 3 get quotes, 1 stays without any
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()
	fam := family.NewStore(pool)
	u, err := fam.CreateUser(ctx, "alex", "A", "h")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	if _, err := fam.CreateSpaceWithOwner(ctx, "S", u.ID); err != nil {
		t.Fatalf("space: %v", err)
	}

	instStore := instrument.NewStore(pool)
	var insts []uuid.UUID
	for i, name := range []string{"Сбербанк", "Газпром", "Лукойл", "Яндекс"} {
		inst, err := instStore.Create(ctx, instrument.Instrument{
			Type: instrument.TypeShare, Name: name, Currency: "RUB",
		})
		if err != nil {
			t.Fatalf("instrument %d: %v", i, err)
		}
		insts = append(insts, inst.ID)
	}

	return fixture{store: marketdata.NewStore(pool), ctx: ctx, insts: insts}
}

func date(s string) time.Time {
	d, _ := time.Parse("2006-01-02", s)
	return d
}

func dec(s string) decimal.Decimal {
	return decimal.RequireFromString(s)
}

func TestUpsertFxRatesAndFxRateOn(t *testing.T) {
	f := newFixture(t)

	// initial batch
	err := f.store.UpsertFxRates(f.ctx, []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: date("2026-07-01"), Rate: dec("90.5"), Source: "cbr"},
		{Base: "USD", Quote: "RUB", On: date("2026-07-03"), Rate: dec("91.2"), Source: "cbr"},
		{Base: "EUR", Quote: "RUB", On: date("2026-07-03"), Rate: dec("98.0"), Source: "cbr"},
	})
	if err != nil {
		t.Fatalf("UpsertFxRates: %v", err)
	}

	// re-upsert same (base, quote, on_date) updates rather than duplicates.
	err = f.store.UpsertFxRates(f.ctx, []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: date("2026-07-01"), Rate: dec("90.9"), Source: "cbr"},
	})
	if err != nil {
		t.Fatalf("UpsertFxRates (update): %v", err)
	}

	// exact date match
	got, err := f.store.FxRateOn(f.ctx, "USD", "RUB", date("2026-07-01"))
	if err != nil {
		t.Fatalf("FxRateOn exact: %v", err)
	}
	if !got.Rate.Equal(dec("90.9")) {
		t.Fatalf("FxRateOn exact rate = %s, want 90.9 (update should replace, not duplicate)", got.Rate)
	}

	// no rate on this exact date -> nearest earlier day (2026-07-01, not 2026-07-03)
	got, err = f.store.FxRateOn(f.ctx, "USD", "RUB", date("2026-07-02"))
	if err != nil {
		t.Fatalf("FxRateOn nearest previous: %v", err)
	}
	if !got.On.Equal(date("2026-07-01")) || !got.Rate.Equal(dec("90.9")) {
		t.Fatalf("FxRateOn nearest previous = %+v, want on=2026-07-01 rate=90.9", got)
	}

	// earlier than all data -> pgx.ErrNoRows
	_, err = f.store.FxRateOn(f.ctx, "USD", "RUB", date("2026-06-01"))
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("FxRateOn before any data: err = %v, want pgx.ErrNoRows", err)
	}

	// unknown pair entirely -> pgx.ErrNoRows
	_, err = f.store.FxRateOn(f.ctx, "GBP", "RUB", date("2026-07-03"))
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("FxRateOn unknown pair: err = %v, want pgx.ErrNoRows", err)
	}
}

func TestLatestFxRates(t *testing.T) {
	f := newFixture(t)

	err := f.store.UpsertFxRates(f.ctx, []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: date("2026-07-01"), Rate: dec("90.5"), Source: "cbr"},
		{Base: "USD", Quote: "RUB", On: date("2026-07-03"), Rate: dec("91.2"), Source: "cbr"},
		{Base: "EUR", Quote: "RUB", On: date("2026-07-02"), Rate: dec("98.0"), Source: "cbr"},
	})
	if err != nil {
		t.Fatalf("UpsertFxRates: %v", err)
	}

	latest, err := f.store.LatestFxRates(f.ctx)
	if err != nil {
		t.Fatalf("LatestFxRates: %v", err)
	}
	if len(latest) != 2 {
		t.Fatalf("LatestFxRates len = %d, want 2: %+v", len(latest), latest)
	}
	byPair := map[string]marketdata.FxRate{}
	for _, r := range latest {
		byPair[r.Base+"/"+r.Quote] = r
	}
	if r, ok := byPair["USD/RUB"]; !ok || !r.On.Equal(date("2026-07-03")) || !r.Rate.Equal(dec("91.2")) {
		t.Fatalf("USD/RUB latest = %+v", r)
	}
	if r, ok := byPair["EUR/RUB"]; !ok || !r.On.Equal(date("2026-07-02")) {
		t.Fatalf("EUR/RUB latest = %+v", r)
	}
}

func TestUpsertQuotesAndQuoteOn(t *testing.T) {
	f := newFixture(t)
	sber := f.insts[0]

	err := f.store.UpsertQuotes(f.ctx, []marketdata.Quote{
		{InstrumentID: sber, On: date("2026-07-01"), Price: dec("305.5"), Currency: "RUB", Source: "moex"},
		{InstrumentID: sber, On: date("2026-07-03"), Price: dec("310.0"), Currency: "RUB", Source: "moex"},
	})
	if err != nil {
		t.Fatalf("UpsertQuotes: %v", err)
	}

	// re-upsert same (instrument, on_date) updates rather than duplicates.
	err = f.store.UpsertQuotes(f.ctx, []marketdata.Quote{
		{InstrumentID: sber, On: date("2026-07-01"), Price: dec("306.0"), Currency: "RUB", Source: "moex"},
	})
	if err != nil {
		t.Fatalf("UpsertQuotes (update): %v", err)
	}

	got, err := f.store.QuoteOn(f.ctx, sber, date("2026-07-01"))
	if err != nil {
		t.Fatalf("QuoteOn exact: %v", err)
	}
	if !got.Price.Equal(dec("306.0")) {
		t.Fatalf("QuoteOn exact price = %s, want 306.0 (update should replace, not duplicate)", got.Price)
	}

	// no quote on this exact date -> nearest earlier day
	got, err = f.store.QuoteOn(f.ctx, sber, date("2026-07-02"))
	if err != nil {
		t.Fatalf("QuoteOn nearest previous: %v", err)
	}
	if !got.On.Equal(date("2026-07-01")) || !got.Price.Equal(dec("306.0")) {
		t.Fatalf("QuoteOn nearest previous = %+v, want on=2026-07-01 price=306.0", got)
	}

	// earlier than all data -> pgx.ErrNoRows
	_, err = f.store.QuoteOn(f.ctx, sber, date("2026-06-01"))
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("QuoteOn before any data: err = %v, want pgx.ErrNoRows", err)
	}

	// instrument with no quotes at all -> pgx.ErrNoRows
	_, err = f.store.QuoteOn(f.ctx, f.insts[3], date("2026-07-03"))
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("QuoteOn no data instrument: err = %v, want pgx.ErrNoRows", err)
	}
}

func TestLatestQuotesBatch(t *testing.T) {
	f := newFixture(t)
	sber, gazp, lkoh, yndx := f.insts[0], f.insts[1], f.insts[2], f.insts[3]

	err := f.store.UpsertQuotes(f.ctx, []marketdata.Quote{
		{InstrumentID: sber, On: date("2026-07-01"), Price: dec("305.5"), Currency: "RUB", Source: "moex"},
		{InstrumentID: sber, On: date("2026-07-03"), Price: dec("310.0"), Currency: "RUB", Source: "moex"},
		{InstrumentID: gazp, On: date("2026-07-02"), Price: dec("150.0"), Currency: "RUB", Source: "moex"},
		{InstrumentID: lkoh, On: date("2026-07-03"), Price: dec("7000.0"), Currency: "RUB", Source: "moex"},
		// yndx gets no quotes at all.
	})
	if err != nil {
		t.Fatalf("UpsertQuotes: %v", err)
	}

	// batch lookup over all 4 instruments, including the one with no data.
	latest, err := f.store.LatestQuotes(f.ctx, []uuid.UUID{sber, gazp, lkoh, yndx})
	if err != nil {
		t.Fatalf("LatestQuotes: %v", err)
	}
	if len(latest) != 3 {
		t.Fatalf("LatestQuotes len = %d, want 3 (no entry for instrument without quotes): %+v", len(latest), latest)
	}
	if _, ok := latest[yndx]; ok {
		t.Fatalf("LatestQuotes has spurious entry for instrument without quotes: %+v", latest[yndx])
	}
	if q, ok := latest[sber]; !ok || !q.On.Equal(date("2026-07-03")) || !q.Price.Equal(dec("310.0")) {
		t.Fatalf("LatestQuotes[sber] = %+v, want on=2026-07-03 price=310.0", q)
	}
	if q, ok := latest[gazp]; !ok || !q.Price.Equal(dec("150.0")) {
		t.Fatalf("LatestQuotes[gazp] = %+v", q)
	}
	if q, ok := latest[lkoh]; !ok || !q.Price.Equal(dec("7000.0")) {
		t.Fatalf("LatestQuotes[lkoh] = %+v", q)
	}

	// empty id list -> empty map, no error.
	empty, err := f.store.LatestQuotes(f.ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("LatestQuotes(nil) = %+v, %v", empty, err)
	}
}
