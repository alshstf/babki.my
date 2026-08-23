package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/corporateaction"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/importer/tinvest"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/marketdata/cbr"
	"babki.my/babki/internal/marketdata/moex"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/jobs"
	"babki.my/babki/internal/platform/version"
	"babki.my/babki/internal/portfolio"
	"babki.my/babki/web"
)

// mountModules builds each domain module and mounts its routes on srv.
// Shared by the "all" and "api" roles so route wiring lives in one place.
//
// inserter is how a request queues background work — today the "sync now"
// button, which goes into the same River queue and the same class of uniqueness
// the hourly schedule uses (see tinvest.EnqueueSync). It is passed in rather
// than built here because the two roles get it from opposite places: "all"
// hands over the client it already started to work jobs, and "api" builds an
// insert-only one, since that process works no jobs at all.
//
// It returns an error because the T-Invest module needs an HTTPS transport
// carrying the gateway's certificate, and building it can fail on a binary
// whose embedded certificate will not parse (see newTinvestClientFactory).
func mountModules(srv *httpserver.Server, r *rt, inserter *river.Client[pgx.Tx]) error {
	famStore := family.NewStore(r.pool)
	famSvc := family.NewService(famStore)
	famSM := family.NewSessionManager(r.pool)
	famAuth := family.NewAuth(famSM, famStore)
	family.NewHandler(famSvc, famStore, famAuth, famSM).Mount(srv)
	mdStore := marketdata.NewStore(r.pool)
	converter := marketdata.NewConverter(mdStore)
	accStore := account.NewStore(r.pool)
	account.NewHandler(accStore, famStore, converter, famAuth, famSM).Mount(srv)
	instStore := instrument.NewStore(r.pool)
	instrument.NewHandler(instStore, famAuth, famSM).Mount(srv)
	opStore := operation.NewStore(r.pool)
	opSvc := operation.NewService(opStore)
	operation.NewHandler(opSvc, opStore, famStore, converter, famAuth, famSM).Mount(srv)
	portfolio.NewHandler(opStore, instStore, mdStore, converter, famStore, famAuth, famSM).Mount(srv)

	newClient, err := newTinvestClientFactory(r)
	if err != nil {
		return err
	}
	// r.box is dereferenced whenever a token is stored or replaced, so this
	// must only ever be reached from a role that required the encryption key —
	// which is exactly the two roles that mount modules ("all" and "api"; see
	// setup's requireEncryptionKey).
	tinvestSvc := tinvest.NewService(tinvest.NewStore(r.pool), accStore, opSvc, r.box, newClient, inserter, r.log)
	tinvest.NewHandler(tinvestSvc, famAuth, famSM).Mount(srv)
	return nil
}

// cbrHTTPTimeout bounds every request the cbr.ru client makes. The history
// download fires one request per currency in use under a 15-minute job
// timeout: without a bound here, cbr.New would fall back to
// http.DefaultClient, whose Timeout is 0 (none), so one stalled TCP
// connection could pin a worker slot for the job's whole budget. 15s is
// unchanged from when the client only ever fetched one day's document,
// because it still fits the larger answers with room to spare: a whole
// thirteen-year series measures ~400KB, which needs only ~27KB/s to arrive
// in time.
const cbrHTTPTimeout = 15 * time.Second

// newCbrHTTPClient builds the HTTP client used for every request to cbr.ru.
// Factored out of startJobClient so cbrHTTPTimeout is unit-testable without
// constructing the rest of startJobClient's dependencies (a live pool, job
// workers, a River client).
func newCbrHTTPClient() *http.Client {
	return &http.Client{Timeout: cbrHTTPTimeout}
}

// tinvestHTTPTimeout bounds every request to the T-Invest REST gateway. It is
// stated here rather than left to the package default so that the one client
// this process builds has a timeout chosen where the rest of the process's
// timeouts are (see cbrHTTPTimeout). 30s is generous for a single page of
// operations and short enough that a stalled connection cannot eat much of the
// sync job's fifteen-minute budget.
const tinvestHTTPTimeout = 30 * time.Second

