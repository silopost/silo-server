package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/silopost/silo-server/internal/config"
	"github.com/silopost/silo-server/internal/store"
)

// migrateCmd implements `silo-server migrate up|down|version`. It needs only
// SILO_DATABASE_URL.
func migrateCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "confirm `down`, which drops every table")
	if len(args) == 0 {
		return errors.New("usage: silo-server migrate up|down|version [--yes]")
	}
	action := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if cfg.DatabaseURL == "" {
		return errors.New("SILO_DATABASE_URL is required")
	}

	switch action {
	case "up":
		return store.MigrateUp(ctx, cfg.DatabaseURL)
	case "down":
		if !*yes {
			return errors.New("migrate down drops all data; pass --yes to confirm")
		}
		return store.MigrateDown(ctx, cfg.DatabaseURL)
	case "version":
		v, dirty, err := store.MigrateVersion(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		fmt.Printf("version=%d dirty=%t\n", v, dirty)
		return nil
	default:
		return fmt.Errorf("unknown migrate action %q (want up, down or version)", action)
	}
}
