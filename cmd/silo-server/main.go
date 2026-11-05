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

	"github.com/silopost/silo-server/internal/config"
	siloHTTP "github.com/silopost/silo-server/internal/http"
	"github.com/silopost/silo-server/internal/store"
	"github.com/silopost/silo-server/internal/version"
)

const shutdownTimeout = 15 * time.Second

// commands maps subcommand names to their implementations. New subcommands
// (migrate, user add, ...) are registered here.
var commands = map[string]func(ctx context.Context, args []string) error{
	"serve":   serve,
	"migrate": migrateCmd,
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
	fmt.Fprintln(os.Stderr, "usage: silo-server <command>\n\ncommands:\n  serve    run the HTTP server (--migrate=false skips migrations)\n  migrate  up, down --yes or version: manage the database schema\n  version  print the version")
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

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           siloHTTP.NewRouter(siloHTTP.Deps{Logger: logger}),
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