// newTinvestDeps assembles what the T-Invest import jobs run on. The two factories
// exist for reasons the types themselves state: a broker client is per token,
// and a Rebuilder is per RUN, because the passport cache it carries is bounded
// by the run and is not safe for concurrent use.
//
// The transport is built ONCE and shared by every client the factory makes: it
// carries the certificate pool the gateway needs (see tinvest.NewHTTPClient) and
// building one per run would rebuild that pool on every sync, per connection,
// forever.
func newTinvestDeps(r *rt, instStore *instrument.Store, opStore *operation.Store,
	accStore *account.Store, converter *marketdata.Converter,
) (jobs.TinvestDeps, error) {
	store := tinvest.NewStore(r.pool)
	newClient, err := newTinvestClientFactory(r)
	if err != nil {
		// THIS ONE FAILURE STOPS EVERY BACKGROUND JOB, not merely the import,
		// and the text has to say so — the person reading it will be looking at
		// missing exchange rates and stale quotes and wondering what those have
		// to do with a broker they may not even have connected.
		//
		// It is left fatal all the same: the only way the factory refuses is an
		// embedded certificate that will not parse, which means the binary
		// itself was built wrong. Starting anyway would hide a broken build
		// behind a module that happens to be idle on this instance.
		return jobs.TinvestDeps{}, fmt.Errorf(
			"the background job queue does not start at all and nothing else it runs — "+
				"exchange rates, quotes — will run either; nothing about this instance's "+
				"configuration causes it: %w", err)
	}
	return jobs.TinvestDeps{
		Store:     store,
		Box:       r.box,
		NewClient: newClient,
		NewRebuilder: func() *tinvest.Rebuilder {
			// The resolver is given the rate table for one purpose: proving
			// what a currency pair the broker has FORGOTTEN trades, from the
			// price the trade was struck at (see Resolver.currencyFromHint).
			resolver := tinvest.NewResolver(store, instStore, r.log).WithRates(converter)
			return tinvest.NewRebuilder(store, resolver, operation.NewService(opStore), opStore, r.log)
		},
		Reconciler: tinvest.NewReconciler(store, opStore, accStore, instStore, r.log),
	}, nil
}

// newTinvestClientFactory builds the per-token broker client factory that both
// halves of the importer run on: the sync worker, which reads history, and the
// request path, which checks a token before storing it.
//
// The transport is built ONCE per call and shared by every client the returned
// factory makes: it carries the certificate pool the gateway needs (see
// tinvest.NewHTTPClient), and building one per client would rebuild that pool on
// every token check and every sync run, forever. The two callers get one
// transport each, which is one per process role and not one per operation.
func newTinvestClientFactory(r *rt) (func(token string) (*tinvest.Client, error), error) {
	hc, err := tinvest.NewHTTPClient(tinvestHTTPTimeout)
	if err != nil {
		return nil, fmt.Errorf("the T-Invest importer's HTTPS trust could not be built: %w", err)
	}
	return func(token string) (*tinvest.Client, error) {
		// Base URL empty: the production gateway. Parameterized in the
		// package for tests, not configured here.
		return tinvest.NewClient(hc, "", token, r.log), nil
	}, nil
}

// startJobClient wires up the job workers and River client and starts it.
// Shared by the "all" and "worker" roles. cbr and moex are used with their
// default base URLs — no configuration knob is exposed for them yet. cbr's
// HTTP client is bounded by cbrHTTPTimeout (see above); moex isn't. The
// reason given here used to be that a quotes run makes a single request, and
// that stopped being true when the provider took on the corporate-bond boards
// — it now makes one request per board. Nothing has replaced the reason: an
// unbounded client on the quotes path is a gap, not a decision, and it is
// filed rather than fixed here.
//
// r.box is dereferenced by the T-Invest sync worker, so this must only ever be
// reached from a role that required the encryption key — which is exactly the
// two roles that call it ("all" and "worker"; see setup's requireEncryptionKey).
func startJobClient(ctx context.Context, r *rt) (*river.Client[pgx.Tx], error) {
	mdStore := marketdata.NewStore(r.pool)
	instStore := instrument.NewStore(r.pool)
	opStore := operation.NewStore(r.pool)
	accStore := account.NewStore(r.pool)
	famStore := family.NewStore(r.pool)
	fxProvider := cbr.New(newCbrHTTPClient(), "")
	quoteProvider := moex.New(nil, "", r.log)
	tinvestDeps, err := newTinvestDeps(r, instStore, opStore, accStore, marketdata.NewConverter(mdStore))
	if err != nil {
		return nil, err
	}
	// The corporate-actions registry writes journal rows through the same door
	// the importer uses (operation.Service.ApplyImportDelta), so it gets a
	// service of its own rather than the store: the engine has to be asked about
	// the journal the difference LEAVES, not only about the rows added.
	caStore := corporateaction.NewStore(r.pool)
	caMaterializer := corporateaction.NewMaterializer(
		caStore, opStore, operation.NewService(opStore), r.log)
	enqueuer := jobs.NewEnqueuer()
	workers := jobs.NewWorkers(r.log, r.pool, mdStore, instStore, opStore, accStore, famStore,
		fxProvider, quoteProvider, tinvestDeps, caStore, caMaterializer, enqueuer)
	client, err := jobs.NewClient(r.pool, workers, enqueuer, r.log)
	if err != nil {
		return nil, err
	}
	if err := client.Start(ctx); err != nil {
		return nil, err
	}
	return client, nil
}

// stopJobClientTimeout bounds the graceful River stop; if it isn't done in
// time we escalate to a forced cancel rather than hang the process shutdown.
//
// IT IS THE OUTER OF TWO BOUNDS and has to stay the longer one. The inner is
// jobs.SoftStopTimeout, after which River cancels the contexts of jobs still
// running; this one covers that escalation and the unwinding that follows it.
// Were it the shorter, every shutdown that used the whole soft window would
// report a graceful stop that "did not complete in time" and escalate to
// StopAndCancel — killing the jobs the soft window exists to spare.
// TestTheJobQueueIsGivenLessTimeToStopThanTheProcessWaitsForIt keeps the order.
const stopJobClientTimeout = 15 * time.Second

