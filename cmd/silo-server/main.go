// Command silo-server is the SiloPost backend. Usage: silo-server <command> [args].
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/silopost/silo-server/internal/accounts"
	"github.com/silopost/silo-server/internal/auth"
	"github.com/silopost/silo-server/internal/blob"
	"github.com/silopost/silo-server/internal/config"
	siloHTTP "github.com/silopost/silo-server/internal/http"
	"github.com/silopost/silo-server/internal/http/api"
	"github.com/silopost/silo-server/internal/secrets"
	"github.com/silopost/silo-server/internal/store"
	"github.com/silopost/silo-server/internal/version"
	"github.com/silopost/silo-server/internal/worker"
)

const shutdownTimeout = 15 * time.Second

// commands maps subcommand names to their implementations. New subcommands
// (migrate, user add, ...) are registered here.
var commands = map[string]func(ctx context.Context, args []string) error{
	"serve":          serve,
	"migrate":        migrateCmd,
	"rotate-secrets": rotateSecretsCmd,
	"reconcile":      reconcileCmd,
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	name := os.Args[1]
	switch name {
	case "version", "-v", "--version":
		fmt.Println(version.Version)
		return
	case "help", "-h", "--help":
		usage()
		return
	}
	cmd, ok := commands[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", name)
		usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cmd(ctx, os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: silo-server <command>\n\ncommands:\n  serve    run the HTTP server (--migrate=false skips migrations)\n  migrate  up, down --yes or version: manage the database schema\n  rotate-secrets  re-encrypt account secrets after a key change\n  version  print the version")
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	migrateOnStart := fs.Bool("migrate", true, "apply pending database migrations before starting")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := cfg.ValidateServe(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	logger, err := newLogger(cfg)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	if *migrateOnStart {
		logger.Info("applying database migrations")
		if err := store.MigrateUp(ctx, cfg.DatabaseURL); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}

	pool, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	blobs, err := blob.NewFSStore(cfg.BlobFSDir)
	if err != nil {
		return err
	}

	current, previous, err := cfg.SecretKeys()
	if err != nil {
		return err
	}
	ring, err := secrets.NewKeyring(current, previous)
	if err != nil {
		return err
	}
	authSvc := auth.New(auth.Options{Pool: pool, Keys: ring, Config: cfg, Logger: logger})
	accountSvc := accounts.New(accounts.Options{Pool: pool, Keys: ring, Config: cfg, Logger: logger})

	// Job handlers are registered by the features that enqueue them.
	jobs := worker.New(worker.Options{Pool: pool, Logger: logger, Concurrency: cfg.WorkerConcurrency})
	// The account sync engine is not built yet: acknowledge the start requests
	// so they do not pile up as dead jobs. The engine replaces these handlers.
	for _, kind := range []string{auth.JobAccountInitialSync, auth.JobAccountReconnect} {
		jobs.Register(kind, func(ctx context.Context, j worker.Job) error {
			logger.DebugContext(ctx, "account start requested; sync engine not available yet", "kind", j.Kind)
			return nil
		})
	}
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); _ = jobs.Run(workerCtx) }()
	defer func() { stopWorker(); <-workerDone }()

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: siloHTTP.NewRouter(siloHTTP.Deps{
			Logger: logger,
			DB:     pool,
			Blobs:  blobs,
			API: api.Options{
				ServerVersion:      version.Version,
				AuthMode:           cfg.AuthMode,
				AllowCustomServers: cfg.AllowCustomServers,
				Auth:               authSvc,
				Accounts:           accountSvc,
				PublicURL:          cfg.PublicURL,
				Logger:             logger,
			},
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	logger.Info("silo-server started", "version", version.Version, "addr", cfg.HTTPAddr)

	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info("stopped")
	return nil
}

func newLogger(cfg config.Config) (*slog.Logger, error) {
	level, err := cfg.SlogLevel()
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts)), nil
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts)), nil
}
