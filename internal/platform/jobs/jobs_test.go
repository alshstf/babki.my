package jobs_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/jobs"
	"babki.my/babki/internal/platform/testdb"
)

// stubFxProvider and stubQuoteProvider are network-free marketdata provider
// stand-ins. NewWorkers registers the fx/quotes/backfill periodic jobs with
// RunOnStart: true, so TestHeartbeat's client.Start also fires all three
// immediately — these stubs let that happen harmlessly instead of hitting
// cbr.ru/iss.moex.com from a test. The backfill job finds no operations in
// this test's empty database and returns before calling either provider, so
// it needs no stub behaviour of its own.
type stubFxProvider struct{}

// RatesOn's Source must match Name(): the backfill job looks up its coverage
// boundary via EarliestFxDate(provider.Name()), which filters fx_rates by
// source, so a mismatch here would make that lookup silently find nothing.
func (stubFxProvider) RatesOn(_ context.Context, on time.Time) ([]marketdata.FxRate, error) {
	return []marketdata.FxRate{{Base: "USD", Quote: "RUB", On: on, Rate: decimal.NewFromInt(90), Source: "stub-fx"}}, nil
}

func (stubFxProvider) Name() string { return "stub-fx" }

type stubQuoteProvider struct{}

func (stubQuoteProvider) QuotesFor(context.Context, []string, time.Time) ([]marketdata.TickerQuote, error) {
	return nil, nil
}

func (stubQuoteProvider) Name() string { return "stub-quotes" }

// TestStubFxProviderSourceMatchesName pins the invariant the comment above
// stubFxProvider.RatesOn documents: every rate it returns must carry
// Source == Name(), because the backfill job's coverage lookup
// (EarliestFxDate(provider.Name())) filters fx_rates by source. If this test
// were ever seeded with operations (making the backfill job actually run
// against these stubs, unlike today), a mismatch here would silently make
// the job think there is never any coverage to build on.
func TestStubFxProviderSourceMatchesName(t *testing.T) {
	provider := stubFxProvider{}
	rates, err := provider.RatesOn(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("RatesOn: %v", err)
	}
	for _, r := range rates {
		if r.Source != provider.Name() {
			t.Fatalf("rate source = %q, want %q (must match Name() for EarliestFxDate(provider.Name()) to find it)",
				r.Source, provider.Name())
		}
	}
}

// TestHeartbeat verifies that the River client starts, the periodic
// heartbeat job (RunOnStart) executes, and leaves a mark in meta.
func TestHeartbeat(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	mdStore := marketdata.NewStore(pool)
	instStore := instrument.NewStore(pool)
	opStore := operation.NewStore(pool)
	workers := jobs.NewWorkers(slog.Default(), pool, mdStore, instStore, opStore, stubFxProvider{}, stubQuoteProvider{})
	client, err := jobs.NewClient(pool, workers, slog.Default())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	}()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var v string
		err := pool.QueryRow(ctx,
			`SELECT value FROM meta WHERE key = 'last_heartbeat_at'`).Scan(&v)
		if err == nil && v != "" {
			return // success
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("heartbeat did not run within 15s")
}
