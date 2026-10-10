package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/background"
	"babki.my/babki/internal/cashflow"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/corporateaction"
	"babki.my/babki/internal/creditcard"
	"babki.my/babki/internal/export"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/forecast"
	"babki.my/babki/internal/importer/table"
	"babki.my/babki/internal/importer/tinvest"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/loan"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/marketdata/cbr"
	"babki.my/babki/internal/marketdata/coingecko"
	"babki.my/babki/internal/marketdata/finex"
	"babki.my/babki/internal/marketdata/moex"
	"babki.my/babki/internal/marketdata/tcapital"
	"babki.my/babki/internal/marketdata/yahoo"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/payouts"
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/jobs"
	"babki.my/babki/internal/platform/metrics"
	"babki.my/babki/internal/platform/version"
	"babki.my/babki/internal/portfolio"
	"babki.my/babki/internal/recurring"
	"babki.my/babki/internal/structure"
	"babki.my/babki/web"
)

// journalValues hands the portfolio engine's valuation of an account to the
// account module, which counts brokerage accounts in the family total by their
// journal and must not import the engine to do it.
type journalValues struct{ positions *portfolio.Service }

func (j journalValues) ValueFromJournal(ctx context.Context, spaceID, accountID uuid.UUID) (account.JournalValue, error) {
	v, err := j.positions.ValueFromJournal(ctx, spaceID, accountID)
	return account.JournalValue(v), err
}

func (j journalValues) ValueOn(ctx context.Context, spaceID, accountID uuid.UUID, day time.Time) (account.JournalValue, error) {
	v, err := j.positions.ValueOn(ctx, spaceID, accountID, day)
	return account.JournalValue(v), err
}

func (j journalValues) ValuesOn(ctx context.Context, spaceID, accountID uuid.UUID, days []time.Time) ([]account.JournalValue, error) {
	values, err := j.positions.ValuesOn(ctx, spaceID, accountID, days)
	out := make([]account.JournalValue, len(values))
	for i, v := range values {
		out[i] = account.JournalValue(v)
	}
	return out, err
}

func (j journalValues) ReturnBasis(ctx context.Context, spaceID, accountID uuid.UUID, from, to time.Time) (account.ReturnBasis, error) {
	b, err := j.positions.ReturnBasis(ctx, spaceID, accountID, from, to)
	out := account.ReturnBasis{Start: account.JournalValue(b.Start), End: account.JournalValue(b.End), Complete: b.Complete}
	for _, f := range b.Flows {
		out.Flows = append(out.Flows, account.ReturnFlow(f))
	}
	return out, err
}

