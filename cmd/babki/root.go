package main

import (
	"context"
	"fmt"
	"log/slog"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/jobs"
	"babki.my/babki/internal/platform/version"
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
	account.NewHandler(account.NewStore(r.pool), famAuth, famSM).Mount(srv)
}

// startJobClient wires up the job workers and River client and starts it.
// Shared by the "all" and "worker" roles.
func startJobClient(ctx context.Context, r *rt) (*river.Client[pgx.Tx], error) {
	workers := jobs.NewWorkers(r.log, r.pool)
	client, err := jobs.NewClient(r.pool, workers, r.log)
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
			r, err := setup(ctx, true)
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
			r, err := setup(ctx, true)
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
			r, err := setup(ctx, true)
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
			r, err := setup(ctx, false)
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
