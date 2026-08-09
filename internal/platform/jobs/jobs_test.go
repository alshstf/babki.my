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
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/importer/tinvest"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/jobs"
	"babki.my/babki/internal/platform/secretbox"
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

// stubTinvestDeps is the T-Invest half of the same arrangement. The hourly
// dispatcher is registered with RunOnStart: true, so client.Start fires it
// immediately here too.
//
// THE CLIENT FACTORY REFUSES TO BUILD ANYTHING, and that is what keeps these
// tests off the network. In TestHeartbeat it is never reached at all: the
// database holds no active connection, so the dispatcher says there is nothing
// to sync and returns, exactly as the backfill job does. In the wiring test
// below it IS reached — a connection is waiting — and the sync job it was
// queued for fails there, one step before the first broker request. That is the
// intended end of that test: what it proves is that the job was queued, not
// that it could have succeeded.
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
		Reconciler: tinvest.NewReconciler(store, opStore, account.NewStore(pool), instrument.NewStore(pool), slog.Default()),
	}
}

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
	enqueuer := jobs.NewEnqueuer()
	workers := jobs.NewWorkers(slog.Default(), pool, mdStore, instStore, opStore, accStore, famStore,
		stubFxProvider{}, stubQuoteProvider{}, stubTinvestDeps(t, pool), enqueuer)
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

// THIS IS THE TEST THAT SAYS THE IMPORT IS SWITCHED ON. Everything the T-Invest
// module does is covered inside that module, against workers a test constructs
// itself; what no test there can see is whether this application ever runs
// them. Between "the module works" and "the feature happens" sit exactly three
// things, all of them here: the periodic job that fires the dispatcher, the
// workers registered to receive what it queues, and the Enqueuer, whose client
// is filled in after it was handed to the dispatcher (see NewClient) and
// through which every queued sync passes.
//
// EACH OF THE THREE FAILS SILENTLY IN PRODUCTION AND LOUDLY ONLY HERE. Drop the
// attachment in NewClient and every hourly dispatch answers "the job queue is
// not running yet", retries, fails again, and no connection is ever synced —
// with nothing but Error lines in a log to say so. Drop the periodic job and
// not even that: the dispatcher simply never runs. Both mutations were made and
// both turn this test red; nothing else in the repository notices either.
//
// A connection is seeded ACTIVE and its token sealed with the same box the
// workers were built with, so the run gets past the decryption and stops at the
// client factory, which refuses to build anything. What is waited for is
// therefore the sync job's ROW — the queue's own record that the dispatcher
// inserted it — and not its success, which no test may have without a broker.
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
	workers := jobs.NewWorkers(slog.Default(), pool, marketdata.NewStore(pool), instrument.NewStore(pool),
		operation.NewStore(pool), account.NewStore(pool), fam,
		stubFxProvider{}, stubQuoteProvider{}, deps, enqueuer)
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
			// The arguments too, and not merely a row of the right kind: a sync
			// queued for nobody, or under a trigger the run log refuses, would
			// satisfy a count and nothing else.
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

// TestAnInsertOnlyClientQueuesWithoutWorkingAnything is what stands behind the
// "api" role's «синхронизировать сейчас» button. That process registers no
// workers and starts no queue, and a client built that way is easy to assume
// cannot insert either — River's own documentation says otherwise and this is
// the measurement of it.
//
// It also checks the second half of the claim: that nothing is worked. The job
// is still sitting in the queue, unclaimed, a moment later — which is the whole
// point of the role, since the worker process is what must pick it up.
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
	// Available, not running and not completed: this process inserted the work
	// and left it for whoever works the queue.
	if state != "available" {
		t.Errorf("the job is %q, want available: an api process must not work the jobs it queues", state)
	}
}

// gracefulProbeArgs is a job kind that exists only inside the test below: it
// runs, says so, and then reports how its own context ended.
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

// TestSigtermLeavesARunningJobItsGracefulWindow measures the one thing
// jobs.SoftStopTimeout buys, and measures it as behaviour rather than as a
// field: a job already running when the process is signalled keeps its context
// for a while instead of losing it on the spot.
//
// THE SIGNAL IS MODELLED BY CANCELLING THE CONTEXT PASSED TO Start, because
// that is precisely what a signal does in this program — cmd/babki's "all" and
// "worker" roles both hand signal.NotifyContext's context to startJobClient,
// which hands it to Start. Without SoftStopTimeout set, River makes that
// context the parent of every job's context, so this cancellation reaches the
// worker below within microseconds and is indistinguishable from
// StopAndCancel; with it set, the work context is detached and the job runs on.
//
// The wait is a full second against a ten-second window — two orders of
// magnitude short of the window and three above the microseconds an
// inherited cancellation takes, so the assertion does not depend on the exact
// value of either. Deleting SoftStopTimeout from the config turns this red.
//
// The job is released rather than left blocking, so the client can stop
// normally afterwards and the test does not lean on the soft timeout firing.
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
	workers := jobs.NewWorkers(slog.Default(), pool, marketdata.NewStore(pool), instrument.NewStore(pool),
		operation.NewStore(pool), account.NewStore(pool), family.NewStore(pool),
		stubFxProvider{}, stubQuoteProvider{}, stubTinvestDeps(t, pool), enqueuer)
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