// mountModules builds each domain module and mounts its routes, for the "all"
// and "api" roles. inserter is how requests queue work ("sync now"), into the
// same queue and uniqueness class as the schedule: "all" passes its working
// client, "api" an insert-only one. It fails only if the T-Invest transport
// cannot be built (an unparsable embedded certificate).
func mountModules(srv *httpserver.Server, r *rt, inserter *river.Client[pgx.Tx]) error {
	famStore := family.NewStore(r.pool)
	famSvc := family.NewService(famStore)
	famSM := family.NewSessionManager(r.pool)
	famSM.Cookie.Secure = r.cfg.CookieSecure
	famAuth := family.NewAuth(famSM, famStore)
	family.NewHandler(famSvc, famStore, famAuth, famSM).WithSetupCode(setupCode(r, famSvc)).Mount(srv)
	mdStore := marketdata.NewStore(r.pool)
	converter := marketdata.NewConverter(mdStore)
	instStore := instrument.NewStore(r.pool)
	instrument.NewHandler(instStore, famAuth, famSM).Mount(srv)
	category.NewHandler(category.NewStore(r.pool), famAuth, famSM).Mount(srv)
	opStore := operation.NewStore(r.pool)
	opSvc := operation.NewService(opStore)
	operation.NewHandler(opSvc, opStore, famStore, converter, famAuth, famSM).
		WithDividendCalendar(mdStore, instStore).Mount(srv)
	accStore := account.NewStore(r.pool)
	// The accounts tell the valuation whose broker trades abroad (Р-20).
	positions := portfolio.NewService(opStore, instStore, mdStore, converter, famStore).WithAccounts(accStore)
	portfolio.NewHandler(positions, famAuth, famSM).Mount(srv)
	background.NewStatusHandler(r.pool, famAuth, famSM).Mount(srv)
	background.NewTasksHandler(r.pool, famAuth, famSM).Mount(srv)
	// For an outside watcher, and Prometheus when asked for (decision Р-22).
	background.NewDataHealthHandler(r.pool).Mount(srv)
	if r.cfg.Metrics {
		metrics.Registry.MustRegister(background.NewSourcesCollector(r.pool))
		srv.Mount("GET /metrics", metrics.Handler())
	}
	account.NewHandler(accStore, famStore, converter, journalValues{positions}, famAuth, famSM).Mount(srv)
	structure.NewHandler(structure.NewService(accStore, positions, famStore, converter), famAuth, famSM).Mount(srv)
	loan.NewHandler(loan.NewService(r.pool, accStore, opSvc, category.NewStore(r.pool)), famAuth, famSM).Mount(srv)
	creditcard.NewHandler(creditcard.NewService(r.pool, accStore, opStore), famAuth, famSM).Mount(srv)
	cashflow.NewHandler(cashflow.NewService(opStore, accStore, category.NewStore(r.pool), famStore, converter), famAuth, famSM).Mount(srv)
	payouts.NewHandler(payouts.NewService(opStore, accStore, mdStore, famStore, converter), famAuth, famSM).Mount(srv)
	recurringSvc := recurring.NewService(opStore, accStore)
	recurring.NewHandler(recurringSvc, famAuth, famSM).Mount(srv)
	forecast.NewHandler(forecast.NewService(accStore, positions, famStore, recurringSvc,
		loan.NewService(r.pool, accStore, opSvc, category.NewStore(r.pool)), converter), famAuth, famSM).Mount(srv)
	table.NewHandler(table.NewService(accStore, instStore, opStore, opSvc, table.NewStore(r.pool),
		moex.New(newMoexHTTPClient(), "", r.log), category.NewStore(r.pool)), instStore, famAuth, famSM).Mount(srv)

	newClient, err := newTinvestClientFactory(r)
	if err != nil {
		return err
	}
	// r.box is used to store tokens; only roles that required the key
	// ("all", "api") reach here.
	tinvestStore := tinvest.NewStore(r.pool)
	tinvestSvc := tinvest.NewService(tinvestStore, accStore, opSvc, opStore, r.box, newClient, inserter, r.log)
	tinvest.NewHandler(tinvestSvc, famAuth, famSM).Mount(srv)

	// The registry's HTTP door gets what the worker role gives it: a
	// materializer, so a recorded split reaches journals before the answer, and
	// a rechecker, so a stale verdict is refreshed.
	caStore := corporateaction.NewStore(r.pool)
	caMaterializer := corporateaction.NewMaterializer(caStore, opSvc, instStore,
		tinvest.NewRechecker(tinvestStore, inserter, r.log), r.log)
	corporateaction.NewHandler(caStore, caMaterializer, inserter, famAuth, famSM, r.log).Mount(srv)
	export.NewHandler(famStore, accStore, opStore, instStore, caStore, mdStore, category.NewStore(r.pool), famAuth, famSM).Mount(srv)
	// A hand entry is followed by the registry at once (a purchase before a
	// known split must not wait for the sweep); opSvc is the one service every
	// hand-entry door writes through.
	opSvc.OnManualWrite(caMaterializer.AfterManualWrite)
	return nil
}

// cbrHTTPTimeout bounds every cbr.ru request; without it cbr.New would use
// http.DefaultClient with no timeout, and one stalled connection could hold a
// worker for the job's 15 minutes. A thirteen-year series is ~400 KB.
const cbrHTTPTimeout = 15 * time.Second

// newCbrHTTPClient builds the cbr.ru client, separately so its timeout is
// testable.
func newCbrHTTPClient() *http.Client {
	return &http.Client{Timeout: cbrHTTPTimeout}
}

// moexHTTPTimeout bounds every MOEX ISS request, as cbrHTTPTimeout does; a
// board listing is a few megabytes.
const moexHTTPTimeout = 30 * time.Second

// newMoexHTTPClient builds the HTTP client used for every request to MOEX ISS.
func newMoexHTTPClient() *http.Client {
	return &http.Client{Timeout: moexHTTPTimeout}
}

