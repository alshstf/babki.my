package jobs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/corporateaction"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/importer/tinvest"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/jobs"
	"babki.my/babki/internal/platform/secretbox"
	"babki.my/babki/internal/platform/testdb"
)

// Network-free providers. NewWorkers' periodic jobs run on start, and with an
// empty database the backfill returns before calling them.
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

// stubTinvestDeps' client factory refuses to build a client, which keeps the
// tests off the network: a queued sync fails one step before the broker.
func stubTinvestDeps(t *testing.T, pool *pgxpool.Pool) jobs.TinvestDeps {
	t.Helper()
	box, err := secretbox.New(bytes.Repeat([]byte{3}, secretbox.KeySize))
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	store := tinvest.NewStore(pool)
	opStore := operation.NewStore(pool)
	return jobs.TinvestDeps{
		Store: store,
		Box:   box,
		NewClient: func(string) (*tinvest.Client, error) {
			return nil, errors.New("stub: this test must never reach the broker")
		},
		NewRebuilder: func() *tinvest.Rebuilder {
			return tinvest.NewRebuilder(store, tinvest.NewResolver(store, instrument.NewStore(pool), slog.Default()),
				operation.NewService(opStore), opStore, slog.Default())
		},
		Reconciler: tinvest.NewReconciler(store, opStore, account.NewStore(pool), instrument.NewStore(pool), nil, slog.Default()),
	}
}

// stubCorporateActions returns real registry stores over the test pool: their
// periodic jobs run on start, so nil would panic in a worker.
func stubCorporateActions(pool *pgxpool.Pool) (*corporateaction.Store, *corporateaction.Materializer) {
	store := corporateaction.NewStore(pool)
	opStore := operation.NewStore(pool)
	return store, corporateaction.NewMaterializer(store, operation.NewService(opStore), instrument.NewStore(pool), nil, slog.Default())
}

