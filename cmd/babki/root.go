package main

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/jobs"
	"babki.my/babki/internal/platform/version"
	"babki.my/babki/web"
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "babki",
		Short:         "babki.my — учет семейных финансов (fair source)",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.AddCommand(newAllCmd(), newAPICmd(), newWorkerCmd(), newMigrateCmd(), newVersionCmd())
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

			workers := jobs.NewWorkers(r.log, r.pool)
			client, err := jobs.NewClient(r.pool, workers, r.log)
			if err != nil {
				return err
			}
			if err := client.Start(ctx); err != nil {
				return err
			}
			srv := httpserver.New(r.log, r.pool)
			srv.Mount("/", web.Handler())

			g, gctx := errgroup.WithContext(ctx)
			g.Go(func() error { return srv.Run(gctx, r.cfg.HTTPAddr) })
			g.Go(func() error {
				<-gctx.Done()
				return client.Stop(context.Background())
			})
			return g.Wait()
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

			workers := jobs.NewWorkers(r.log, r.pool)
			client, err := jobs.NewClient(r.pool, workers, r.log)
			if err != nil {
				return err
			}
			if err := client.Start(ctx); err != nil {
				return err
			}
			<-ctx.Done()
			return client.Stop(context.Background())
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