// referenceHTTPTimeout bounds every request to the outside feeds (FinEx NAV,
// Yahoo Finance prices and dividends); a fund's whole history is about 200 KB.
const referenceHTTPTimeout = 30 * time.Second

// tinvestHTTPTimeout bounds every T-Invest request, set here with the
// process's other timeouts; generous for a page, short against the sync's
// fifteen minutes.
const tinvestHTTPTimeout = 30 * time.Second

// newTinvestDeps assembles what the T-Invest jobs run on: a client factory
// (clients are per token) and a Rebuilder factory (per run; its passport cache is
// not safe for concurrent use). The transport is built once and shared.
func newTinvestDeps(r *rt, instStore *instrument.Store, opStore *operation.Store,
	accStore *account.Store, converter *marketdata.Converter,
) (background.TinvestDeps, error) {
	store := tinvest.NewStore(r.pool)
	newClient, err := newTinvestClientFactory(r)
	if err != nil {
		// This stops every background job, not just the import, so the message
		// says so. Fatal anyway: it means the binary was built wrong.
		return background.TinvestDeps{}, fmt.Errorf(
			"the background job queue does not start at all and nothing else it runs — "+
				"exchange rates, quotes — will run either; nothing about this instance's "+
				"configuration causes it: %w", err)
	}
	// One exchange client for every rebuild, so its schedules are
	// remembered.
	faces := moex.New(newMoexHTTPClient(), "", r.log)
	return background.TinvestDeps{
		Store:     store,
		Box:       r.box,
		NewClient: newClient,
		NewRebuilder: func() *tinvest.Rebuilder {
			// Rates prove what a forgotten currency pair traded (see
			// Resolver.currencyFromHint).
			resolver := tinvest.NewResolver(store, instStore, r.log).WithRates(converter).
				// A paper the broker forgot is created from the exchange's reference
				// (Р-19).
				WithExchange(faces)
			// Repayment schedules measure partial repayments against outstanding
			// face (Р-4).
			return tinvest.NewRebuilder(store, resolver, operation.NewService(opStore), opStore, r.log).
				WithFaceSchedule(faces)
		},
		Reconciler: tinvest.NewReconciler(store, opStore, accStore, instStore, corporateaction.NewStore(r.pool), r.log),
	}, nil
}

// newTinvestClientFactory builds the per-token client factory for the sync
// worker and the token check. The transport, with the gateway's certificate pool,
// is built once per call and shared by every client the factory makes.
func newTinvestClientFactory(r *rt) (func(token string) (*tinvest.Client, error), error) {
	hc, err := tinvest.NewHTTPClient(tinvestHTTPTimeout)
	if err != nil {
		return nil, fmt.Errorf("the T-Invest importer's HTTPS trust could not be built: %w", err)
	}
	return func(token string) (*tinvest.Client, error) {
		// The production gateway.
		return tinvest.NewClient(hc, "", token, r.log), nil
	}, nil
}

// startJobClient wires the job workers and River client and starts it, for the
// "all" and "worker" roles (both require the key, which the sync worker uses).
// cbr and moex use their default URLs with bounded clients.
func startJobClient(ctx context.Context, r *rt) (*river.Client[pgx.Tx], error) {
	mdStore := marketdata.NewStore(r.pool)
	instStore := instrument.NewStore(r.pool)
	opStore := operation.NewStore(r.pool)
	accStore := account.NewStore(r.pool)
	famStore := family.NewStore(r.pool)
	fxProvider := cbr.New(newCbrHTTPClient(), "")
	quoteProvider := moex.New(newMoexHTTPClient(), "", r.log)
	tinvestDeps, err := newTinvestDeps(r, instStore, opStore, accStore, marketdata.NewConverter(mdStore))
	if err != nil {
		return nil, err
	}
	// The registry writes through the importer's door
	// (operation.Service.ApplyImportDelta), so it gets a service, which judges
	// the journal a difference leaves.
	caStore := corporateaction.NewStore(r.pool)
	enqueuer := jobs.NewEnqueuer()
	// The rechecker uses the Enqueuer NewClient fills in below; a
	// materialization before the queue is up logs a refusal.
	caMaterializer := corporateaction.NewMaterializer(
		caStore, operation.NewService(opStore), instStore,
		tinvest.NewRechecker(tinvest.NewStore(r.pool), enqueuer, r.log), r.log)
	feed := yahoo.New(&http.Client{Timeout: referenceHTTPTimeout}, "")
	references := background.ReferenceSources{
		// FinEx's funds first; T-Capital's closed funds of blocked assets (#334).
		NAV: []marketdata.NAVProvider{
			finex.New(&http.Client{Timeout: referenceHTTPTimeout}, ""),
			tcapital.New(&http.Client{Timeout: referenceHTTPTimeout}, ""),
		},
		Foreign:   feed,
		Dividends: feed,
		Splits:    feed,
		Crypto:    coingecko.New(&http.Client{Timeout: referenceHTTPTimeout}, ""),
	}
	workers := background.NewWorkers(r.log, r.pool, mdStore, instStore, opStore, accStore, famStore,
		fxProvider, quoteProvider, references, tinvestDeps, caStore, caMaterializer, enqueuer)
	client, err := background.NewClient(r.pool, workers, enqueuer, r.log)
	if err != nil {
		return nil, err
	}
	if err := client.Start(ctx); err != nil {
		return nil, err
	}
	return client, nil
}