// stopJobClientForceTimeout bounds the forced StopAndCancel fallback.
const stopJobClientForceTimeout = 5 * time.Second

// stopJobClient performs a bounded, graceful shutdown of the job client. If
// the graceful stop doesn't complete within stopJobClientTimeout (e.g. it
// returns a context error), it escalates to StopAndCancel — which cancels
// in-progress job contexts — bounded by stopJobClientForceTimeout, so the
// process always terminates promptly instead of hanging.
func stopJobClient(client *river.Client[pgx.Tx], log *slog.Logger) {
	stopCtx, cancel := context.WithTimeout(context.Background(), stopJobClientTimeout)
	defer cancel()
	if err := client.Stop(stopCtx); err != nil {
		log.Warn("job client graceful stop did not complete in time, forcing cancel", "err", err)
		forceCtx, forceCancel := context.WithTimeout(context.Background(), stopJobClientForceTimeout)
		defer forceCancel()
		if err := client.StopAndCancel(forceCtx); err != nil {
			log.Error("job client forced stop failed", "err", err)
		}
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "babki",
		Short:         "babki.my — учет семейных финансов (fair source)",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.AddCommand(newAllCmd(), newAPICmd(), newWorkerCmd(), newMigrateCmd(), newVersionCmd(), newSeedCmd())
	return root
}

// signalCtx derives the context every long-running role blocks on: cancelled
// by SIGINT or SIGTERM, and also by whatever cancels the parent.
//
// THE PARENT IS THE COMMAND'S OWN CONTEXT, not context.Background, and that is
// what makes a role runnable from a test at all: cobra's Execute installs
// Background here, so signals behave in production exactly as before, while
// ExecuteContext lets a caller hand in a context it can cancel — which is how
// the role smoke tests shut a server down without raising a real signal at the
// test binary.
func signalCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
}

func newAllCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "all",
		Short: "API + worker в одном процессе (режим homelab)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signalCtx(cmd.Context())
			defer stop()
			r, err := setup(ctx, true, true)
			if err != nil {
				return err
			}
			defer r.close()

			client, err := startJobClient(ctx, r)
			if err != nil {
				return err
			}
			srv := httpserver.New(r.log, r.pool)
			// The very client this process works jobs with is what its requests
			// enqueue through: one queue, one class of uniqueness, so a manual
			// sync and the hourly one cannot run over a connection at once.
			if err := mountModules(srv, r, client); err != nil {
				stopJobClient(client, r.log)
				return err
			}
			srv.Mount("/", web.Handler())

			// Sequenced shutdown: the HTTP server drains its in-flight
			// requests first, and the job queue's bounded stop begins only
			// after the last handler has returned. By then the signal has
			// already stopped the producers FETCHING — that happens the
			// moment ctx is cancelled — while jobs already running keep the
			// window jobs.SoftStopTimeout gives them, so the two shutdowns
			// overlap without either cutting the other short.
			g, gctx := errgroup.WithContext(ctx)
			g.Go(func() error { return srv.Run(gctx, r.cfg.HTTPAddr) })
			err = g.Wait()
			stopJobClient(client, r.log)
			return err
		},
	}
}

func newAPICmd() *cobra.Command {
	return &cobra.Command{
		Use:   "api",
		Short: "Только HTTP API",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signalCtx(cmd.Context())
			defer stop()
			r, err := setup(ctx, true, true)
			if err != nil {
				return err
			}
			defer r.close()
			// An INSERT-ONLY River client: this role works no jobs, and one
			// that could would compete with the worker process for them. It is
			// deliberately never Start()ed and never Stop()ped — a client with
			// no queues configured does nothing in the background, so there is
			// nothing to shut down (see jobs.NewInsertOnlyClient).
			inserter, err := jobs.NewInsertOnlyClient(r.pool, r.log)
			if err != nil {
				return err
			}
			srv := httpserver.New(r.log, r.pool)
			if err := mountModules(srv, r, inserter); err != nil {
				return err
			}
			srv.Mount("/", web.Handler())
			return srv.Run(ctx, r.cfg.HTTPAddr)
		},
	}
}

func newWorkerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "worker",
		Short: "Только фоновые задачи",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signalCtx(cmd.Context())
			defer stop()
			r, err := setup(ctx, true, true)
			if err != nil {
				return err
			}
			defer r.close()

			client, err := startJobClient(ctx, r)
			if err != nil {
				return err
			}
			<-ctx.Done()
			stopJobClient(client, r.log)
			return nil
		},
	}
}

func newMigrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Накатить миграции и выйти",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signalCtx(cmd.Context())
			defer stop()
			// requireEncryptionKey=false: migrate exists precisely so schema
			// can be applied on a machine where secrets have not been
			// provisioned yet.
			r, err := setup(ctx, false, false)
			if err != nil {
				return err
			}
			defer r.close()
			return db.Migrate(ctx, r.pool)
		},
	}
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Версия сборки",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), version.Version)
			return err
		},
	}
}
