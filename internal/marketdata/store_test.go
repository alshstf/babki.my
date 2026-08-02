package marketdata_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/testdb"
)

type fixture struct {
	store *marketdata.Store
	pool  *pgxpool.Pool // exposed so tests can inspect Stat() (e.g. zero-round-trip assertions)
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

	return fixture{store: marketdata.NewStore(pool), pool: pool, ctx: ctx, insts: insts}
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

func TestFxRatesOnBatch(t *testing.T) {
	f := newFixture(t)

	err := f.store.UpsertFxRates(f.ctx, []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: date("2026-07-01"), Rate: dec("90.5"), Source: "cbr"},
		{Base: "USD", Quote: "RUB", On: date("2026-07-03"), Rate: dec("91.2"), Source: "cbr"},
		{Base: "EUR", Quote: "RUB", On: date("2026-07-03"), Rate: dec("98.0"), Source: "cbr"},
	})
	if err != nil {
		t.Fatalf("UpsertFxRates: %v", err)
	}

	keys := []marketdata.FxRateKey{
		{Base: "USD", Quote: "RUB", On: date("2026-07-01")}, // exact match
		{Base: "USD", Quote: "RUB", On: date("2026-07-01")}, // duplicate of the above -> collapses
		{Base: "USD", Quote: "RUB", On: date("2026-07-02")}, // no row on this day -> nearest earlier (07-01)
		{Base: "USD", Quote: "RUB", On: date("2026-07-05")}, // later than all rows -> nearest earlier (07-03)
		{Base: "EUR", Quote: "RUB", On: date("2026-07-03")}, // exact match
		{Base: "GBP", Quote: "RUB", On: date("2026-07-03")}, // unknown pair -> absent
		{Base: "USD", Quote: "RUB", On: date("2026-06-01")}, // earlier than all data -> absent
	}

	// Discriminating check: batching N keys must take exactly one round trip
	// to the database, not N — that is the entire reason FxRatesOn exists
	// instead of a loop of FxRateOn calls. AcquireCount is a lifetime
	// counter on the pool (same technique the empty-input check below uses),
	// so comparing before and after catches an implementation that compiles
	// and returns correct rates while quietly issuing one query per key.
	beforeBatch := f.pool.Stat().AcquireCount()
	got, err := f.store.FxRatesOn(f.ctx, keys)
	if err != nil {
		t.Fatalf("FxRatesOn: %v", err)
	}
	if afterBatch := f.pool.Stat().AcquireCount(); afterBatch-beforeBatch != 1 {
		t.Fatalf("FxRatesOn(%d keys) acquired %d connections, want exactly 1", len(keys), afterBatch-beforeBatch)
	}

	// Discriminating check: every key's outcome must agree with what FxRateOn
	// returns for that same (base, quote, on) individually — same resolved
	// date, same rate, same presence/absence. This is what catches a lateral
	// join missing its ORDER BY (wrong row picked when more than one
	// candidate qualifies) or a result that reports the requested date
	// instead of the row's own.
	for _, k := range keys {
		want, wantErr := f.store.FxRateOn(f.ctx, k.Base, k.Quote, k.On)
		gotRate, ok := got[k]
		if wantErr != nil {
			if !errors.Is(wantErr, pgx.ErrNoRows) {
				t.Fatalf("FxRateOn(%+v) unexpected error: %v", k, wantErr)
			}
			if ok {
				t.Fatalf("FxRatesOn[%+v] = %+v, want absent (FxRateOn found nothing)", k, gotRate)
			}
			continue
		}
		if !ok {
			t.Fatalf("FxRatesOn[%+v] absent, want %+v (FxRateOn found it)", k, want)
		}
		if gotRate.Base != want.Base || gotRate.Quote != want.Quote ||
			!gotRate.On.Equal(want.On) || !gotRate.Rate.Equal(want.Rate) || gotRate.Source != want.Source {
			t.Fatalf("FxRatesOn[%+v] = %+v, want %+v (from FxRateOn)", k, gotRate, want)
		}
	}

	// 7 keys in, 2 collapse (exact duplicate) and 2 are absent (GBP/RUB has
	// no rows at all; 2026-06-01 is earlier than every USD/RUB row) -> 4
	// distinct present keys.
	if len(got) != 4 {
		t.Fatalf("FxRatesOn len = %d, want 4: %+v", len(got), got)
	}

	// Empty input -> empty map and not a single round trip to the database.
	// AcquireCount is a lifetime counter on the pool, so comparing before and
	// after catches an implementation that forgot the short-circuit and
	// queried with empty arrays instead.
	before := f.pool.Stat().AcquireCount()
	empty, err := f.store.FxRatesOn(f.ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("FxRatesOn(nil) = %+v, %v", empty, err)
	}
	if after := f.pool.Stat().AcquireCount(); after != before {
		t.Fatalf("FxRatesOn(nil) acquired a connection (before=%d after=%d), want zero round trips for empty input", before, after)
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