// stopJobClientTimeout bounds the graceful River stop before escalating to a
// forced cancel. It must stay longer than jobs.SoftStopTimeout, after which River
// cancels running jobs, or every full-length soft stop would escalate
// (TestTheJobQueueIsGivenLessTimeToStopThanTheProcessWaitsForIt).
const stopJobClientTimeout = 15 * time.Second

// stopJobClientForceTimeout bounds the forced StopAndCancel fallback.
const stopJobClientForceTimeout = 5 * time.Second

// stopJobClient stops the job client gracefully within stopJobClientTimeout,
// else escalates to StopAndCancel within stopJobClientForceTimeout, so shutdown
// never hangs.
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
	root.AddCommand(newAllCmd(), newAPICmd(), newWorkerCmd(), newMigrateCmd(), newVersionCmd(), newSeedCmd(), newResealCmd())
	return root
}

// signalCtx is the context long-running roles block on, cancelled by SIGINT,
// SIGTERM or the parent. The parent is the command's context (Background in
// production), so tests can stop a role by cancelling it.
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
			// Requests enqueue through the client this process works jobs with: one
			// queue, one uniqueness class.
			if err := mountModules(srv, r, client); err != nil {
				stopJobClient(client, r.log)
				return err
			}
			srv.Mount("/", web.Handler())

			// Sequenced shutdown: the HTTP server drains first, then the job queue's
			// bounded stop. Producers stopped fetching when ctx was cancelled; running
			// jobs keep jobs.SoftStopTimeout.
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
			// Insert-only: this role works no jobs and must not compete with the
			// worker. Never started or stopped (see jobs.NewInsertOnlyClient).
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
			// migrate runs before secrets are provisioned.
			r, err := setup(ctx, false, false)
			if err != nil {
				return err
			}
			defer r.close()
			return db.Migrate(ctx, r.pool)
		},
	}
}

// newResealCmd re-encrypts every broker token with BABKI_ENCRYPTION_KEY, reading
// those sealed with BABKI_ENCRYPTION_KEY_PREVIOUS, the last step of a key
// change.
func newResealCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reseal",
		Short: "Перешифровать токены брокеров текущим ключом",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signalCtx(cmd.Context())
			defer stop()
			// The key is required: resealing is nothing but using it.
			r, err := setup(ctx, false, true)
			if err != nil {
				return err
			}
			defer r.close()
			n, err := tinvest.NewStore(r.pool).ResealTokens(ctx, r.box)
			if err != nil {
				return err
			}
			r.log.Info("broker tokens resealed with the current key", "count", n)
			return nil
		},
	}
}

// setupCode is the one-time code first-run setup asks for, logged at start
// while there is no owner, so whoever reads the log, not whoever reaches the
// port first, becomes the owner. Made even if the check below fails.
func setupCode(r *rt, svc *family.Service) string {
	if chosen := strings.ToUpper(strings.TrimSpace(r.cfg.SetupCode)); chosen != "" {
		return chosen
	}
	code := family.NewSetupCode()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if needed, err := svc.SetupNeeded(ctx); err != nil || needed {
		r.log.Warn("first-run setup: enter this code on the setup screen", "setup_code", code)
	}
	return code
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
