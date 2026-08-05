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
func mountModules(srv *httpserver.Server, r *rt) {
	famStore := family.NewStore(r.pool)
	famSvc := family.NewService(famStore)
	famSM := family.NewSessionManager(r.pool)
	famAuth := family.NewAuth(famSM, famStore)
	family.NewHandler(famSvc, famStore, famAuth, famSM).Mount(srv)
	mdStore := marketdata.NewStore(r.pool)
	converter := marketdata.NewConverter(mdStore)
	account.NewHandler(account.NewStore(r.pool), famStore, converter, famAuth, famSM).Mount(srv)
	instStore := instrument.NewStore(r.pool)
	instrument.NewHandler(instStore, famAuth, famSM).Mount(srv)
	opStore := operation.NewStore(r.pool)
	operation.NewHandler(operation.NewService(opStore), opStore, famStore, converter, famAuth, famSM).Mount(srv)
	portfolio.NewHandler(opStore, instStore, mdStore, converter, famStore, famAuth, famSM).Mount(srv)
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
	accStore *account.Store,
) (jobs.TinvestDeps, error) {
	store := tinvest.NewStore(r.pool)
	hc, err := tinvest.NewHTTPClient(tinvestHTTPTimeout)
	if err != nil {
		// THIS ONE FAILURE STOPS EVERY BACKGROUND JOB, not merely the import,
		// and the text has to say so — the person reading it will be looking at
		// missing exchange rates and stale quotes and wondering what those have
		// to do with a broker they may not even have connected.
		//
		// It is left fatal all the same: the only way NewHTTPClient refuses is
		// an embedded certificate that will not parse, which means the binary
		// itself was built wrong. Starting anyway would hide a broken build
		// behind a module that happens to be idle on this instance.
		return jobs.TinvestDeps{}, fmt.Errorf(
			"the T-Invest importer's HTTPS trust could not be built, so the background job "+
				"queue does not start at all and nothing else it runs — exchange rates, quotes — "+
				"will run either; nothing about this instance's configuration causes it: %w", err)
	}
	return jobs.TinvestDeps{
		Store: store,
		Box:   r.box,
		NewClient: func(token string) (*tinvest.Client, error) {
			// Base URL empty: the production gateway. Parameterized in the
			// package for tests, not configured here.
			return tinvest.NewClient(hc, "", token, r.log), nil
		},
		NewRebuilder: func() *tinvest.Rebuilder {
			return tinvest.NewRebuilder(store, tinvest.NewResolver(store, instStore, r.log),
				operation.NewService(opStore), opStore, r.log)
		},
		Reconciler: tinvest.NewReconciler(store, opStore, accStore, r.log),
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
	tinvestDeps, err := newTinvestDeps(r, instStore, opStore, accStore)
	if err != nil {
		return nil, err
	}
	enqueuer := jobs.NewEnqueuer()
	workers := jobs.NewWorkers(r.log, r.pool, mdStore, instStore, opStore, accStore, famStore,
		fxProvider, quoteProvider, tinvestDeps, enqueuer)
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

func signalCtx() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

func newAllCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "all",
		Short: "API + worker в одном процессе (режим homelab)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signalCtx()
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
			mountModules(srv, r)
			srv.Mount("/", web.Handler())

			// Sequenced shutdown: let the HTTP server fully drain in-flight
			// requests first, then stop the job queue. This avoids cutting
			// off jobs enqueued by a request that's still being handled.
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
			ctx, stop := signalCtx()
			defer stop()
			r, err := setup(ctx, true, true)
			if err != nil {
				return err
			}
			defer r.close()
			srv := httpserver.New(r.log, r.pool)
			mountModules(srv, r)
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
			ctx, stop := signalCtx()
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
			ctx, stop := signalCtx()
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
