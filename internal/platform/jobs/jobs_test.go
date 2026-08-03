package jobs_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/jobs"
	"babki.my/babki/internal/platform/testdb"
)

// stubFxProvider and stubQuoteProvider are network-free marketdata provider
// stand-ins. NewWorkers registers the fx/quotes/backfill periodic jobs with
// RunOnStart: true, so TestHeartbeat's client.Start also fires all three
// immediately — these stubs let that happen harmlessly instead of hitting
// cbr.ru/iss.moex.com from a test. The backfill job finds no operations in
// this test's empty database and returns before calling either provider, so
// its history methods need no stub behaviour of their own.
//
// stubFxProvider implements marketdata.FxHistoryProvider (not just
// FxProvider) because that is what NewWorkers takes: the history download
// needs a source that can deliver a whole date range in one request.
type stubFxProvider struct{}

func (stubFxProvider) RatesOn(_ context.Context, on time.Time) ([]marketdata.FxRate, error) {
	return []marketdata.FxRate{{Base: "USD", Quote: "RUB", On: on, Rate: decimal.NewFromInt(90), Source: "stub-fx"}}, nil
}

func (stubFxProvider) CurrencyIDs(context.Context) (map[string]string, error) {
	return map[string]string{"USD": "R01235"}, nil
}

func (stubFxProvider) RatesRange(_ context.Context, code, _ string, _, to time.Time) ([]marketdata.FxRate, error) {
	return []marketdata.FxRate{{Base: code, Quote: "RUB", On: to, Rate: decimal.NewFromInt(90), Source: "stub-fx"}}, nil
}

func (stubFxProvider) Name() string { return "stub-fx" }

type stubQuoteProvider struct{}

func (stubQuoteProvider) QuotesFor(context.Context, []string) ([]marketdata.TickerQuote, error) {
	return nil, nil
}

func (stubQuoteProvider) Name() string { return "stub-quotes" }

// TestHeartbeat verifies that the River client starts, the periodic
// heartbeat job (RunOnStart) executes, and leaves a mark in meta.
func TestHeartbeat(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	mdStore := marketdata.NewStore(pool)
	instStore := instrument.NewStore(pool)
	opStore := operation.NewStore(pool)
	accStore := account.NewStore(pool)
	famStore := family.NewStore(pool)
	workers := jobs.NewWorkers(slog.Default(), pool, mdStore, instStore, opStore, accStore, famStore,
		stubFxProvider{}, stubQuoteProvider{})
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