// The client starts, the heartbeat runs on start and leaves its mark in meta.
func TestHeartbeat(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	mdStore := marketdata.NewStore(pool)
	instStore := instrument.NewStore(pool)
	opStore := operation.NewStore(pool)
	accStore := account.NewStore(pool)
	famStore := family.NewStore(pool)
	enqueuer := jobs.NewEnqueuer()
	caStore, caMaterializer := stubCorporateActions(pool)
	workers := jobs.NewWorkers(slog.Default(), pool, mdStore, instStore, opStore, accStore, famStore,
		stubFxProvider{}, stubQuoteProvider{}, stubTinvestDeps(t, pool),
		caStore, caMaterializer, enqueuer)
	client, err := jobs.NewClient(pool, workers, enqueuer, slog.Default())
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

// Starting the queue enqueues both registry jobs: the refresh that learns
// splits from the exchange and the sweep that applies events to journals.
// Without the periodic entries nothing else in the suite would notice.
func TestStartingTheQueueQueuesTheCorporateActionJobs(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	enqueuer := jobs.NewEnqueuer()
	caStore, caMaterializer := stubCorporateActions(pool)
	workers := jobs.NewWorkers(slog.Default(), pool, marketdata.NewStore(pool), instrument.NewStore(pool),
		operation.NewStore(pool), account.NewStore(pool), family.NewStore(pool),
		stubFxProvider{}, stubQuoteProvider{}, stubTinvestDeps(t, pool),
		caStore, caMaterializer, enqueuer)
	client, err := jobs.NewClient(pool, workers, enqueuer, slog.Default())
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

	for _, kind := range []string{
		"corporateaction.refresh_moex_splits",
		"corporateaction.materialize_all",
	} {
		deadline := time.Now().Add(30 * time.Second)
		for {
			var n int
			if err := pool.QueryRow(ctx,
				`SELECT count(*) FROM river_job WHERE kind = $1`, kind).Scan(&n); err != nil {
				t.Fatalf("count %s: %v", kind, err)
			}
			if n > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s was never queued: the registry is wired but nothing asks it to run", kind)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// Starting the queue syncs an active connection: the periodic dispatcher, the
// registered workers and the Enqueuer attached in NewClient all have to work.
// The sync's queued row is the evidence; it fails at the refusing client.
func TestStartingTheQueueQueuesASyncForAnActiveConnection(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	fam := family.NewStore(pool)
	user, err := fam.CreateUser(ctx, "alex", "Александр", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	space, err := fam.CreateSpaceWithOwner(ctx, "Семья", user.ID)
	if err != nil {
		t.Fatalf("CreateSpaceWithOwner: %v", err)
	}
	deps := stubTinvestDeps(t, pool)
	conn, err := tinvest.NewStore(pool).CreateConnection(ctx, space.ID,
		deps.Box.Seal([]byte("t.a-read-only-token")), "oken", tinvest.StatusActive)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}

	enqueuer := jobs.NewEnqueuer()
	caStore, caMaterializer := stubCorporateActions(pool)
	workers := jobs.NewWorkers(slog.Default(), pool, marketdata.NewStore(pool), instrument.NewStore(pool),
		operation.NewStore(pool), account.NewStore(pool), fam,
		stubFxProvider{}, stubQuoteProvider{}, deps, caStore, caMaterializer, enqueuer)
	client, err := jobs.NewClient(pool, workers, enqueuer, slog.Default())
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

	deadline := time.Now().Add(30 * time.Second)
	for {
		var raw []byte
		err := pool.QueryRow(ctx,
			`SELECT args FROM river_job WHERE kind = 'tinvest.sync' ORDER BY id LIMIT 1`).Scan(&raw)
		if err == nil {
			// The arguments too: a count alone would accept a sync queued for nobody.
			var args struct {
				ConnectionID string `json:"connection_id"`
				Trigger      string `json:"trigger"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				t.Fatalf("decode the queued job's args %s: %v", raw, err)
			}
			if args.ConnectionID != conn.ID.String() || args.Trigger != "schedule" {
				t.Fatalf("queued %+v, want {%s schedule}", args, conn.ID)
			}
			return // success
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("read river_job: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("no sync job was queued for the active connection within 30s; " +
				"the hourly dispatch is not reaching the queue")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// The api role's insert-only client can queue a job, and leaves it unworked
// for the worker process.
func TestAnInsertOnlyClientQueuesWithoutWorkingAnything(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	client, err := jobs.NewInsertOnlyClient(pool, slog.Default())
	if err != nil {
		t.Fatalf("NewInsertOnlyClient: %v", err)
	}
	connID := uuid.New()
	res, err := tinvest.EnqueueSync(ctx, client, connID, tinvest.TriggerManual)
	if err != nil {
		t.Fatalf("EnqueueSync through an insert-only client: %v", err)
	}
	if res.UniqueSkippedAsDuplicate {
		t.Fatal("the first sync of a connection was skipped as a duplicate")
	}

	var raw []byte
	var state string
	if err := pool.QueryRow(ctx,
		`SELECT args, state FROM river_job WHERE kind = 'tinvest.sync'`).Scan(&raw, &state); err != nil {
		t.Fatalf("read river_job: %v", err)
	}
	var args struct {
		ConnectionID string `json:"connection_id"`
		Trigger      string `json:"trigger"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatalf("decode the queued job's args %s: %v", raw, err)
	}
	if args.ConnectionID != connID.String() || args.Trigger != "manual" {
		t.Fatalf("queued %+v, want {%s manual}", args, connID)
	}
	if state != "available" {
		t.Errorf("the job is %q, want available: an api process must not work the jobs it queues", state)
	}
}

// gracefulProbeArgs runs, reports, and then reports how its context ended.
type gracefulProbeArgs struct{}

func (gracefulProbeArgs) Kind() string { return "test.graceful_probe" }

type gracefulProbeWorker struct {
	river.WorkerDefaults[gracefulProbeArgs]
	started   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
}

func (w *gracefulProbeWorker) Work(ctx context.Context, _ *river.Job[gracefulProbeArgs]) error {
	close(w.started)
	select {
	case <-ctx.Done():
		close(w.cancelled)
	case <-w.release:
	}
	return nil
}

// Cancelling Start's context, as a signal does, leaves a running job its
// context for SoftStopTimeout. Without the setting River cancels it at once.
// The one-second wait sits far from both the window and an inherited
// cancellation.
func TestSigtermLeavesARunningJobItsGracefulWindow(t *testing.T) {
	pool := testdb.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	probe := &gracefulProbeWorker{
		started:   make(chan struct{}),
		cancelled: make(chan struct{}),
		release:   make(chan struct{}),
	}
	enqueuer := jobs.NewEnqueuer()
	caStore, caMaterializer := stubCorporateActions(pool)
	workers := jobs.NewWorkers(slog.Default(), pool, marketdata.NewStore(pool), instrument.NewStore(pool),
		operation.NewStore(pool), account.NewStore(pool), family.NewStore(pool),
		stubFxProvider{}, stubQuoteProvider{}, stubTinvestDeps(t, pool),
		caStore, caMaterializer, enqueuer)
	river.AddWorker(workers, probe)

	client, err := jobs.NewClient(pool, workers, enqueuer, slog.Default())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := client.Insert(ctx, gracefulProbeArgs{}, nil); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	select {
	case <-probe.started:
	case <-time.After(30 * time.Second):
		t.Fatal("the probe job never started")
	}

	cancel() // the SIGTERM

	select {
	case <-probe.cancelled:
		t.Fatal("the running job's context was cancelled the moment the process was signalled: " +
			"it got no graceful window at all, which is what jobs.SoftStopTimeout is set to prevent")
	case <-time.After(time.Second):
	}

	close(probe.release)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()
	if err := client.Stop(stopCtx); err != nil {
		t.Fatalf("Stop after the job finished on its own: %v", err)
	}
}

// A registry retry queued by a request has a worker. The api role's client
// has none, so a missing registration would wait forever silently.
func TestTheRegistrysRetryJobHasAWorker(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	enqueuer := jobs.NewEnqueuer()
	caStore, caMaterializer := stubCorporateActions(pool)
	workers := jobs.NewWorkers(slog.Default(), pool, marketdata.NewStore(pool), instrument.NewStore(pool),
		operation.NewStore(pool), account.NewStore(pool), family.NewStore(pool),
		stubFxProvider{}, stubQuoteProvider{}, stubTinvestDeps(t, pool), caStore, caMaterializer, enqueuer)
	client, err := jobs.NewClient(pool, workers, enqueuer, slog.Default())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.Insert(ctx, corporateaction.MaterializeISINArgs{ISIN: "US0231351067"},
		corporateaction.MaterializeISINInsertOpts()); err != nil {
		t.Fatalf("the worker's own queue will not take the registry's retry job: %v", err)
	}
}
